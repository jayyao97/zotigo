package workspace

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/catalogschema"
)

func (s *Store) migrate(ctx context.Context) error {
	if s.schemaLock != nil {
		_ = s.schemaLock.Close()
		s.schemaLock = nil
	}
	lease, err := catalogschema.Acquire(ctx, s.rootDir, s.db, true)
	if err != nil {
		return err
	}
	s.schemaLock = lease
	return nil
}

// MigrateCatalog changes the schema only while daemon and workers are stopped.
func MigrateCatalog(ctx context.Context, rootDir string, target uint) error {
	if target < catalogschema.Baseline || target > schemaVersion {
		return fmt.Errorf("catalog migration target must be between %d and %d", catalogschema.Baseline, schemaVersion)
	}
	store, err := openWriter(rootDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return catalogschema.MigrateOffline(ctx, store.db, store.rootDir, target)
}
