package zotigod

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
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type workerCommandCursor struct {
	Offset   int64  `json:"offset"`
	Sequence uint64 `json:"sequence"`
}

func replayWorkerCommands(ctx context.Context, client *http.Client, daemonURL string, sessionID string, runtime *workerRuntime, cursor workerCommandCursor) (workerCommandCursor, error) {
	type pendingProfile struct {
		sequence   uint64
		completion <-chan error
	}
	pendingProfiles := make([]pendingProfile, 0)
	appliedCommands, err := loadAppliedWorkerCommandSequences(ctx, runtime)
	if err != nil {
		return cursor, err
	}
	flushProfiles := func() error {
		for _, pending := range pendingProfiles {
			select {
			case err := <-pending.completion:
				if err != nil {
					return err
				}
				cursor.Sequence = pending.sequence
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		pendingProfiles = pendingProfiles[:0]
		return nil
	}

	for {
		previousOffset := cursor.Offset
		commands, err := fetchWorkerCommands(ctx, client, daemonURL, sessionID, cursor)
		if err != nil {
			return cursor, err
		}
		for _, command := range commands.Commands {
			if command.Sequence <= cursor.Sequence {
				continue
			}
			// Recovery can rewind past an unacknowledged command. Later commands
			// with durable execution evidence must not run again across that gap.
			if appliedCommands[command.Sequence] {
				if err := flushProfiles(); err != nil {
					return cursor, err
				}
				cursor.Sequence = command.Sequence
				continue
			}
			if command.Type != sessionCommandProfile {
				if err := flushProfiles(); err != nil {
					return cursor, err
				}
			}
			completion, err := runtime.handleCommand(ctx, command)
			if err != nil {
				return cursor, err
			}
			if completion != nil {
				pendingProfiles = append(pendingProfiles, pendingProfile{sequence: command.Sequence, completion: completion})
				continue
			}
			cursor.Sequence = command.Sequence
		}
		if err := flushProfiles(); err != nil {
			return cursor, err
		}
		cursor.Offset = commands.NextOffset
		if err := saveWorkerCommandCursor(sessionID, cursor); err != nil {
			return cursor, err
		}
		if commands.NextOffset == previousOffset || len(commands.Commands) < maxCommandsLimit {
			return cursor, nil
		}
	}
}

func loadAppliedWorkerCommandSequences(ctx context.Context, runtime *workerRuntime) (map[uint64]bool, error) {
	if runtime == nil || runtime.display == nil || runtime.display.items == nil {
		return nil, nil
	}
	items, _, err := runtime.display.items.LoadItems(ctx, runtime.sessionID)
	if err != nil {
		return nil, fmt.Errorf("load worker command completions: %w", err)
	}
	return appliedWorkerCommandSequences(items), nil
}

func fetchWorkerCommands(ctx context.Context, client *http.Client, daemonURL string, sessionID string, cursor workerCommandCursor) (commandsResponse, error) {
	endpoint := strings.TrimRight(daemonURL, "/") + "/internal/sessions/" + url.PathEscape(sessionID) + "/commands?offset=" + strconv.FormatInt(cursor.Offset, 10) + "&limit=" + strconv.Itoa(maxCommandsLimit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return commandsResponse{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return commandsResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return commandsResponse{}, fmt.Errorf("fetch worker commands failed: %s", resp.Status)
	}
	var body commandsResponse
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&body); err != nil {
		return commandsResponse{}, err
	}
	return body, nil
}

func loadWorkerCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string) (workerCommandCursor, error) {
	data, err := os.ReadFile(workerCommandCursorPath(sessionID))
	if os.IsNotExist(err) {
		return workerCommandCursor{}, nil
	}
	if err != nil {
		return workerCommandCursor{}, err
	}
	var cursor workerCommandCursor
	if err := sonic.Unmarshal(data, &cursor); err == nil {
		return validateWorkerCommandCursor(ctx, store, sessionID, cursor)
	}
	sequence, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return recoverWorkerCommandCursor(ctx, store, sessionID)
	}
	return validateWorkerCommandCursor(ctx, store, sessionID, workerCommandCursor{Sequence: sequence})
}

func validateWorkerCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string, cursor workerCommandCursor) (workerCommandCursor, error) {
	if cursor.Offset < 0 {
		return recoverWorkerCommandCursor(ctx, store, sessionID)
	}
	if store == nil {
		return cursor, nil
	}
	items, _, err := store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return workerCommandCursor{}, fmt.Errorf("validate worker command cursor: %w", err)
	}
	safeSequence := recoverAppliedCommandSequence(items)
	if cursor.Sequence > safeSequence {
		return workerCommandCursor{Sequence: safeSequence}, nil
	}
	if cursor.Offset > 0 {
		if cursor.Sequence < latestCommandSequence(items) {
			return workerCommandCursor{Sequence: cursor.Sequence}, nil
		}
		type offsetStore interface {
			ListDisplayItemsFromOffset(ctx context.Context, id string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
		}
		if offsetReader, ok := store.(offsetStore); ok {
			if _, _, _, err := offsetReader.ListDisplayItemsFromOffset(ctx, sessionID, cursor.Offset, 1); err != nil {
				return workerCommandCursor{Sequence: safeSequence}, nil
			}
		}
	}
	return cursor, nil
}

func latestCommandSequence(items []zotigosession.DisplayItem) uint64 {
	var latest uint64
	for _, item := range items {
		if isDurableCommandItem(item) {
			latest = item.Sequence
		}
	}
	return latest
}

func isDurableCommandItem(item zotigosession.DisplayItem) bool {
	return item.Command != nil && item.Command.Type != "" && (item.Command.Type != sessionCommandSteering || item.Type == zotigosession.DisplayItemSessionCommand)
}

func recoverWorkerCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string) (workerCommandCursor, error) {
	if store == nil {
		return workerCommandCursor{}, nil
	}
	items, _, err := store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return workerCommandCursor{}, fmt.Errorf("recover worker command cursor: %w", err)
	}
	return workerCommandCursor{Sequence: recoverAppliedCommandSequence(items)}, nil
}

func recoverAppliedCommandSequence(items []zotigosession.DisplayItem) uint64 {
	applied := appliedWorkerCommandSequences(items)
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

func appliedWorkerCommandSequences(items []zotigosession.DisplayItem) map[uint64]bool {
	safe := make(map[uint64]bool)
	pendingMessages := make([]uint64, 0)
	pendingByTurn := make(map[string][]uint64)
	pendingSteering := make(map[string]uint64)
	pendingProfiles := make(map[string]uint64)
	pendingApprovalPolicies := make(map[string]uint64)

	for _, item := range items {
		if isDurableCommandItem(item) {
			switch item.Command.Type {
			case sessionCommandMessage:
				pendingMessages = append(pendingMessages, item.Sequence)
			case sessionCommandPause:
				if item.Command.TurnID != "" {
					pendingByTurn[item.Command.TurnID] = append(pendingByTurn[item.Command.TurnID], item.Sequence)
				}
			case sessionCommandSteering:
				pendingSteering[item.ID] = item.Sequence
			case sessionCommandProfile:
				pendingProfiles[item.ID] = item.Sequence
			case sessionCommandApprovalPolicy:
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

func saveWorkerCommandCursor(sessionID string, cursor workerCommandCursor) error {
	path := workerCommandCursorPath(sessionID)
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

// compactedTranscriptDir is where native workers archive full transcripts
// before compaction. It follows the process state root (~/.zotigo, or
// --state-dir), not a handler's custom session store, so the fork API finds
// archives where workers wrote them.
func compactedTranscriptDir() string {
	root, err := config.StateDir()
	if err != nil {
		root = filepath.Join(os.TempDir(), "zotigo")
	}
	return filepath.Join(root, "sessions", "compacted")
}

func workerCommandCursorPath(sessionID string) string {
	root, err := config.StateDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "zotigo-"+sessionID+".cursor")
	}
	return filepath.Join(root, "sessions", sessionID+".worker.cursor")
}
