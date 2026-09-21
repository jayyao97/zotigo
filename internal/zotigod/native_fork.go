package zotigod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	session "github.com/jayyao97/zotigo/core/session"
)

const nativeArchiveBudget = 64 << 20

var nativeTranscriptName = regexp.MustCompile(`^transcript_[0-9]+\.jsonl$`)

func nativeForkSnapshot(ctx context.Context, source *session.Session, turnID, archiveDir string) (agent.Snapshot, error) {
	point, ok := source.ForkPoints[turnID]
	if !ok || point.HistoryLength < 1 || point.HistoryDigest == "" {
		return agent.Snapshot{}, errors.New("exact native fork checkpoint unavailable for this legacy turn")
	}
	match := func(history []protocol.Message) (agent.Snapshot, bool) {
		if len(history) < point.HistoryLength {
			return agent.Snapshot{}, false
		}
		snapshot := agent.Snapshot{State: agent.StateIdle, CreatedAt: time.Now().UTC(), History: history[:point.HistoryLength]}
		if session.SnapshotDigest(snapshot) != point.HistoryDigest {
			return agent.Snapshot{}, false
		}
		// No pending actions, safety decisions or caches cross the fork boundary.
		snapshot.History = append([]protocol.Message(nil), snapshot.History...)
		return snapshot, true
	}
	history := source.AgentSnapshot.History
	seen := make(map[string]bool)
	remaining := int64(nativeArchiveBudget)
	remainingMessages := 100000
	for {
		if err := ctx.Err(); err != nil {
			return agent.Snapshot{}, err
		}
		if snapshot, ok := match(history); ok {
			return snapshot, nil
		}
		index, path := nativeSummaryTranscript(history)
		if index < 0 {
			return agent.Snapshot{}, errors.New("native history was compacted or replaced; available archives do not match the fork checkpoint")
		}
		if seen[path] || len(seen) >= 64 {
			return agent.Snapshot{}, errors.New("native fork archive chain is cyclic or exceeds 64 files")
		}
		seen[path] = true
		archived, err := readNativeTranscript(ctx, archiveDir, path, &remaining, &remainingMessages)
		if err != nil {
			return agent.Snapshot{}, fmt.Errorf("native fork archive unavailable: %w", err)
		}
		// The archive itself can contain the complete target prefix. In particular,
		// do not let newly rebased contextual messages contaminate this candidate.
		if snapshot, ok := match(archived); ok {
			return snapshot, nil
		}
		expanded := make([]protocol.Message, 0, len(history)-1+len(archived))
		expanded = append(expanded, history[:index]...)
		expanded = append(expanded, archived...)
		expanded = append(expanded, history[index+1:]...)
		if snapshot, ok := match(expanded); ok {
			return snapshot, nil
		}
		// rebaseUserContext inserts fresh contextual messages immediately after
		// the summary. They did not exist at a pre-compaction checkpoint. Try
		// removing only that block; archived contextual messages stay intact.
		tail := index + 1
		for tail < len(history) && history[tail].Role == protocol.RoleUser && history[tail].Contextual {
			tail++
		}
		if tail > index+1 {
			expanded = expanded[:index+len(archived)]
			expanded = append(expanded, history[tail:]...)
		}
		history = expanded
	}
}

// Only compressor-generated summaries have this zero-timestamp shape. A user
// pasting a transcript path must not turn fork into an arbitrary file reader.
func nativeSummaryTranscript(history []protocol.Message) (int, string) {
	const reference = "\n\nIf you need specific details from before compaction, read the full transcript at: "
	for index, message := range history {
		if message.Role != protocol.RoleUser || message.ID != "" || !message.CreatedAt.IsZero() || message.Contextual || message.Metadata != nil || len(message.Content) != 1 {
			continue
		}
		part := message.Content[0]
		if part.Type != protocol.ContentTypeText || !strings.HasPrefix(part.Text, "[Previous conversation summary]\n") {
			continue
		}
		if offset := strings.LastIndex(part.Text, reference); offset >= 0 {
			return index, part.Text[offset+len(reference):]
		}
	}
	return -1, ""
}

func readNativeTranscript(ctx context.Context, dir, path string, remaining *int64, remainingMessages *int) ([]protocol.Message, error) {
	name := filepath.Base(path)
	if !filepath.IsAbs(path) || filepath.Clean(filepath.Dir(path)) != filepath.Clean(dir) || !nativeTranscriptName.MatchString(name) {
		return nil, errors.New("invalid compacted transcript reference")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("compacted transcript directory unavailable")
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("compacted transcript missing or not a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("cannot open compacted transcript")
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(io.LimitReader(file, *remaining+1))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var messages []protocol.Message
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		*remaining -= int64(len(scanner.Bytes()) + 1)
		if *remaining < 0 {
			return nil, errors.New("compacted transcripts exceed 64 MiB recovery budget")
		}
		*remainingMessages--
		if *remainingMessages < 0 {
			return nil, errors.New("compacted transcripts exceed 100000 message recovery budget")
		}
		var message protocol.Message
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.UseNumber()
		if err := decoder.Decode(&message); err != nil {
			return nil, errors.New("invalid compacted transcript record")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, errors.New("invalid compacted transcript trailing data")
		}
		messages = append(messages, message)
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("cannot read complete compacted transcript")
	}
	if len(messages) == 0 {
		return nil, errors.New("empty compacted transcript")
	}
	return messages, nil
}
