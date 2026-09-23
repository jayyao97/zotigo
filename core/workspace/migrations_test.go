package workspace

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func TestCatalogMigrationRoundTrip(t *testing.T) {
	s, ws, _ := createGitWorkspaceFixture(t)
	ctx := context.Background()
	if _, err := s.AssignSession(ctx, "preserved", ws.ID); err != nil {
		t.Fatal(err)
	}
	for _, item := range []NavigationItem{{Kind: "session", ID: "preserved"}, {Kind: "project", ID: ws.ProjectID}, {Kind: "workspace", ID: ws.ID}} {
		if err := s.SetNavigationPinned(ctx, item, true); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.GetSessionOrganization(ctx, "preserved")
	if err != nil {
		t.Fatal(err)
	}
	root := s.RootDir()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := MigrateCatalog(ctx, root, 7); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(root, "catalog.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		var version, remaining int
		err = db.QueryRow("SELECT version FROM schema_meta").Scan(&version)
		if err != nil || version != 7 {
			t.Fatalf("downgraded version = %d: %v", version, err)
		}
		err = db.QueryRow("SELECT count(*) FROM pragma_table_info('projects') WHERE name IN ('position','pinned_at','pinned_position')").Scan(&remaining)
		if err != nil || remaining != 0 {
			t.Fatalf("navigation columns remain: %d, %v", remaining, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		// Startup performs the upgrade again, without losing sessions/checkouts.
		s, err = Open(root)
		if err != nil {
			t.Fatal(err)
		}
		after, err := s.GetSessionOrganization(ctx, "preserved")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("session changed: before=%+v after=%+v err=%v", before, after, err)
		}
		actual, err := s.GetWorkspace(ctx, ws.ID)
		if err != nil || actual.RootPath != ws.RootPath {
			t.Fatalf("workspace path changed: %+v, %v", actual, err)
		}
		nav, err := s.GetNavigation(ctx)
		if err != nil || !reflect.DeepEqual(nav.Pinned, []NavigationItem{{Kind: "session", ID: "preserved"}}) {
			t.Fatalf("pins after round trip: %+v, %v", nav, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogMigrationRejectsActiveWriterAndInvalidTargets(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := MigrateCatalog(context.Background(), s.RootDir(), 7); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("active writer migration error = %v", err)
	}
	if other, err := Open(s.RootDir()); err == nil {
		_ = other.Close()
		t.Fatal("second writer acquired catalog")
	}
	reader, err := OpenReadOnly(s.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	for _, target := range []uint{0, 6, schemaVersion + 1} {
		if err := MigrateCatalog(context.Background(), s.RootDir(), target); err == nil {
			t.Fatalf("accepted target %d", target)
		}
	}
	if err := MigrateCatalog(context.Background(), t.TempDir(), 7); err == nil {
		t.Fatal("migration created missing catalog")
	}
}

func TestCatalogMigrationAdoptsLegacyV8(t *testing.T) {
	s, err := openWriter(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Use the actual frozen initializer: a new SQL database with its journal
	// deleted cannot reproduce historical inline UNIQUE constraints/defaults.
	if err := s.migrateLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	project, err := s.CreateProject(context.Background(), "Before adoption")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProject(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	physicalSchema := func(store *Store) []string {
		t.Helper()
		rows, err := store.db.Query(`SELECT name || ':' || sql FROM sqlite_master
WHERE sql IS NOT NULL AND name NOT IN ('schema_meta', 'navigation_migration', 'catalog_migrations', 'version_unique') ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var schema []string
		for rows.Next() {
			var statement string
			if err := rows.Scan(&statement); err != nil {
				t.Fatal(err)
			}
			schema = append(schema, statement)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return schema
	}
	if !reflect.DeepEqual(physicalSchema(s), physicalSchema(fresh)) {
		t.Fatal("adopted catalog differs physically from fresh SQL migration schema")
	}
	// Subsequent Atlas migrations can address the same indexes in both catalogs.
	if err := (&catalogMigrationDriver{db: s.db, ctx: context.Background()}).Run(strings.NewReader(`
DROP INDEX projects_storage_name;
CREATE UNIQUE INDEX projects_storage_name ON projects(storage_name);`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE catalog_migrations SET version=7"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(context.Background()); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("mismatched journal error = %v", err)
	}
}

func TestCatalogMigrationFailureRollsBackAndStaysDirty(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	driver, err := migratesqlite.WithInstance(s.db, &migratesqlite.Config{MigrationsTable: "catalog_migrations"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := iofs.New(fstest.MapFS{
		"000009_existing.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		"000010_broken.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE must_rollback(id INTEGER); UPDATE schema_meta SET version=10; INSERT INTO missing_table VALUES(1);")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	m, err := migrate.NewWithInstance("iofs", source, "sqlite", &catalogMigrationDriver{Driver: driver, db: s.db, ctx: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(10); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	var exists bool
	if err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='must_rollback')").Scan(&exists); err != nil || exists {
		t.Fatalf("partial DDL survived: %v, %v", exists, err)
	}
	version, err := s.catalogVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("partial version survived: %d, %v", version, err)
	}
	if err := s.migrate(context.Background()); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty migration was not blocked: %v", err)
	}
}

func TestCatalogMigrationRebuildPreservesChildrenAndValidatesForeignKeys(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`CREATE TABLE parent(id INTEGER PRIMARY KEY);
CREATE TABLE child(parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE);
INSERT INTO parent VALUES(1); INSERT INTO child VALUES(1);`)
	if err != nil {
		t.Fatal(err)
	}
	driver := &catalogMigrationDriver{db: s.db, ctx: context.Background()}
	if err := driver.Run(strings.NewReader(`CREATE TABLE new_parent(id INTEGER PRIMARY KEY, added TEXT);
INSERT INTO new_parent(id) SELECT id FROM parent;
DROP TABLE parent; ALTER TABLE new_parent RENAME TO parent;`)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM child").Scan(&count); err != nil || count != 1 {
		t.Fatalf("rebuild deleted children: %d, %v", count, err)
	}
	if err := driver.Run(strings.NewReader("DELETE FROM parent;")); err == nil {
		t.Fatal("committed broken foreign keys")
	}
	if err := s.db.QueryRow("SELECT count(*) FROM parent").Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed migration was not rolled back: %d, %v", count, err)
	}
	if _, err := s.db.Exec("INSERT INTO child VALUES(99)"); err == nil {
		t.Fatal("foreign key enforcement was not restored")
	}
}
