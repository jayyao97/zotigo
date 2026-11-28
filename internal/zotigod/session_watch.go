package zotigod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

// Imported turns can arrive out of timestamp order. Narrow the indexed time
// window as candidates are found, keeping sequence order only as a tiebreaker.
func (h *handler) submittedWatchCommand(ctx context.Context, id string, earliest bool) (string, error) {
	bestID, bestAt, err := h.catalog.QueuedSubmissionBoundary(ctx, id, earliest)
	if err != nil {
		return "", err
	}
	hasQueue := bestID != ""
	read, indexed := h.store.(*session.FileStore)
	// Steering admission is an event until the native worker applies it.
	query := session.DisplayPageQuery{Limit: 32, HasAfter: earliest, ExcludeTools: true}
	for {
		var items []session.DisplayItem
		if indexed {
			var window session.DisplayTimeWindow
			if bestID != "" && !bestAt.IsZero() {
				if earliest {
					window.Until = &bestAt
				} else {
					window.Since = &bestAt
				}
			}
			page, _, readErr := read.ReadDisplayPage(ctx, id, query, window)
			items, err = page.Items, readErr
		} else {
			items, _, err = h.items.LoadItems(ctx, id)
		}
		if err != nil {
			return "", err
		}
		inputs := slices.Backward(items)
		if earliest {
			inputs = slices.All(items)
		}
		for _, item := range inputs {
			if !watchInput(&item) {
				continue
			}
			if bestID != "" && ((earliest && !item.CreatedAt.Before(bestAt)) || (!earliest && !item.CreatedAt.After(bestAt))) {
				continue
			}
			// A paused queue can be dispatched after newer direct submissions.
			// Compare the original queue timestamp, not the batch admission time.
			if hasQueue {
				batch, err := h.catalog.IsQueuedBatch(ctx, id, item.ID)
				if err != nil {
					return "", err
				}
				if batch {
					continue
				}
			}
			bestID, bestAt = item.ID, item.CreatedAt
		}
		if !indexed || len(items) < query.Limit {
			return bestID, nil
		}
		if earliest {
			query.After = items[len(items)-1].Sequence
		} else {
			query.Before, query.HasBefore = items[0].Sequence, true
		}
	}
}

func watchInput(item *session.DisplayItem) bool {
	if item.Subagent != nil || item.ID == "" {
		return false
	}
	if item.Command != nil {
		return item.Command.Type == wire.CommandMessage || (item.Command.Type == wire.CommandSteering && item.Command.TurnID != "")
	}
	// Codex history sync attaches the turn directly to the user message.
	return item.Type == session.DisplayItemUserMessage && item.Turn != nil && item.Turn.ID != ""
}

func watchInputTurn(item *session.DisplayItem) string {
	if item.Turn != nil {
		return item.Turn.ID
	}
	if item.Command != nil {
		return item.Command.TurnID
	}
	return ""
}

func (h *handler) validateWatchCommand(ctx context.Context, id, command string) error {
	// A queued command becomes durable in the log before the queue row is retired.
	row, err := h.catalog.QueuedInputByID(ctx, id, command)
	if err != nil {
		return err
	}
	if row != nil && row.State != "cancelled" {
		return nil
	}
	if read, ok := h.store.(*session.FileStore); ok {
		item, err := indexedWatchInput(ctx, read, id, command)
		if err != nil || item != nil {
			return err
		}
	} else {
		items, _, err := h.items.LoadItems(ctx, id)
		if err != nil {
			return err
		}
		for i := range items {
			if items[i].ID == command && watchInput(&items[i]) {
				return nil
			}
		}
	}
	return errors.New("watch_command_id is not a submitted message in the watched session")
}

func indexedWatchInput(ctx context.Context, read *session.FileStore, id, command string) (*session.DisplayItem, error) {
	seq, err := read.DisplaySequence(ctx, id, command)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	query := session.DisplayPageQuery{Limit: 1, HasAfter: true, After: seq - 1}
	page, _, err := read.ReadDisplayPage(ctx, id, query, session.DisplayTimeWindow{})
	if err != nil || len(page.Items) == 0 {
		return nil, err
	}
	item := &page.Items[0]
	if item.ID == command && watchInput(item) {
		return item, nil
	}
	return nil, nil
}

// Queue rows retain the ID of their dispatched batch. Read its root turn's
// latest terminal because Codex sync can append a corrected outcome.
func (h *handler) watchTerminal(ctx context.Context, id, command string) (*session.DisplayItem, error) {
	if command == "" {
		var err error
		command, err = h.submittedWatchCommand(ctx, id, true)
		if err != nil || command == "" {
			return nil, err
		}
	}
	row, err := h.catalog.QueuedInputByID(ctx, id, command)
	if err != nil {
		return nil, err
	}
	if row != nil {
		if row.BatchID == "" {
			return nil, nil
		}
		command = row.BatchID
	}
	read, indexed := h.store.(*session.FileStore)
	var turn string
	if indexed {
		input, err := indexedWatchInput(ctx, read, id, command)
		if err != nil || input == nil {
			return nil, err
		}
		turn = watchInputTurn(input)
		query := session.DisplayPageQuery{Limit: 32, HasAfter: true, After: input.Sequence, ContentKind: session.DisplayContentEvent}
		// An imported user message identifies its turn and may be backfilled
		// after the terminal record. Search from the newest event in that case.
		if input.Command == nil && turn != "" {
			query.HasAfter = false
		}
		var latest *session.DisplayItem
		for {
			page, _, err := read.ReadDisplayPage(ctx, id, query, session.DisplayTimeWindow{})
			if err != nil {
				return nil, err
			}
			if item := latestWatchTerminal(page.Items, &turn); item != nil {
				latest = item
				if !query.HasAfter {
					return latest, nil
				}
			}
			if len(page.Items) < query.Limit {
				return latest, nil
			}
			if query.HasAfter {
				query.After = page.Items[len(page.Items)-1].Sequence
			} else {
				query.Before, query.HasBefore = page.Items[0].Sequence, true
			}
		}
	}
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		if watchInput(&item) && command == item.ID {
			turn = watchInputTurn(&item)
			if item.Command == nil && turn != "" {
				return latestWatchTerminal(items, &turn), nil
			}
			return latestWatchTerminal(items[i+1:], &turn), nil
		}
	}
	return nil, nil
}

func latestWatchTerminal(items []session.DisplayItem, turn *string) *session.DisplayItem {
	var terminal *session.DisplayItem
	for i := range items {
		item := &items[i]
		if item.Subagent != nil || item.Turn == nil || item.Turn.ID == "" {
			continue
		}
		switch item.Type {
		case session.DisplayItemTurnStarted:
			if *turn == "" {
				*turn = item.Turn.ID
			}
		case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
			if *turn == item.Turn.ID {
				terminal = item
			}
		}
	}
	return terminal
}

func (h *handler) maintainSessionWatches(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		h.reconcileSessionWatches(ctx, "")
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *handler) reconcileSessionWatches(ctx context.Context, target string) {
	watches, err := h.catalog.PendingSessionWatches(ctx, target)
	if err != nil {
		return
	}
	for _, w := range watches {
		terminal, err := h.watchTerminal(ctx, w.WatchSessionID, w.BoundCommandID)
		if err != nil || terminal == nil {
			continue
		}
		text := fmt.Sprintf("Session %s, turn %s: %s.", w.WatchSessionID, terminal.Turn.ID, terminal.Type)
		unlock := h.queueOps.lock(w.SessionID)
		inserted, err := h.enqueueCompletion(ctx, w, text)
		unlock()
		if err == nil && inserted {
			go func() { _ = h.dispatchQueuedInputs(w.SessionID, false, false) }()
		}
	}
}
