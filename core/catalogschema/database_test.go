package catalogschema

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func execSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func testDatabase(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := OpenDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func legacyDatabase(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "session_index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schema, err := os.ReadFile("testdata/session_index_v9.sql")
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"+string(schema))
	return db
}

func tableRows(t *testing.T, db *sql.DB, table string) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM "` + table + `" ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLegacyImportPreservesAllTablesAndWAL(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	db := testDatabase(t, root)
	if err := Migrate(ctx, db, 9); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO projects(id,name,storage_name,created_at,updated_at) VALUES('p','Project','project',1,2)`)
	legacy := legacyDatabase(t, root)
	execSQL(t, legacy, `INSERT INTO sessions VALUES('s','/work','codex','profile','model','high','thread','v1',3,4,'bypass','{"key":"value"}','{"flag":true}','prompt',1,2);
INSERT INTO session_images VALUES('s','photo','/blob','image/png',123,10,20,1);
INSERT INTO metadata VALUES('bootstrapped','1'),('legacy_registry_mtime','999');
INSERT INTO display_items VALUES('s',1,100,1,0,64),('s',2,110,0,64,32);
INSERT INTO display_index_files VALUES('s',96,1234,96);`)
	if info, err := os.Stat(filepath.Join(root, "session_index.sqlite-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("fixture must include uncheckpointed WAL: %v", err)
	}
	before := map[string][][]any{}
	for _, table := range sessionTables {
		before[table] = tableRows(t, legacy, table)
	}
	lease, err := Acquire(ctx, root, db, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range sessionTables {
		got := tableRows(t, db, table)
		want := before[table]
		if table == "display_items" {
			for i, row := range got {
				if row[len(row)-1] != "unknown" {
					t.Fatalf("legacy content must await classification: %v", row)
				}
				got[i] = row[:len(row)-1]
			}
		}
		if table == "display_index_files" {
			// Imported logs must be reclassified, but the legacy file below stays
			// untouched, including its original freshness markers.
			want = nil
		}
		if table == "metadata" {
			var filtered [][]any
			for _, row := range got {
				if row[0] != importMarker {
					filtered = append(filtered, row)
				}
			}
			got = filtered
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed during import: got %v want %v", table, got, want)
		}
		if !reflect.DeepEqual(tableRows(t, legacy, table), before[table]) {
			t.Fatalf("legacy %s was modified", table)
		}
	}
	var project string
	if err := db.QueryRow("SELECT name FROM projects WHERE id='p'").Scan(&project); err != nil || project != "Project" {
		t.Fatalf("lost catalog row: %q, %v", project, err)
	}
	execSQL(t, db, "UPDATE sessions SET model='new'; DELETE FROM session_images; DELETE FROM display_items; DELETE FROM display_index_files")
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	// A restart must neither overwrite current rows nor resurrect deleted ones.
	lease, err = Acquire(ctx, root, db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var model string
	if err := db.QueryRow("SELECT model FROM sessions WHERE id='s'").Scan(&model); err != nil || model != "new" {
		t.Fatalf("reimport overwrote current row: %q, %v", model, err)
	}
	for _, table := range []string{"session_images", "display_items", "display_index_files"} {
		if rows := tableRows(t, db, table); len(rows) != 0 {
			t.Fatalf("resurrected %s: %v", table, rows)
		}
	}
}

func TestLegacyImportFailureRollsBackAndRetries(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	legacy := legacyDatabase(t, root)
	execSQL(t, legacy, `INSERT INTO sessions(id,working_directory,created_at,updated_at) VALUES('s','/work',1,2);
DROP TABLE display_items;
CREATE TABLE display_items(session_id TEXT, sequence INTEGER, message_at INTEGER, dialogue INTEGER, offset INTEGER, length INTEGER);
INSERT INTO display_items VALUES('s',1,1,1,0,NULL);`)
	db := testDatabase(t, root)
	lease, err := Acquire(ctx, root, db, false)
	if err == nil {
		_ = lease.Close()
		t.Fatal("invalid legacy row imported")
	}
	for _, table := range sessionTables {
		if rows := tableRows(t, db, table); len(rows) != 0 {
			t.Fatalf("partial import survived in %s: %v", table, rows)
		}
	}
	// Schema is already current, but the absent marker requires retrying import.
	execSQL(t, legacy, "UPDATE display_items SET length=64")
	lease, err = Acquire(ctx, root, db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if rows := tableRows(t, db, "sessions"); len(rows) != 1 {
		t.Fatalf("retry did not import session: %v", rows)
	}
	if rows := tableRows(t, db, "display_items"); len(rows) != 1 {
		t.Fatalf("retry did not import display row: %v", rows)
	}
}

func TestLegacyImportMissingColumnsUsesDefaults(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	legacy := legacyDatabase(t, root)
	execSQL(t, legacy, `DROP TABLE sessions;
CREATE TABLE sessions(id TEXT PRIMARY KEY, working_directory TEXT NOT NULL, last_prompt TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
INSERT INTO sessions VALUES('old','/work','hello',1,2);
ALTER TABLE display_index_files DROP COLUMN observed_size;
INSERT INTO display_index_files VALUES('old',64,200);
DROP TABLE session_images;`)
	db := testDatabase(t, root)
	lease, err := Acquire(ctx, root, db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var agent, policy, prompt, last string
	if err := db.QueryRow("SELECT agent,approval_policy,prompt_config,last_prompt FROM sessions WHERE id='old'").Scan(&agent, &policy, &prompt, &last); err != nil || agent != "zotigo" || policy != "auto" || prompt != "{}" || last != "hello" {
		t.Fatalf("legacy defaults/data: %q %q %q %q: %v", agent, policy, prompt, last, err)
	}
	var observed int
	if err := db.QueryRow("SELECT observed_size FROM display_index_files WHERE session_id='old'").Scan(&observed); err != nil || observed != -1 {
		t.Fatalf("legacy log must require reindex: %d, %v", observed, err)
	}
}

func TestConcurrentFirstOpenAndWorkerLease(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	const workers = 8
	var group sync.WaitGroup
	errors := make(chan error, workers)
	leases := make(chan *flock.Flock, workers)
	start := make(chan struct{})
	for range workers {
		db := testDatabase(t, root)
		group.Go(func() {
			<-start
			lease, err := Acquire(ctx, root, db, false)
			if err != nil {
				errors <- err
				return
			}
			leases <- lease
		})
	}
	close(start)
	group.Wait()
	close(errors)
	close(leases)
	for err := range errors {
		t.Error(err)
	}
	db := testDatabase(t, root)
	if err := MigrateOffline(ctx, db, root, 9); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("active worker allowed downgrade: %v", err)
	}
	for lease := range leases {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}
	// Closing all stores releases the lease, allowing an empty schema round trip.
	if err := MigrateOffline(ctx, db, root, 9); err != nil {
		t.Fatal(err)
	}
	if err := MigrateOffline(ctx, db, root, Version); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "session_index.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("fresh install created a second SQLite file: %v", err)
	}
}

func TestDowngradeRejectsPopulatedUnifiedStorage(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	db := testDatabase(t, root)
	if err := MigrateOffline(ctx, db, root, Version); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, "INSERT INTO sessions(id,working_directory,created_at,updated_at) VALUES('new','/work',1,2)")
	if err := MigrateOffline(ctx, db, root, 9); err == nil || !strings.Contains(err.Error(), "populated") {
		t.Fatalf("destructive downgrade allowed: %v", err)
	}
	if ok, err := ready(ctx, db); !ok || err != nil {
		t.Fatalf("refused downgrade changed migration state: ready=%v err=%v", ok, err)
	}
}

func TestWorkerOpenRespectsCatalogOwner(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	db := testDatabase(t, root)
	if err := Migrate(ctx, db, 9); err != nil {
		t.Fatal(err)
	}
	owner := flock.New(filepath.Join(root, "catalog.lock"))
	if err := owner.Lock(); err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	lease, err := Acquire(short, root, db, false)
	if err == nil {
		_ = lease.Close()
		t.Fatal("worker migrated a running old daemon's catalog")
	}
	if version, err := CatalogVersion(ctx, db); err != nil || version != 9 {
		t.Fatalf("blocked upgrade modified catalog: %d, %v", version, err)
	}
	// The owner upgrades before serving; workers then need only shared leases.
	ownerLease, err := Acquire(ctx, root, db, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerLease.Close()
	workerDB := testDatabase(t, root)
	lease, err = Acquire(ctx, root, workerDB, false)
	if err != nil {
		t.Fatalf("current-schema worker blocked by daemon: %v", err)
	}
	defer lease.Close()
	execSQL(t, workerDB, "INSERT INTO sessions(id,working_directory,created_at,updated_at) VALUES('worker','/work',1,2)")
	if rows := tableRows(t, db, "sessions"); len(rows) != 1 {
		t.Fatalf("daemon cannot see worker write: %v", rows)
	}
}

func TestCatalogTransactionSerializesSessionWriter(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	db := testDatabase(t, root)
	if err := MigrateOffline(ctx, db, root, Version); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO projects(id,name,storage_name,created_at,updated_at) VALUES('p','Project','project',1,1);
INSERT INTO sessions(id,working_directory,created_at,updated_at) VALUES('s','/work',1,1)`)
	worker := testDatabase(t, root)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var updated int
	if err := tx.QueryRow("SELECT updated_at FROM projects WHERE id='p'").Scan(&updated); err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() {
		_, err := worker.Exec("UPDATE sessions SET updated_at=2 WHERE id='s'")
		workerDone <- err
	}()
	select {
	case err := <-workerDone:
		t.Fatalf("worker changed a read-then-write transaction's snapshot: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := tx.Exec("UPDATE projects SET updated_at=? WHERE id='p'", updated+1); err != nil {
		t.Fatalf("catalog read-then-write failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("worker did not resume after catalog commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker stayed blocked after catalog commit")
	}
}
