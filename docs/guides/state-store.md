# Guide: state store (`internal/state/store/`)

SQLite (default) and PostgreSQL behind one `Dialect`. Design background:
[`docs/design/state.md`](../design/state.md), concurrency rules:
[`docs/concurrency.md`](../concurrency.md).

## Migrations are `NNN_name.sql` and sort lexicographically

`migrationNames` sorts filenames with `sort.Strings`, then `migrationNumber`
parses the digits before the first `_`. Numbering is currently at `017`. A new
migration is `018_short_name.sql`.

> **Gotcha**: mixing widths — adding `18_sessions_flag.sql` next to the
> `017_…` files. `sort.Strings` does not order `18_…` where its number says
> (`019_…` still sorts before it), so a later three-digit file can be applied
> first and the two-digit one then skipped because its number is already below
> `schema_version`. Always zero-pad to three digits.

## Migration SQL runs unchanged on both engines

`runMigrations` executes the file verbatim inside a transaction. Only the
`schema_version` write is passed through `dialect.Rebind`. Placeholders in a
migration file are therefore **not** rewritten for PostgreSQL: write portable
DDL/DML and avoid `?`, `$1`, and engine-specific syntax (`AUTOINCREMENT`,
`SERIAL`, `json_each`).

> **Gotcha**: `INSERT INTO entities (id) VALUES (?)` inside a migration. It
> works on SQLite and fails on PostgreSQL with a syntax error at `?`.

## `schema_version` holds exactly one row

The runner deletes the existing row and inserts the new number in the same
transaction as the migration. Keep writes to `schema_version` out of
application code; touching it by hand makes migrations skip or re-run.

## `Dialect.Rebind` is not quote-aware

`Rebind` replaces every `?` with `$n` on PostgreSQL. A literal question mark in a
query — a string constant, a `LIKE '%?%'` pattern — is rewritten too and breaks
the statement.

> **Gotcha**: `WHERE question LIKE '%?%'` passing on SQLite and failing on
> PostgreSQL. Move the literal into the argument list or drop the `?`.

## Read-modify-write uses `BeginExclusive` and `ForUpdate`

Use `d.dialect.BeginExclusive(ctx, db)` for any read-then-write, deferring the
returned cleanup. On SQLite that is a dedicated connection with `BEGIN
IMMEDIATE`; on PostgreSQL a plain transaction, so every SELECT that the write
depends on must append `d.dialect.ForUpdate()`.

> **Gotcha**: `SELECT balance …` then `UPDATE balance …` in one transaction with
> no `FOR UPDATE`. Correct on SQLite (writers are serialised), an interleaving
> bug on PostgreSQL.

## SQLite pragmas live in the DSN and are verified

`journal_mode(WAL)` and `busy_timeout(5000)` are DSN parameters, so they apply to
every pooled connection, and `verifySQLitePragmas` asserts them at open time. A
`PRAGMA` issued with `Exec` hits one connection only and the pool keeps the
defaults.

> **Gotcha**: dropping `busy_timeout` from the DSN and calling
> `db.Exec("PRAGMA busy_timeout = 5000")` — the `SQLITE_BUSY` failures come back.

## Plugin databases have their own runner and version table

`RunPluginMigrations` opens `<data_dir>/plugin_data/<plugin>.db` (always SQLite,
regardless of the main driver) and applies `<pluginPath>/migrations/*.sql` in
sorted order against that database's own `schema_version`. A plugin migration is
not a main-DB migration: it never runs on PostgreSQL and never touches the main
schema.

> **Gotcha**: adding a host feature by dropping a `migrations/` folder into a
> plugin and expecting it to alter `state.db`. It lands in the plugin's private
> database and the host schema is unchanged.

## Advice

- Data fixes that cannot be expressed as DDL belong in a migration, not in
  `Open`; the one exception in the tree, `backfillSessionOwnership`, is gated on
  `prevVersion < 4` and must stay that way.
- New tables get their indexes in the same migration; an index added later means
  two migrations for one logical change.

## Specs

### `internal/state/store/migrations/NNN_*.sql`

- Filename is `NNN_snake_name.sql` with the next free three-digit number.
- Statement(s) run on both SQLite and PostgreSQL unchanged.
- Reversible or additive: no destructive `DROP` of a column still read by code.

### `internal/state/store/db.go`

- A migration needing data movement keeps it in the file itself, or gates the Go
  fix on `prevVersion < N`.
- New queries go through `d.dialect.Rebind(...)` before execution.

### `internal/state/store/store_test.go`

- A fresh-open test proves the migration applies on an empty database.
- A re-open test proves a second `Open` is a no-op (no re-run, no error).
