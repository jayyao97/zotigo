package sessionlog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type ItemSource interface {
	LoadItems(ctx context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error)
	AppendItem(ctx context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error)
	AppendItemIf(ctx context.Context, sessionID string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error)
}

type OffsetItemSource interface {
	LoadItemsFromOffset(ctx context.Context, sessionID string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
}

type BatchItemSource interface {
	AppendItems(ctx context.Context, sessionID string, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error)
	AppendItemsAfter(ctx context.Context, sessionID string, expectedSequence uint64, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error)
}

type AtomicItemSource interface {
	AppendItemAtomically(ctx context.Context, sessionID string, build func([]zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error)) (zotigosession.DisplayItem, bool, error)
}

type StoredItemSource struct {
	Store zotigosession.Store
}

type EventingItemSource struct {
	Source ItemSource
	Events *EventBroker
}

type EventingOffsetItemSource struct {
	EventingItemSource
	Offset OffsetItemSource
}

func (s EventingItemSource) LoadItems(ctx context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
	return s.Source.LoadItems(ctx, sessionID)
}

func (s EventingOffsetItemSource) LoadItemsFromOffset(ctx context.Context, sessionID string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error) {
	return s.Offset.LoadItemsFromOffset(ctx, sessionID, offset, maxLines)
}

func (s EventingItemSource) AppendItem(ctx context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	stored, err := s.Source.AppendItem(ctx, sessionID, item)
	if err == nil {
		s.Events.Wake(sessionID)
	}
	return stored, err
}

func (s EventingItemSource) AppendItemIf(ctx context.Context, sessionID string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error) {
	stored, err := s.Source.AppendItemIf(ctx, sessionID, item, condition)
	if err == nil {
		s.Events.Wake(sessionID)
	}
	return stored, err
}

func (s EventingItemSource) AppendItemAtomically(ctx context.Context, sessionID string, build func([]zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error)) (zotigosession.DisplayItem, bool, error) {
	source, ok := s.Source.(AtomicItemSource)
	if !ok {
		return zotigosession.DisplayItem{}, false, errors.New("display item source does not support atomic append")
	}
	stored, appended, err := source.AppendItemAtomically(ctx, sessionID, build)
	if err == nil && appended {
		s.Events.Wake(sessionID)
	}
	return stored, appended, err
}

func (s EventingItemSource) AppendItems(ctx context.Context, sessionID string, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	batch, ok := s.Source.(BatchItemSource)
	if !ok {
		return nil, errors.New("display item source does not support atomic append")
	}
	stored, err := batch.AppendItems(ctx, sessionID, items)
	if err == nil && len(stored) > 0 {
		s.Events.Wake(sessionID)
	}
	return stored, err
}

func (s EventingItemSource) AppendItemsAfter(ctx context.Context, sessionID string, expectedSequence uint64, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	batch, ok := s.Source.(BatchItemSource)
	if !ok {
		return nil, errors.New("display item source does not support atomic append")
	}
	stored, err := batch.AppendItemsAfter(ctx, sessionID, expectedSequence, items)
	if err == nil && len(stored) > 0 {
		s.Events.Wake(sessionID)
	}
	return stored, err
}

func (s StoredItemSource) LoadItems(ctx context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
	return s.Store.ListDisplayItems(ctx, sessionID)
}

func (s StoredItemSource) LoadItemsFromOffset(ctx context.Context, sessionID string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error) {
	type offsetStore interface {
		ListDisplayItemsFromOffset(ctx context.Context, id string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
	}
	store, ok := s.Store.(offsetStore)
	if !ok {
		items, exists, err := s.Store.ListDisplayItems(ctx, sessionID)
		return items, exists, 0, err
	}
	return store.ListDisplayItemsFromOffset(ctx, sessionID, offset, maxLines)
}

func (s StoredItemSource) AppendItem(ctx context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return zotigosession.DisplayItem{}, err
	}
	return s.Store.AppendDisplayItem(ctx, sessionID, item)
}

func (s StoredItemSource) AppendItemIf(ctx context.Context, sessionID string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return zotigosession.DisplayItem{}, err
	}
	type conditionalStore interface {
		AppendDisplayItemIf(ctx context.Context, id string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error)
	}
	if store, ok := s.Store.(conditionalStore); ok {
		return store.AppendDisplayItemIf(ctx, sessionID, item, condition)
	}
	items, _, err := s.Store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return zotigosession.DisplayItem{}, err
	}
	if condition != nil {
		if err := condition(items); err != nil {
			return zotigosession.DisplayItem{}, err
		}
	}
	return s.Store.AppendDisplayItem(ctx, sessionID, item)
}

func (s StoredItemSource) AppendItemAtomically(ctx context.Context, sessionID string, build func([]zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error)) (zotigosession.DisplayItem, bool, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return zotigosession.DisplayItem{}, false, err
	}
	type atomicStore interface {
		AppendDisplayItemAtomically(context.Context, string, func([]zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error)) (zotigosession.DisplayItem, bool, error)
	}
	store, ok := s.Store.(atomicStore)
	if !ok {
		return zotigosession.DisplayItem{}, false, errors.New("session store does not support atomic display append")
	}
	return store.AppendDisplayItemAtomically(ctx, sessionID, build)
}

func (s StoredItemSource) AppendItems(ctx context.Context, sessionID string, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return nil, err
	}
	type batchStore interface {
		AppendDisplayItems(context.Context, string, []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error)
	}
	store, ok := s.Store.(batchStore)
	if !ok {
		return nil, errors.New("session store does not support atomic display append")
	}
	return store.AppendDisplayItems(ctx, sessionID, items)
}

func (s StoredItemSource) AppendItemsAfter(ctx context.Context, sessionID string, expectedSequence uint64, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return nil, err
	}
	type batchStore interface {
		AppendDisplayItemsAfter(context.Context, string, uint64, []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error)
	}
	store, ok := s.Store.(batchStore)
	if !ok {
		return nil, errors.New("session store does not support conditional atomic display append")
	}
	return store.AppendDisplayItemsAfter(ctx, sessionID, expectedSequence, items)
}

func (s StoredItemSource) ensureSession(ctx context.Context, sessionID string) error {
	sess, err := s.Store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}
	if sess != nil {
		return nil
	}

	now := time.Now().UTC()
	return s.Store.Put(ctx, &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: now,
		},
		Turns: make([]zotigosession.Turn, 0),
	})
}

type FailingItemSource struct {
	Err error
}

func (s FailingItemSource) LoadItems(context.Context, string) ([]zotigosession.DisplayItem, bool, error) {
	return nil, false, s.Err
}

func (s FailingItemSource) AppendItem(context.Context, string, zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	return zotigosession.DisplayItem{}, s.Err
}

func (s FailingItemSource) AppendItemIf(context.Context, string, zotigosession.DisplayItem, func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error) {
	return zotigosession.DisplayItem{}, s.Err
}
