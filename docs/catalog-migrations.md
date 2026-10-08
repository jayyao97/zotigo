# Catalog schema migrations

`core/catalogschema/schema.sql` declares the desired catalog and session index structure. Atlas Community
1.3.1 computes its difference from the replayed migration history and generates
reviewable SQL. `golang-migrate` executes the embedded SQL inside zotigod; users do
not need Atlas installed, and generation never connects to their catalog.

## Development

Install Atlas **Community 1.3.1** (the `atlas-community-<platform>-v1.3.1` binaries
from atlasbinaries.com). The Linux amd64 binary used for validation has SHA-256
`521aafcdc4c81a6aee3d553adc97633821a7cb28076ffcdf4add27add930c804`.
Set `ATLAS=/path/to/atlas` if it is not on PATH.

1. Edit `core/catalogschema/schema.sql`.
2. Run `make catalog-diff NAME=describe_change`.
3. Review both generated `.up.sql` and `.down.sql` files. Add explicit data
   backfills when required: a schema diff cannot infer business data transforms.
4. After editing SQL, run
   `atlas migrate hash --dir 'file://core/catalogschema/migrations?format=golang-migrate'`.
5. Run `make catalog-check` and `go test ./core/catalogschema ./core/workspace ./core/session ./internal/zotigod`.
   Add data-preservation and rollback tests for the change.

The generator assigns the next sequential version, appends the `schema_meta`
version update to each direction, and updates `catalogschema.Version` in Go. Commit
`schema.sql`, both SQL files, `atlas.sum`, and the Go version change together.
`catalog-check` uses a disposable database and fails when a schema change has
no corresponding migration. Run it before opening a PR; automatic CI wiring is
not included because the publishing credential cannot update GitHub workflows. Do not edit migrations already released.

Each file runs in one transaction. Do not include `BEGIN`, `COMMIT`, `ROLLBACK`,
or statements that cannot run in a transaction. SQLite table rebuilds run with
foreign-key actions disabled outside the transaction, then check every foreign
key before commit. This preserves child records across parent table rebuilds.
Do not run a generic migrate/Atlas apply command directly against the catalog:
use zotigod so the catalog lock and foreign-key checks are retained.

## Existing catalogs

Schema 7 is the oldest SQL migration baseline; schema 8 adds shared navigation.
`schema_legacy.go` is a frozen bridge for schemas 1–6, including historical
backfills and partial-schema handling. Existing 7/8 databases are adopted without
replaying the baseline. New versions must use generated SQL, not extend that bridge.

Schema 9 normalizes historical inline UNIQUE constraints and column defaults to
the canonical SQL schema, preserving every row. This explicit migration is needed
because old catalogs otherwise have different physical indexes from the generator's
baseline. Its down migration keeps the equivalent canonical representation while
returning to version 8; old version-8 binaries can use that representation.

Schema 10 moves the five session index tables (`sessions`, `session_images`,
`metadata`, `display_items`, `display_index_files`) into `catalog.sqlite`. Their
DDL is now part of the same generated migrations. Daemon, CLI and session workers
use that file; discovery uses ordinary joins within a single read transaction.
Writable connections start transactions with `BEGIN IMMEDIATE`, so catalog
read-then-write operations wait for worker writes instead of failing a stale
snapshot upgrade. Discovery keeps a separate read-only, deferred connection.
JSON snapshots, runtime WAL files, image blobs and display JSONL remain at their
existing paths. This change does not move message bodies into SQLite.

After the schema upgrade, startup reads the legacy `session_index.sqlite`
through a read-only SQLite attachment, including committed WAL contents. All
five tables and `metadata.catalog_session_import_v10=1` commit in one transaction.
Older indexes may lack optional tables/columns; SQL defaults fill missing
columns. A malformed row fails the whole import instead of silently skipping it.
An interrupted import rolls back and retries on startup; it does not require
resetting the schema journal. A completed import is never replayed, so the old
file cannot overwrite new data or resurrect deleted sessions. The legacy file is
retained unchanged as a pre-upgrade artifact and is no longer a live database.
Fresh installs create only `catalog.sqlite` for these two stores.

To rebuild a derived index, clear only its rows in `sessions`, `session_images`,
`display_items`, `display_index_files` and its bootstrap metadata as appropriate.
Never delete `catalog.sqlite`: it also contains authoritative project/workspace
organization. Preserve the `catalog_session_import_v10` marker during a rebuild.

`catalog_migrations` is golang-migrate's version/dirty journal. `schema_meta` remains
the compatibility version checked by old binaries/read-only clients, and changes
inside the same transaction as the corresponding schema change. A journal mismatch
or dirty migration fails startup; it is never silently reset with `Force`.

## Offline upgrade and rollback

Stop **all** daemons, CLI sessions and workers using the root, and take a consistent
backup of both existing databases and session files before upgrading. Include any SQLite WAL state, using SQLite's backup API
or a checkpoint with every writer stopped; do not copy only a live database file.

```sh
zotigod catalog-migrate --root /path/to/.zotigo --to 7
```

The command requires an existing catalog and an explicit target between 7 and the
binary's latest schema version. To upgrade explicitly, pass the latest version
(currently 10). Normal startup automatically upgrades; downgrade first, then launch
the matching older binary, otherwise startup will upgrade again.

Downgrading 8 to 7 discards project/workspace pins, their ordering, and the legacy
import marker. Existing session pins, session ordering, workspace paths, and
checkout ownership survive. Dropped data cannot be recovered by re-upgrading;
restore the backup when that data is needed. Versions below 7 require a historical
backup rather than invented reverse migrations.

Downgrading below 10 is refused if unified session tables contain data or a legacy
index backup exists. The generated down SQL can remove an empty schema, but is
not a reverse data export. To return to a pre-10 binary after real use, restore
the complete pre-upgrade backup with all processes stopped. Restoring loses
changes made after that backup; retaining the old index alone is not a rollback.

The workspace writer and offline migration command hold `catalog.lock`. Session
stores acquire it only when an upgrade/import is needed. Both workspace writers
and session stores hold a shared `schema.lock` until close; migrations require an
exclusive lease. Thus workers can open the current schema while the daemon runs,
but an offline migration cannot alter tables beneath a running worker. Older
releases do not participate in this shared lease protocol, and the oldest do not
take `catalog.lock` either: stopping all old processes before upgrade is mandatory.

If migration fails, that file's transaction rolls back and the migration journal
stays dirty. Startup then stops rather than guessing whether a crash happened
before or after commit. Restore a consistent backup with all daemons stopped,
fix the migration, and retry. Do not clear the dirty flag automatically.
