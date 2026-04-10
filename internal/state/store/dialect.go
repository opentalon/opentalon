package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Dialect encapsulates SQL differences between SQLite and PostgreSQL.
type Dialect struct{ name string }

var (
	SQLiteDialect   = Dialect{"sqlite"}
	PostgresDialect = Dialect{"postgres"}
)

// Rebind converts ? placeholders to $1, $2, … for PostgreSQL; no-op for SQLite.
func (d Dialect) Rebind(query string) string {
	if d.name != "postgres" {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

// TagMatch returns the dialect-appropriate SQL fragment for "JSON array column contains value".
// The fragment uses a single ? placeholder (rebind it after building the full query).
//
//	SQLite:   EXISTS (SELECT 1 FROM json_each(col) WHERE json_each.value = ?)
//	Postgres: EXISTS (SELECT 1 FROM json_array_elements_text(col::json) AS _t WHERE _t = ?)
func (d Dialect) TagMatch(column string) string {
	if d.name == "postgres" {
		return fmt.Sprintf("EXISTS (SELECT 1 FROM json_array_elements_text(%s::json) AS _t WHERE _t = ?)", column)
	}
	return fmt.Sprintf("EXISTS (SELECT 1 FROM json_each(%s) WHERE json_each.value = ?)", column)
}

// ExclusiveTx is a write-serialised transaction for read-modify-write operations.
// Both *sql.Tx (postgres) and sqliteConnTx (sqlite BEGIN IMMEDIATE) satisfy this interface.
type ExclusiveTx interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Commit() error
	Rollback() error
}

// BeginExclusive starts a serialised write transaction.
// The returned cleanup func must be deferred by the caller to release connection resources.
//
//	SQLite:   dedicated Conn + BEGIN IMMEDIATE (blocks concurrent writers at start)
//	Postgres: db.BeginTx with LevelSerializable
func (d Dialect) BeginExclusive(ctx context.Context, db *sql.DB) (ExclusiveTx, func(), error) {
	if d.name == "postgres" {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return nil, func() {}, fmt.Errorf("begin exclusive: %w", err)
		}
		return tx, func() {}, nil
	}
	// SQLite: use a reserved connection so BEGIN IMMEDIATE serialises concurrent writers.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, func() {}, fmt.Errorf("begin exclusive conn: %w", err)
	}
	cleanup := func() { _ = conn.Close() }
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("begin immediate: %w", err)
	}
	return &sqliteConnTx{conn: conn, ctx: ctx}, cleanup, nil
}

// sqliteConnTx wraps a *sql.Conn that has an open BEGIN IMMEDIATE transaction.
type sqliteConnTx struct {
	conn *sql.Conn
	ctx  context.Context
}

func (c *sqliteConnTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.conn.QueryRowContext(ctx, query, args...)
}

func (c *sqliteConnTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(ctx, query, args...)
}

func (c *sqliteConnTx) Commit() error {
	_, err := c.conn.ExecContext(c.ctx, "COMMIT")
	return err
}

func (c *sqliteConnTx) Rollback() error {
	_, err := c.conn.ExecContext(c.ctx, "ROLLBACK")
	return err
}
