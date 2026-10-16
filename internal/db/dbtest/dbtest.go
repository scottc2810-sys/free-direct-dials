// Package dbtest provides a throwaway database for tests.
package dbtest

import (
	"context"
	"path/filepath"
	"testing"

	"markemind/dials/internal/db"
)

// Open returns a migrated database in a temp dir, closed on cleanup.
func Open(t testing.TB) *db.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(context.Background(), p, db.Options{Readers: 4, WriterCacheKB: 4096, ReaderCacheKB: 2048, MmapBytes: 16 << 20})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
