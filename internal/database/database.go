// Package database uses the existing Rails SQLite schema and explicit SQL.
package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schema string

var migrations = []string{"20231215043540", "20231220143106", "20240110071740", "20240115124901", "20240130003150", "20240130213001", "20240131105830", "20240209110503", "20250825100957", "20250825100958", "20250825100959", "20251126092013", "20251126115722", "20251126130131", "20251212154340"}

// A single writer prevents pool starvation while WAL readers proceed independently.
type DB struct {
	ResetConnections    func(int64)
	PurgeBlobs          func([]int64)
	RemoveBannedContent func(int64)
	Read                *readPool
	Write               *sql.DB
	Now                 func() time.Time
	version             versionReader
	memo                versionMemo
	observer            versionObserver
}

func Open(path string, readers int) (*DB, error) {
	if readers < 1 {
		return nil, errors.New("database readers must be positive")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	options := "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL&_cache_size=2000"
	// Reuse transaction statements on the single writer connection. The driver
	// resets bindings on reuse; results and authorization are never cached.
	w, err := sql.Open("sqlite3", uri+options+"&_txlock=immediate&_stmt_cache_size=64")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	fail := func(err error) (*DB, error) { w.Close(); return nil, err }
	if err = w.Ping(); err != nil {
		return fail(err)
	}
	if err = prepare(w); err != nil {
		return fail(err)
	}
	r, err := sql.Open("sqlite3", uri+options+"&mode=ro&_query_only=on")
	if err != nil {
		return fail(err)
	}
	r.SetMaxOpenConns(readers)
	r.SetMaxIdleConns(readers)
	if err = r.Ping(); err != nil {
		r.Close()
		return fail(err)
	}
	version, err := openVersionReader(uri + options + "&mode=ro&_query_only=on")
	if err != nil {
		r.Close()
		return fail(err)
	}
	now := time.Now
	if raw := os.Getenv("CAMPFIRE_FROZEN_TIME"); raw != "" {
		frozen, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			version.Close()
			r.Close()
			return fail(err)
		}
		now = func() time.Time { return frozen }
	}
	return &DB{Read: &readPool{DB: r, statements: make(map[string]*sql.Stmt)}, Write: w, Now: now, version: version}, nil
}
func (d *DB) Close() error {
	return errors.Join(d.Read.Close(), d.Write.Close(), d.version.Close())
}
func Stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000000") }
func (d *DB) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func prepare(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		var count int
		if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("refusing to initialize a nonempty database without schema_migrations")
		}
		if _, err = tx.Exec(schema); err != nil {
			return err
		}
		for i := len(migrations) - 1; i >= 0; i-- {
			if _, err = tx.Exec("INSERT INTO schema_migrations(version) VALUES (?)", migrations[i]); err != nil {
				return err
			}
		}
		now := Stamp(time.Now())
		for _, p := range [][2]string{{"environment", "production"}, {"schema_sha1", "f75da8dad38bfb179ffd757bd7a7c2b3f818bc29"}} {
			if _, err = tx.Exec("INSERT INTO ar_internal_metadata(key,value,created_at,updated_at) VALUES (?,?,?,?)", p[0], p[1], now, now); err != nil {
				return err
			}
		}
	}
	for _, v := range migrations {
		var found int
		if err = tx.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=?", v).Scan(&found); err != nil {
			return err
		}
		if found != 1 {
			return fmt.Errorf("pending migration %s: migrate with the reference app before starting", v)
		}
	}
	if _, err = tx.Exec("CREATE INDEX IF NOT EXISTS index_messages_on_room_id_and_created_at ON messages(room_id,created_at)"); err != nil {
		return err
	}
	if _, err = tx.Exec("CREATE INDEX IF NOT EXISTS index_messages_on_room_id_and_updated_at ON messages(room_id,updated_at)"); err != nil {
		return err
	}
	return tx.Commit()
}

// SQLite's DATETIME(6) declaration is returned as text by go-sqlite3.
type timestamp struct{ value *time.Time }

func (t timestamp) Scan(value any) error {
	if v, ok := value.(time.Time); ok {
		*t.value = v
		return nil
	}
	var raw string
	switch v := value.(type) {
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("invalid timestamp type %T", value)
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05.999999999-07:00", time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			*t.value = parsed
			return nil
		}
	}
	return fmt.Errorf("invalid timestamp %q", raw)
}

// A pinned reader observes commits from every writer, including this process.
// PRAGMA data_version is connection-local, so it must never use the read pool.
//
// Concurrent callers share one query: a caller needs a result from a query that
// started after it arrived, so it joins the next query rather than one already in
// flight. Every caller still observes all commits that finished before its call.
func (d *DB) ResponseVersion(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	o := &d.observer
	o.mu.Lock()
	if o.cond.L == nil {
		o.cond.L = &o.mu
	}
	target := o.started + 1
	for o.done < target {
		if o.running {
			o.cond.Wait()
			continue
		}
		o.running = true
		o.started = target
		o.mu.Unlock()
		version, err := d.version.query()
		o.mu.Lock()
		o.value, o.err, o.done, o.running = version, err, target, false
		o.cond.Broadcast()
	}
	version, err := o.value, o.err
	o.mu.Unlock()
	return version, err
}

type versionObserver struct {
	mu            sync.Mutex
	cond          sync.Cond
	started, done uint64
	running       bool
	value         uint64
	err           error
}

// versionReader owns one read-only SQLite connection for PRAGMA data_version. It talks to the
// driver directly: database/sql's locking and row allocation cost more than the pragma itself.
// Only the query's single runner (see ResponseVersion) calls query.
type versionReader struct {
	conn driver.Conn
	stmt driver.Stmt
	dest []driver.Value
}

func openVersionReader(dsn string) (versionReader, error) {
	conn, err := (&sqlite3.SQLiteDriver{}).Open(dsn)
	if err != nil {
		return versionReader{}, err
	}
	stmt, err := conn.Prepare("PRAGMA data_version")
	if err != nil {
		conn.Close()
		return versionReader{}, err
	}
	return versionReader{conn: conn, stmt: stmt, dest: make([]driver.Value, 1)}, nil
}

func (v versionReader) query() (uint64, error) {
	rows, err := v.stmt.Query(nil) //nolint:staticcheck // driver.Stmt.Query is the supported path without a context
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if err = rows.Next(v.dest); err != nil {
		return 0, err
	}
	n, ok := v.dest[0].(int64)
	if !ok {
		return 0, fmt.Errorf("unexpected data_version type %T", v.dest[0])
	}
	return uint64(n), nil
}

func (v versionReader) Close() error {
	return errors.Join(v.stmt.Close(), v.conn.Close())
}
