package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

const schemaVersion = 9

type Store struct {
	db          *sql.DB
	rootDir     string
	operationMu sync.Mutex
	writerLock  *flock.Flock
}

// OpenReadOnly opens an existing catalog without running migrations or writing
// pragmas. It is intended for CLI projections; zotigod remains the sole writer.
func OpenReadOnly(rootDir string) (*Store, error) {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("workspace catalog home: %w", err)
		}
		rootDir = filepath.Join(home, ".zotigo")
	}
	dbPath := filepath.Join(rootDir, "catalog.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("open workspace catalog read-only: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open workspace catalog read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, rootDir: rootDir}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_meta WHERE singleton = 1`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read workspace catalog version: %w", err)
	}
	if version != schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("workspace catalog version %d is not supported", version)
	}
	return store, nil
}

func Open(rootDir string) (*Store, error) {
	store, err := openWriter(rootDir, false)
	if err != nil {
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// openWriter holds a process-wide file lock for the lifetime of the store.
// Migration commands use the same lock and never open a serving catalog.
func openWriter(rootDir string, existingOnly bool) (*Store, error) {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("workspace catalog home: %w", err)
		}
		rootDir = filepath.Join(home, ".zotigo")
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace catalog root: %w", err)
	}

	dbPath := filepath.Join(rootDir, "catalog.sqlite")
	if existingOnly {
		if _, err := os.Stat(dbPath); err != nil {
			return nil, fmt.Errorf("open existing workspace catalog: %w", err)
		}
	}
	lock := flock.New(filepath.Join(rootDir, "catalog.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock workspace catalog: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("workspace catalog is in use; stop zotigod before migrating")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("open workspace catalog: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, rootDir: rootDir, writerLock: lock}
	if err := db.Ping(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("protect workspace catalog: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	if s.writerLock != nil {
		err = errors.Join(err, s.writerLock.Close())
	}
	return err
}

func (s *Store) RootDir() string {
	return s.rootDir
}
