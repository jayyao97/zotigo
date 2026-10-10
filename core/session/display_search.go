package session

import (
	"context"
	"fmt"
	"os"
	"strings"
)

type DisplaySearchHit struct {
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
}
type DisplaySearchResult struct {
	Hits      []DisplaySearchHit `json:"hits"`
	Truncated bool               `json:"truncated"`
}

// Store only conversational prose; large tool payloads and reasoning do not
// belong in the find index. The display log remains the source of truth.
func displaySearchText(item DisplayItem) string {
	if item.ContentKind() != DisplayContentConversation {
		return ""
	}
	var parts []string
	for _, part := range item.Content {
		if part.Type == "text" {
			parts = append(parts, part.Text)
		}
	}
	if len(parts) == 0 && item.Type == DisplayItemSteeringMessage && item.Command != nil {
		parts = append(parts, item.Command.Text)
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

// SearchDisplayMessages never opens the JSONL log or builds the index on demand.
// Literal substring matching is scoped by the (session, kind, sequence) index;
// it deliberately does not claim a B-tree accelerates the substring predicate.
func (s *FileStore) SearchDisplayMessages(ctx context.Context, id, query string, limit int) (DisplaySearchResult, error) {
	result := DisplaySearchResult{Hits: []DisplaySearchHit{}}
	if strings.TrimSpace(query) == "" || len([]rune(query)) > 256 || limit < 1 || limit > 250 {
		return result, fmt.Errorf("invalid search query or limit")
	}
	if _, err := os.Stat(s.sessionPath(id)); err != nil {
		return result, err
	}
	if err := s.displayIndexReadyWithDB(ctx, id, s.index.searchDB); err != nil {
		return result, err
	}
	rows, err := s.index.searchDB.QueryContext(ctx, `SELECT item_id,sequence FROM display_items WHERE session_id=? AND content_kind='conversation' AND instr(search_text,?)>0 ORDER BY sequence DESC LIMIT ?`, id, strings.ToLower(query), limit+1)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hit DisplaySearchHit
		if err = rows.Scan(&hit.ID, &hit.Sequence); err != nil {
			return result, err
		}
		result.Hits = append(result.Hits, hit)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if len(result.Hits) > limit {
		result.Truncated = true
		result.Hits = result.Hits[:limit]
	}
	return result, nil
}

func (s *FileStore) DisplaySequence(ctx context.Context, id, messageID string) (uint64, error) {
	if err := s.displayIndexReadyWithDB(ctx, id, s.index.searchDB); err != nil {
		return 0, err
	}
	var sequence uint64
	err := s.index.searchDB.QueryRowContext(ctx, `SELECT sequence FROM display_items WHERE session_id=? AND item_id=? LIMIT 1`, id, messageID).Scan(&sequence)
	return sequence, err
}
