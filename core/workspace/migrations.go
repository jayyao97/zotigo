package workspace

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

const catalogBaseline = 7

//go:embed migrations/*.sql
var catalogMigrations embed.FS

func (s *Store) migrate(ctx context.Context) error {
	return s.migrateTo(ctx, schemaVersion)
}

// MigrateCatalog changes an existing, offline catalog to a supported version.
// Open always upgrades to the latest version; downgrades must be explicit.
func MigrateCatalog(ctx context.Context, rootDir string, target uint) error {
	if target < catalogBaseline || target > schemaVersion {
		return fmt.Errorf("catalog target must be between %d and %d", catalogBaseline, schemaVersion)
	}
	s, err := openWriter(rootDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	return s.migrateTo(ctx, target)
}

func (s *Store) migrateTo(ctx context.Context, target uint) error {
	for _, pragma := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000"} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure workspace catalog: %w", err)
		}
	}
	version, err := s.catalogVersion(ctx)
	if err != nil {
		return err
	}
	if version > schemaVersion || version < 0 {
		return fmt.Errorf("workspace catalog version %d is not supported", version)
	}
	driver, err := migratesqlite.WithInstance(s.db, &migratesqlite.Config{MigrationsTable: "catalog_migrations"})
	if err != nil {
		return err
	}
	// Check the journal ourselves: the upstream SQLite driver's Version method
	// treats every query error as an uninitialized database.
	var recorded int
	var dirty bool
	err = s.db.QueryRowContext(ctx, "SELECT version, dirty FROM catalog_migrations").Scan(&recorded, &dirty)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read catalog migration journal: %w", err)
	}
	if dirty {
		return fmt.Errorf("catalog migration %d is dirty; restore a consistent backup before retrying", recorded)
	}
	if err == nil && recorded != version {
		return fmt.Errorf("catalog migration journal version %d differs from schema version %d", recorded, version)
	}
	if errors.Is(err, sql.ErrNoRows) && version > 0 {
		if version < catalogBaseline {
			if err := s.migrateLegacy(ctx); err != nil {
				return err
			}
			version = legacySchemaVersion
		}
		// Adopt only pre-journal catalogs, never overwrite a dirty or inconsistent
		// journal. Existing 7/8 catalogs already contain their baseline schema.
		if err := driver.SetVersion(version, false); err != nil {
			return err
		}
	}
	source, err := iofs.New(catalogMigrations, "migrations")
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	m, err := migrate.NewWithInstance("iofs", source, "sqlite", &catalogMigrationDriver{Driver: driver, db: s.db, ctx: ctx})
	if err != nil {
		return err
	}
	if err := m.Migrate(target); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate workspace catalog: %w", err)
	}
	version, err = s.catalogVersion(ctx)
	if err != nil {
		return err
	}
	if version != int(target) {
		return fmt.Errorf("migration did not record schema version %d (found %d)", target, version)
	}
	return nil
}

func (s *Store) catalogVersion(ctx context.Context) (int, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='schema_meta')").Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var version int
	err := s.db.QueryRowContext(ctx, "SELECT version FROM schema_meta WHERE singleton=1").Scan(&version)
	return version, err
}

// Atlas may rebuild referenced tables when SQLite cannot ALTER them. Disable
// foreign-key actions outside the transaction, then validate all references
// before committing, so a rebuild cannot cascade-delete child records.
type catalogMigrationDriver struct {
	database.Driver
	db  *sql.DB
	ctx context.Context
}

func (d *catalogMigrationDriver) Run(r io.Reader) (err error) {
	query, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if _, err := d.db.ExecContext(d.ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer func() {
		_, restoreErr := d.db.ExecContext(context.Background(), "PRAGMA foreign_keys = ON")
		err = errors.Join(err, restoreErr)
	}()
	tx, err := d.db.BeginTx(d.ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(d.ctx, string(query)); err != nil {
		return err
	}
	var violations int
	if err := tx.QueryRowContext(d.ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return fmt.Errorf("migration introduced %d foreign key violations", violations)
	}
	return tx.Commit()
}
