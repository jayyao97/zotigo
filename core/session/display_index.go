package session

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrDisplayIndexPending means history is available but its derived index is
// rebuilding. Callers must not silently return an incomplete result.
var ErrDisplayIndexPending = errors.New("conversation index is rebuilding; retry shortly")

type DisplayTimeWindow struct{ Since, Until *time.Time }

// InvalidateDisplayIndex must precede replacing/truncating a display log. Normal
// display writes are append-only; importers must not reuse old file offsets.
func (s *FileStore) InvalidateDisplayIndex(ctx context.Context, id string) error {
	_, err := s.index.db.ExecContext(ctx, `DELETE FROM display_index_files WHERE session_id=?`, id)
	return err
}

// RebuildDisplayIndex catches up derived offsets without loading complete logs
// into memory. Run during startup/background maintenance, never in a read query.
func (s *FileStore) RebuildDisplayIndex(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(s.rootDir, "sessions"))
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".display.jsonl") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".display.jsonl")
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			s.mu.Lock()
			displayLogAppendMu.Lock()
			unlock, lockErr := s.lockDisplayLogAppendLocked(ctx, id)
			complete := false
			if lockErr == nil {
				complete, lockErr = s.syncDisplayIndexBatch(ctx, id)
				unlock()
			}
			displayLogAppendMu.Unlock()
			s.mu.Unlock()
			if lockErr != nil {
				if !errors.Is(lockErr, os.ErrNotExist) {
					failures = append(failures, fmt.Errorf("index session %s: %w", id, lockErr))
				}
				break
			}
			if complete {
				break
			}
		}
	}
	return errors.Join(failures...)
}

// Called with the display writer lock. JSONL remains authoritative. A failed
// index transaction leaves its old size marker, so reads fail closed until repair.
func (s *FileStore) syncDisplayIndex(ctx context.Context, id string) error {
	_, err := s.syncDisplayIndexBatch(ctx, id)
	return err
}

// Bound each transaction and lock hold to 256 records. Old logs are filled by
// repeated maintenance batches; a normal append never triggers a full backfill.
func (s *FileStore) syncDisplayIndexBatch(ctx context.Context, id string) (bool, error) {
	file, err := os.Open(s.displayLogPath(id))
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	var offset, mtime, observed int64
	err = s.index.db.QueryRowContext(ctx, `SELECT size,mtime,observed_size FROM display_index_files WHERE session_id=?`, id).Scan(&offset, &mtime, &observed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && observed == info.Size() && mtime == info.ModTime().UnixNano() {
		return true, nil
	}
	missing := errors.Is(err, sql.ErrNoRows)
	tx, err := s.index.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Appends preserve offsets. Replacements must invalidate the index first.
	if offset >= info.Size() || missing {
		offset = 0
		if _, err = tx.ExecContext(ctx, `DELETE FROM display_items WHERE session_id=?`, id); err != nil {
			return false, err
		}
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	reader := bufio.NewReader(file)
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO display_items(session_id,sequence,message_at,dialogue,offset,length,content_kind,item_id,search_text) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = stmt.Close() }()
	complete := false
	for count := 0; count < 256; count++ {
		line, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) {
			complete = true
			break
		}
		if readErr != nil {
			return false, readErr
		}
		var item DisplayItem
		if len(strings.TrimSpace(string(line))) == 0 {
			offset += int64(len(line))
			continue
		}
		if err = json.Unmarshal(line, &item); err != nil {
			return false, err
		}
		dialogue := item.Type == DisplayItemUserMessage || item.Type == DisplayItemAssistantMessage || item.Type == DisplayItemSteeringMessage
		if _, err = stmt.ExecContext(ctx, id, item.Sequence, item.CreatedAt.UnixNano(), dialogue, offset, len(line), item.ContentKind(), item.ID, displaySearchText(item)); err != nil {
			return false, err
		}
		offset += int64(len(line))
	}
	observedSize := int64(-1)
	if complete || offset == info.Size() {
		observedSize = info.Size()
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO display_index_files(session_id,size,mtime,observed_size) VALUES(?,?,?,?)`, id, offset, info.ModTime().UnixNano(), observedSize); err != nil {
		return false, err
	}
	return complete, tx.Commit()
}

func (s *FileStore) displayIndexReady(ctx context.Context, id string) error {
	return s.displayIndexReadyWithDB(ctx, id, s.index.db)
}

func (s *FileStore) displayIndexReadyWithDB(ctx context.Context, id string, db *sql.DB) error {
	info, err := os.Stat(s.displayLogPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var size, mtime int64
	err = db.QueryRowContext(ctx, `SELECT observed_size,mtime FROM display_index_files WHERE session_id=?`, id).Scan(&size, &mtime)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (size != info.Size() || mtime != info.ModTime().UnixNano()) {
		return ErrDisplayIndexPending
	}
	return err
}

func displayTimeSQL(window DisplayTimeWindow) (string, []any) {
	where := ""
	args := []any{}
	if window.Since != nil {
		where += " AND message_at>=?"
		args = append(args, window.Since.UnixNano())
	}
	if window.Until != nil {
		where += " AND message_at<?"
		args = append(args, window.Until.UnixNano())
	}
	return where, args
}

// ReadDisplayPage reads only the selected JSONL records, not the full history.
func (s *FileStore) ReadDisplayPage(ctx context.Context, id string, query DisplayPageQuery, window DisplayTimeWindow) (DisplayPage, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	page := DisplayPage{Items: []DisplayItem{}}
	if _, err := os.Stat(s.sessionPath(id)); errors.Is(err, os.ErrNotExist) {
		return page, false, nil
	} else if err != nil {
		return page, false, err
	}
	if query.Limit < 1 || query.Limit > 1000 {
		return page, true, fmt.Errorf("invalid page limit")
	}
	unlock, err := s.lockDisplayLogAppendLocked(ctx, id)
	if err != nil {
		return page, true, err
	}
	defer unlock()
	if err := s.displayIndexReady(ctx, id); err != nil {
		return page, true, err
	}
	where, args := displayTimeSQL(window)
	base := "session_id=?" + where
	args = append([]any{id}, args...)
	if query.ContentKind != "" {
		base += " AND content_kind=?"
		args = append(args, query.ContentKind)
	}
	var first, last int64
	for _, bound := range []struct {
		order  string
		target *int64
	}{{"ASC", &first}, {"DESC", &last}} {
		err := s.index.db.QueryRowContext(ctx, `SELECT sequence FROM display_items WHERE `+base+` ORDER BY sequence `+bound.order+` LIMIT 1`, args...).Scan(bound.target)
		if errors.Is(err, sql.ErrNoRows) {
			return page, true, nil
		}
		if err != nil {
			return page, true, err
		}
	}
	order := " DESC"
	if query.HasAfter {
		base += " AND sequence>?"
		args = append(args, query.After)
		order = " ASC"
	}
	if query.HasBefore {
		base += " AND sequence<?"
		args = append(args, query.Before)
	}
	args = append(args, query.Limit)
	rows, err := s.index.db.QueryContext(ctx, `SELECT offset,length FROM display_items WHERE `+base+` ORDER BY sequence`+order+` LIMIT ?`, args...)
	if err != nil {
		return page, true, err
	}
	type span struct {
		offset int64
		length int
	}
	var spans []span
	for rows.Next() {
		var value span
		if err = rows.Scan(&value.offset, &value.length); err != nil {
			_ = rows.Close()
			return page, true, err
		}
		spans = append(spans, value)
	}
	err = rows.Err()
	err = errors.Join(err, rows.Close())
	if err != nil {
		return page, true, err
	}
	if len(spans) == 0 {
		return page, true, nil
	}
	file, err := os.Open(s.displayLogPath(id))
	if err != nil {
		return page, true, err
	}
	defer func() { _ = file.Close() }()
	for _, value := range spans {
		data := make([]byte, value.length)
		if _, err = file.ReadAt(data, value.offset); err != nil {
			return page, true, err
		}
		var item DisplayItem
		if err = json.Unmarshal(data, &item); err != nil {
			return page, true, err
		}
		page.Items = append(page.Items, item)
	}
	// A concurrent writer in another process may invalidate the selected offsets.
	if err = s.displayIndexReady(ctx, id); err != nil {
		return DisplayPage{}, true, err
	}
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].Sequence < page.Items[j].Sequence })
	if int64(page.Items[0].Sequence) > first {
		page.PrevCursor = strconv.FormatUint(page.Items[0].Sequence, 10)
	}
	if int64(page.Items[len(page.Items)-1].Sequence) < last {
		page.NextCursor = strconv.FormatUint(page.Items[len(page.Items)-1].Sequence, 10)
	}
	page.HasMore = page.PrevCursor != "" || page.NextCursor != ""
	return page, true, nil
}
