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

func TestBackfillSessionDialogueActivity(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	db := testDatabase(t, root)
	if err := Migrate(ctx, db, 15); err != nil {
		t.Fatal(err)
	}
	if err := migrateExtensions(ctx, db, 6); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO sessions(id,working_directory,created_at,updated_at)
		VALUES ('resumed','/',1,1),('newer','/',1,5),('empty','/',1,1);
		INSERT INTO sessions(id,working_directory,agent,created_at,updated_at,backend_updated_at)
		VALUES ('imported','/','codex',1,2,2);
		INSERT INTO display_items(session_id,sequence,message_at,dialogue,offset,length)
		VALUES ('resumed',1,3,1,0,10),('resumed',2,4,0,10,10),('newer',1,2,1,0,10),
		('imported',1,9,1,0,10)`)
	if err := ensureExtensions(ctx, db); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"resumed": 3, "newer": 5, "empty": 1, "imported": 2} {
		var got int
		if err := db.QueryRow("SELECT updated_at FROM sessions WHERE id=?", id).Scan(&got); err != nil || got != want {
			t.Fatalf("%s updated_at=%d, want %d: %v", id, got, want, err)
		}
	}
}

func TestDetachedSessionProvenance(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	db := testDatabase(t, root)
	if err := Migrate(ctx, db, 15); err != nil {
		t.Fatal(err)
	}
	if err := migrateExtensions(ctx, db, 7); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO projects(id,name,storage_name,created_at,updated_at) VALUES('p','p','p',1,1);
		INSERT INTO workspaces(id,project_id,title,storage_name,root_path,owner_nonce,status,deleted_at,created_at,updated_at)
		VALUES('w','p','w','w','/work','nonce','deleted',10,1,10);
		INSERT INTO sessions(id,working_directory,created_at,updated_at)
		VALUES ('inside','/work/code',1,1),('unrelated','/work-other',1,1),('different-time','/work',1,1);
		INSERT INTO session_organization(session_id,self_archived_at,created_at,updated_at,activity_at)
		VALUES ('inside',3,1,10,1),('unrelated',10,1,10,1),('different-time',11,1,11,1)`)
	if err := ensureExtensions(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Older daemons use this same delete transaction, irrespective of cwd.
	execSQL(t, db, `INSERT INTO workspaces(id,project_id,title,storage_name,root_path,owner_nonce,status,created_at,updated_at)
		VALUES('future','p','future','future','/future','nonce','deleting',1,20);
		INSERT INTO sessions(id,working_directory,created_at,updated_at) VALUES('assigned','/elsewhere',1,1);
		INSERT INTO session_organization(session_id,project_id,workspace_id,created_at,updated_at,activity_at)
		VALUES ('assigned','p','future',1,1,1);
		UPDATE session_organization SET workspace_id=NULL,project_id=NULL,self_archived_at=20,updated_at=20 WHERE session_id='assigned';`)
	for id, want := range map[string]string{"inside": "w", "unrelated": "", "different-time": "", "assigned": "future"} {
		var got string
		if err := db.QueryRow("SELECT coalesce(deleted_workspace_id,'') FROM session_organization WHERE session_id=?", id).Scan(&got); err != nil || got != want {
			t.Fatalf("origin of %s = %q, want %q: %v", id, got, want, err)
		}
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

func TestDelegatedQueueExtensionRoundTrip(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	db := testDatabase(t, root)
	if err := upgrade(ctx, db, root); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO session_input_queue(session_id,id,payload,state) VALUES('s','normal','{}','queued'),('s','completion:1','{}','completion_paused'),('s','session_tool:1','{}','delegated_queued')`)
	// The previous daemon cannot select this delegation for admission.
	var runnable int
	if err := db.QueryRow(`SELECT count(*) FROM session_input_queue WHERE id='session_tool:1' AND state IN ('queued','paused','sending','completion_queued','completion_paused','completion_sending')`).Scan(&runnable); err != nil || runnable != 0 {
		t.Fatalf("legacy runnable = %d, err = %v", runnable, err)
	}
	for _, version := range []int{4, 5} {
		if err := migrateExtensions(ctx, db, version); err != nil {
			t.Fatal(err)
		}
	}
	var preserved int
	if err := db.QueryRow(`SELECT count(*) FROM session_input_queue WHERE (id='normal' AND state='queued') OR (id='completion:1' AND state='completion_paused')`).Scan(&preserved); err != nil || preserved != 2 {
		t.Fatalf("preserved = %d, err = %v", preserved, err)
	}
}
