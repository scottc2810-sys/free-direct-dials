// Package db opens the SQLite database as one writer connection plus a small
// pool of read-only connections, and applies embedded migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

type Options struct {
	Readers       int   // reader pool size (default 4)
	WriterCacheKB int   // page cache for the writer connection (default 32 MB)
	ReaderCacheKB int   // page cache per reader connection (default 16 MB)
	MmapBytes     int64 // shared mmap window (default 128 MB)
}

type DB struct {
	W    *sql.DB // exactly one connection: all writes serialize here
	R    *sql.DB // read-only pool
	Path string

	version atomic.Int64 // cached dataset_version
	wcache  int          // writer page cache (KB), restored after bulk imports
}

// WriterCacheKB is the writer connection's normal page-cache size.
func (d *DB) WriterCacheKB() int { return d.wcache }

var dimIndexCols = []string{"country", "state", "city", "industry", "sector", "ctype", "seniority", "division", "decision", "csize", "revenue", "econf"}

// FilterIndexes are the contacts indexes, measured at 1 M rows (docs/PERFORMANCE.md):
//   - one per dimension, carrying has_email/has_phone so "has email/phone" filters and
//     facets are answered from the index alone (facet under flags: 1.1 s → 0.12 s);
//   - composites for the pairs visitors combine most (country × industry, country × region),
//     which turn 2-filter counts into covering range scans (126 ms → 3 ms).
func FilterIndexes() map[string]string {
	m := map[string]string{}
	for _, c := range dimIndexCols {
		m["c_"+c] = c + ", has_email, has_phone"
	}
	m["c_country_industry"] = "country, industry, has_email, has_phone"
	m["c_industry_country"] = "industry, country, has_email, has_phone"
	m["c_country_state"] = "country, state, has_email, has_phone"
	m["c_country_city"] = "country, city, has_email, has_phone"
	return m
}

// EnsureIndexes (re)creates the filter indexes. Bulk imports drop them on an
// empty table and rebuild them from sorted data; running this at every start
// makes a crash mid-import harmless.
func EnsureIndexes(ctx context.Context, q Querier) error {
	for name, cols := range FilterIndexes() {
		if _, err := q.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS `+name+` ON contacts(`+cols+`)`); err != nil {
			return err
		}
	}
	return nil
}

func (o *Options) defaults() {
	if o.Readers <= 0 {
		o.Readers = 4
	}
	if o.WriterCacheKB <= 0 {
		o.WriterCacheKB = 32 * 1024
	}
	if o.ReaderCacheKB <= 0 {
		o.ReaderCacheKB = 16 * 1024
	}
	if o.MmapBytes <= 0 {
		o.MmapBytes = 128 << 20
	}
}

func dsn(path string, cacheKB int, mmap int64, readOnly bool) string {
	p := []string{
		"_pragma=auto_vacuum(INCREMENTAL)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=secure_delete(FAST)",          // erased contacts are zeroed in-page, not left in free space
		"_pragma=journal_size_limit(67108864)", // WAL file shrinks back to 64 MB after checkpoints
		fmt.Sprintf("_pragma=cache_size(-%d)", cacheKB),
		fmt.Sprintf("_pragma=mmap_size(%d)", mmap),
		"_txlock=immediate",
	}
	if readOnly {
		p = append(p, "_pragma=query_only(1)")
	}
	return "file:" + filepath.ToSlash(path) + "?" + strings.Join(p, "&")
}

// Open opens (creating if needed) the database and runs migrations.
func Open(ctx context.Context, path string, o Options) (*DB, error) {
	o.defaults()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(path, o.WriterCacheKB, o.MmapBytes, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.PingContext(ctx); err != nil {
		w.Close()
		return nil, fmt.Errorf("open writer: %w", err)
	}
	d := &DB{W: w, Path: path, wcache: o.WriterCacheKB}
	if err := d.migrate(ctx); err != nil {
		w.Close()
		return nil, err
	}
	if err := EnsureIndexes(ctx, w); err != nil {
		w.Close()
		return nil, err
	}
	// a bulk import pauses FTS merging; restore the default in case one crashed
	w.ExecContext(ctx, `INSERT INTO fts(fts, rank) VALUES('automerge', 4)`)
	w.ExecContext(ctx, `INSERT INTO tech_fts(tech_fts, rank) VALUES('automerge', 4)`)
	restrictFiles(path)
	r, err := sql.Open("sqlite", dsn(path, o.ReaderCacheKB, o.MmapBytes, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(o.Readers)
	r.SetMaxIdleConns(o.Readers)
	r.SetConnMaxLifetime(0)
	d.R = r
	if err := d.loadVersion(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// OpenAux opens a separate single connection (used for backups so the writer
// is never blocked by a long VACUUM INTO).
func OpenAux(path string) (*sql.DB, error) {
	a, err := sql.Open("sqlite", dsn(path, 8*1024, 0, false))
	if err != nil {
		return nil, err
	}
	a.SetMaxOpenConns(1)
	return a, nil
}

func (d *DB) Close() error {
	var errs []error
	if d.R != nil {
		errs = append(errs, d.R.Close())
	}
	if d.W != nil {
		// best effort: fold the WAL back so the next start is fast
		d.W.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		errs = append(errs, d.W.Close())
	}
	return errors.Join(errs...)
}

// Tx runs fn inside a BEGIN IMMEDIATE transaction on the writer connection.
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(tx)
}

func (d *DB) migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	current := 0
	var v string
	if err := d.W.QueryRowContext(ctx, `SELECT v FROM meta WHERE k='schema_version'`).Scan(&v); err == nil {
		current, _ = strconv.Atoi(v)
	}
	for _, e := range entries {
		num, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad name", e.Name())
		}
		if num <= current {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		err = d.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", e.Name(), err)
			}
			return SetMeta(ctx, tx, "schema_version", strconv.Itoa(num))
		})
		if err != nil {
			return err
		}
		current = num
	}
	return nil
}

// SchemaVersion returns the applied migration number.
func (d *DB) SchemaVersion(ctx context.Context) int {
	n, _ := strconv.Atoi(GetMeta(ctx, d.R, "schema_version"))
	return n
}

func GetMeta(ctx context.Context, q Querier, k string) string {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT v FROM meta WHERE k=?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

func SetMeta(ctx context.Context, q Querier, k, v string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (d *DB) loadVersion(ctx context.Context) error {
	n, err := strconv.ParseInt(GetMeta(ctx, d.W, "dataset_version"), 10, 64)
	if err != nil {
		n = 1
	}
	d.version.Store(n)
	return nil
}

// DatasetVersion changes whenever contact data changes; it is part of every
// cache key so imports and removals invalidate caches instantly.
func (d *DB) DatasetVersion() int64 { return d.version.Load() }

// BumpDataset increments dataset_version (call after commit of a data change).
func (d *DB) BumpDataset(ctx context.Context) (int64, error) {
	var n int64
	err := d.W.QueryRowContext(ctx,
		`UPDATE meta SET v = CAST(CAST(v AS INTEGER)+1 AS TEXT) WHERE k='dataset_version' RETURNING CAST(v AS INTEGER)`).Scan(&n)
	if err != nil {
		return 0, err
	}
	d.version.Store(n)
	return n, nil
}

// restrictFiles makes the database, WAL and shared-memory files owner-only
// (no-op on Windows). Personal data must not be world-readable on the host.
func restrictFiles(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			os.Chmod(p, 0o600)
		}
	}
	os.Chmod(filepath.Dir(path), 0o700)
}

// Placeholders returns "?,?,?" for n parameters.
func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
