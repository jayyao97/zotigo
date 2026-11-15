package workerbase

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type CommandCursor struct {
	Offset   int64  `json:"offset"`
	Sequence uint64 `json:"sequence"`
}

func FetchCommands(ctx context.Context, client *http.Client, daemonURL string, sessionID string, cursor CommandCursor) (wire.CommandsPage, error) {
	endpoint := strings.TrimRight(daemonURL, "/") + "/internal/sessions/" + url.PathEscape(sessionID) + "/commands?offset=" + strconv.FormatInt(cursor.Offset, 10) + "&limit=" + strconv.Itoa(wire.MaxCommandsLimit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return wire.CommandsPage{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return wire.CommandsPage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return wire.CommandsPage{}, fmt.Errorf("fetch worker commands failed: %s", resp.Status)
	}
	var body wire.CommandsPage
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&body); err != nil {
		return wire.CommandsPage{}, err
	}
	return body, nil
}

func LoadCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string) (CommandCursor, error) {
	data, err := os.ReadFile(commandCursorPath(sessionID))
	if os.IsNotExist(err) {
		return CommandCursor{}, nil
	}
	if err != nil {
		return CommandCursor{}, err
	}
	var cursor CommandCursor
	if err := sonic.Unmarshal(data, &cursor); err == nil {
		return validateCommandCursor(ctx, store, sessionID, cursor)
	}
	sequence, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return RecoverCommandCursor(ctx, store, sessionID)
	}
	return validateCommandCursor(ctx, store, sessionID, CommandCursor{Sequence: sequence})
}

func validateCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string, cursor CommandCursor) (CommandCursor, error) {
	if cursor.Offset < 0 {
		return RecoverCommandCursor(ctx, store, sessionID)
	}
	if store == nil {
		return cursor, nil
	}
	items, _, err := store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return CommandCursor{}, fmt.Errorf("validate worker command cursor: %w", err)
	}
	safeSequence := RecoverAppliedCommandSequence(items)
	if cursor.Sequence > safeSequence {
		return CommandCursor{Sequence: safeSequence}, nil
	}
	if cursor.Offset > 0 {
		if cursor.Sequence < LatestCommandSequence(items) {
			return CommandCursor{Sequence: cursor.Sequence}, nil
		}
		type offsetStore interface {
			ListDisplayItemsFromOffset(ctx context.Context, id string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
		}
		if offsetReader, ok := store.(offsetStore); ok {
			if _, _, _, err := offsetReader.ListDisplayItemsFromOffset(ctx, sessionID, cursor.Offset, 1); err != nil {
				return CommandCursor{Sequence: safeSequence}, nil
			}
		}
	}
	return cursor, nil
}

func LatestCommandSequence(items []zotigosession.DisplayItem) uint64 {
	var latest uint64
	for _, item := range items {
		if isDurableCommandItem(item) {
			latest = item.Sequence
		}
	}
	return latest
}

func isDurableCommandItem(item zotigosession.DisplayItem) bool {
	return item.Command != nil && item.Command.Type != "" && (item.Command.Type != wire.CommandSteering || item.Type == zotigosession.DisplayItemSessionCommand)
}

func RecoverCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string) (CommandCursor, error) {
	if store == nil {
		return CommandCursor{}, nil
	}
	items, _, err := store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return CommandCursor{}, fmt.Errorf("recover worker command cursor: %w", err)
	}
	return CommandCursor{Sequence: RecoverAppliedCommandSequence(items)}, nil
}

func RecoverAppliedCommandSequence(items []zotigosession.DisplayItem) uint64 {
	applied := AppliedCommandSequences(items)
	var cursor uint64
	for _, item := range items {
		if !isDurableCommandItem(item) {
			continue
		}
		if !applied[item.Sequence] {
			return cursor
		}
		cursor = item.Sequence
	}
	return cursor
}

func AppliedCommandSequences(items []zotigosession.DisplayItem) map[uint64]bool {
	safe := make(map[uint64]bool)
	pendingMessages := make([]uint64, 0)
	pendingByTurn := make(map[string][]uint64)
	pendingSteering := make(map[string]uint64)
	pendingProfiles := make(map[string]uint64)
	pendingApprovalPolicies := make(map[string]uint64)

	for _, item := range items {
		if isDurableCommandItem(item) {
			switch item.Command.Type {
			case wire.CommandMessage:
				pendingMessages = append(pendingMessages, item.Sequence)
			case wire.CommandPause:
				if item.Command.TurnID != "" {
					pendingByTurn[item.Command.TurnID] = append(pendingByTurn[item.Command.TurnID], item.Sequence)
				}
			case wire.CommandSteering:
				pendingSteering[item.ID] = item.Sequence
			case wire.CommandProfile:
				pendingProfiles[item.ID] = item.Sequence
			case wire.CommandApprovalPolicy:
				pendingApprovalPolicies[item.ID] = item.Sequence
			default:
				safe[item.Sequence] = true
			}
		}
		switch item.Type {
		case zotigosession.DisplayItemSteeringMessage:
			if sequence, ok := pendingSteering[item.ID]; ok {
				safe[sequence] = true
				delete(pendingSteering, item.ID)
			}
		case zotigosession.DisplayItemTurnStarted:
			if len(pendingMessages) > 0 {
				safe[pendingMessages[0]] = true
				pendingMessages = pendingMessages[1:]
			}
		case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			if item.Turn == nil || item.Turn.ID == "" {
				continue
			}
			for _, seq := range pendingByTurn[item.Turn.ID] {
				safe[seq] = true
			}
			delete(pendingByTurn, item.Turn.ID)
		case zotigosession.DisplayItemProfileChanged, zotigosession.DisplayItemProfileFailed:
			if item.Profile != nil {
				if seq, ok := pendingProfiles[item.Profile.CommandID]; ok {
					safe[seq] = true
					delete(pendingProfiles, item.Profile.CommandID)
				} else if item.Type == zotigosession.DisplayItemProfileChanged && item.Profile.CommandID == "" {
					for commandID, pendingSeq := range pendingProfiles {
						safe[pendingSeq] = true
						delete(pendingProfiles, commandID)
					}
				}
			}
		case zotigosession.DisplayItemApprovalPolicyChanged:
			if item.ApprovalPolicy != nil {
				if seq, ok := pendingApprovalPolicies[item.ApprovalPolicy.CommandID]; ok {
					safe[seq] = true
					delete(pendingApprovalPolicies, item.ApprovalPolicy.CommandID)
				}
			}
		}
	}

	return safe
}

func SaveCommandCursor(sessionID string, cursor CommandCursor) error {
	path := commandCursorPath(sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := sonic.Marshal(cursor)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// CompactedTranscriptDir is where native workers archive full transcripts
// before compaction. It follows the process state root (~/.zotigo, or
// --state-dir), not a handler's custom session store, so the fork API finds
// archives where workers wrote them.
func CompactedTranscriptDir() string {
	root, err := config.StateDir()
	if err != nil {
		root = filepath.Join(os.TempDir(), "zotigo")
	}
	return filepath.Join(root, "sessions", "compacted")
}

func commandCursorPath(sessionID string) string {
	root, err := config.StateDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "zotigo-"+sessionID+".cursor")
	}
	return filepath.Join(root, "sessions", sessionID+".worker.cursor")
}
