package db_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"markemind/dials/internal/db"
	"markemind/dials/internal/db/dbtest"
)

func TestMigrateIdempotentAndBlanks(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "x.db")
	d, err := db.Open(ctx, p, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	v1 := d.SchemaVersion(ctx)
	d.Close()
	d, err = db.Open(ctx, p, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v2 := d.SchemaVersion(ctx); v2 != v1 || v1 < 1 {
		t.Fatalf("schema version %d then %d", v1, v2)
	}
	var n int
	d.R.QueryRow(`SELECT count(*) FROM dims WHERE value_n='__blank__'`).Scan(&n)
	if n != 12 {
		t.Fatalf("want 12 blank dims, got %d", n)
	}
	var av int
	d.R.QueryRow(`PRAGMA auto_vacuum`).Scan(&av)
	if av != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental)", av)
	}
}

func TestReadersAreReadOnly(t *testing.T) {
	d := dbtest.Open(t)
	if _, err := d.R.Exec(`INSERT INTO meta(k,v) VALUES('x','y')`); err == nil {
		t.Fatal("reader pool accepted a write")
	}
}

func TestTxRollbackAndBump(t *testing.T) {
	ctx := context.Background()
	d := dbtest.Open(t)
	boom := errors.New("boom")
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		db.SetMeta(ctx, tx, "k", "v")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := db.GetMeta(ctx, d.R, "k"); got != "" {
		t.Fatalf("rollback failed, k=%q", got)
	}
	before := d.DatasetVersion()
	after, err := d.BumpDataset(ctx)
	if err != nil || after != before+1 || d.DatasetVersion() != after {
		t.Fatalf("bump: before=%d after=%d err=%v", before, after, err)
	}
}
