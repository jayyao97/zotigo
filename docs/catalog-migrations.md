# Catalog schema migrations

`core/workspace/schema.sql` declares the desired catalog structure. Atlas Community
1.3.1 computes its difference from the replayed migration history and generates
reviewable SQL. `golang-migrate` executes the embedded SQL inside zotigod; users do
not need Atlas installed, and generation never connects to their catalog.

## Development

Install Atlas **Community 1.3.1** (the `atlas-community-<platform>-v1.3.1` binaries
from atlasbinaries.com). The Linux amd64 binary used for validation has SHA-256
`521aafcdc4c81a6aee3d553adc97633821a7cb28076ffcdf4add27add930c804`.
Set `ATLAS=/path/to/atlas` if it is not on PATH.

1. Edit `core/workspace/schema.sql`.
2. Run `make catalog-diff NAME=describe_change`.
3. Review both generated `.up.sql` and `.down.sql` files. Add explicit data
   backfills when required: a schema diff cannot infer business data transforms.
4. After editing SQL, run
   `atlas migrate hash --dir 'file://core/workspace/migrations?format=golang-migrate'`.
5. Run `make catalog-check` and `go test ./core/workspace ./internal/zotigod`.
   Add data-preservation and rollback tests for the change.

The generator assigns the next sequential version, appends the `schema_meta`
version update to each direction, and updates `schemaVersion` in Go. Commit
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

`catalog_migrations` is golang-migrate's version/dirty journal. `schema_meta` remains
the compatibility version checked by old binaries/read-only clients, and changes
inside the same transaction as the corresponding schema change. A journal mismatch
or dirty migration fails startup; it is never silently reset with `Force`.

## Offline upgrade and rollback

Stop **all** daemons using the root, and take a consistent backup of the catalog
before changing versions. Include any SQLite WAL state, using SQLite's backup API
or a checkpoint with every writer stopped; do not copy only a live database file.

```sh
zotigod catalog-migrate --root /path/to/.zotigo --to 7
```

The command requires an existing catalog and an explicit target between 7 and the
binary's latest schema version. To upgrade explicitly, pass the latest version
(currently 9). Normal startup automatically upgrades; downgrade first, then launch
the matching older binary, otherwise startup will upgrade again.

Downgrading 8 to 7 discards project/workspace pins, their ordering, and the legacy
import marker. Existing session pins, session ordering, workspace paths, and
checkout ownership survive. Dropped data cannot be recovered by re-upgrading;
restore the backup when that data is needed. Versions below 7 require a historical
backup rather than invented reverse migrations.

New writable stores and this command hold `catalog.lock` for their whole lifetime,
so a new daemon prevents concurrent migration. **Old daemons do not acquire this
lock.** On the first transition from an old release, stopping old processes is
mandatory; acquiring this lock is not proof that an old daemon has stopped.

If migration fails, that file's transaction rolls back and the migration journal
stays dirty. Startup then stops rather than guessing whether a crash happened
before or after commit. Restore a consistent backup with all daemons stopped,
fix the migration, and retry. Do not clear the dirty flag automatically.
