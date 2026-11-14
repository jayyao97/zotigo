package catalogschema

import (
	"context"
	"fmt"
	"testing"
)

func TestCompatibleExtensionsPreserveDev31MetadataAndQueueSequence(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	db := testDatabase(t, root)
	if err := Migrate(ctx, db, 15); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO session_input_queue(sequence,session_id,id,payload,state) VALUES(200,'s','removed','{}','sent'); DELETE FROM session_input_queue`)
	if err := upgrade(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	var base, journal int
	var dirty bool
	if err := db.QueryRow(`SELECT version FROM schema_meta`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT version,dirty FROM catalog_migrations`).Scan(&journal, &dirty); err != nil {
		t.Fatal(err)
	}
	if base != 15 || journal != 15 || dirty {
		t.Fatalf("dev31 cannot open metadata: %d/%d dirty=%t", base, journal, dirty)
	}
	execSQL(t, db, `INSERT INTO session_input_queue(session_id,id,payload,state) VALUES('s','normal','{}','queued'),('s','completion:1','{}','completion_queued')`)
	var sequence int
	if err := db.QueryRow(`SELECT sequence FROM session_input_queue WHERE id='normal'`).Scan(&sequence); err != nil || sequence != 201 {
		t.Fatalf("sequence high water mark lost: %d %v", sequence, err)
	}
	// These are dev.31's recovery/claim predicates. They must not authorize a
	// completion notification even if an old daemon resumes the ordinary queue.
	execSQL(t, db, `UPDATE session_input_queue SET state='paused',revision=revision+1 WHERE state IN ('queued','sending'); UPDATE session_input_queue SET state='queued',revision=revision+1 WHERE session_id='s' AND state='paused' AND batch_id=''; UPDATE session_input_queue SET batch_id='old-batch',state='sending',revision=revision+1 WHERE session_id='s' AND batch_id='' AND state IN ('queued','paused')`)
	var state, batch string
	if err := db.QueryRow(`SELECT state,batch_id FROM session_input_queue WHERE id='completion:1'`).Scan(&state, &batch); err != nil {
		t.Fatal(err)
	}
	if state != "completion_queued" || batch != "" {
		t.Fatalf("old daemon claimed callback: %s %s", state, batch)
	}
	if err := upgrade(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM session_input_queue WHERE id='normal'`).Scan(&state); err != nil || state != "sending" {
		t.Fatalf("new loader lost old writes: %s %v", state, err)
	}
	if version, err := extensionVersion(ctx, db); err != nil || version != ExtensionVersion {
		t.Fatalf("extensions=%d %v", version, err)
	}
}

func TestExtensionFailureRollsBackOnlyTheIncompleteAddition(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	db := testDatabase(t, root)
	if err := Migrate(ctx, db, 15); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `CREATE VIEW repositories AS SELECT 1 AS id`)
	if err := migrateExtensions(ctx, db, ExtensionVersion); err == nil {
		t.Fatal("expected extension DDL conflict")
	}
	if version, err := extensionVersion(ctx, db); err != nil || version != 1 {
		t.Fatalf("partial addition committed: %d %v", version, err)
	}
	var base, journal, columns int
	if err := db.QueryRow(`SELECT version FROM schema_meta`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT version FROM catalog_migrations WHERE dirty=0`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('workspace_checkouts') WHERE name='used_cached_base'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if base != 15 || journal != 15 || columns != 0 {
		t.Fatalf("old base changed on failure: %d %d %d", base, journal, columns)
	}
	execSQL(t, db, `DROP VIEW repositories`)
	if err := migrateExtensions(ctx, db, ExtensionVersion); err != nil {
		t.Fatal(err)
	}
}

func TestCompatibleFutureExtensionsDoNotBlockBinaryRollback(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	db := testDatabase(t, root)
	if err := upgrade(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, fmt.Sprintf(`CREATE TABLE future_addition(id INTEGER); INSERT INTO future_addition VALUES(42); UPDATE catalog_extensions SET version=%d`, ExtensionVersion+1))
	if ok, err := ready(ctx, db); err != nil || !ok {
		t.Fatalf("future compatible extension blocks old startup: %t %v", ok, err)
	}
	if err := CheckExtensions(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := upgrade(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	var value, version int
	if err := db.QueryRow(`SELECT id FROM future_addition`).Scan(&value); err != nil || value != 42 {
		t.Fatalf("future data removed: %d %v", value, err)
	}
	if err := db.QueryRow(`SELECT version FROM catalog_extensions`).Scan(&version); err != nil || version != ExtensionVersion+1 {
		t.Fatalf("future journal downgraded: %d %v", version, err)
	}
	if err := MigrateOffline(ctx, db, root, 14); err == nil {
		t.Fatal("unknown destructive downgrade accepted")
	}
}
