package importer

import (
	"context"
	"fmt"

	"markemind/dials/internal/db"
)

// bulkSession tunes the writer for a large import and undoes it afterwards.
//
//   - A bigger page cache (128 MB) for the import only: the hot B-tree pages of the
//     identity indexes, FTS and dictionaries stay in memory instead of being re-read
//     on each random insert.
//   - Fewer WAL checkpoints (every 64 MB instead of 4 MB), then one TRUNCATE at the end.
//   - FTS5 automatic merging is paused and replaced by a single 'optimize' merge at the end.
//   - On the very first load (empty table) the 12 filter indexes are dropped and rebuilt
//     once from sorted data, which is several times faster than maintaining them row by row.
//     db.Open re-creates them on start, so a crash mid-import can't leave them missing.
type bulkSession struct {
	d          *db.DB
	dropped    bool
	ftsTouched bool
}

const (
	bulkCacheKB    = 128 * 1024
	bulkCheckpoint = 16384 // pages
)

func beginBulk(ctx context.Context, d *db.DB) (*bulkSession, error) {
	b := &bulkSession{d: d}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA cache_size=-%d`, bulkCacheKB),
		fmt.Sprintf(`PRAGMA wal_autocheckpoint=%d`, bulkCheckpoint),
		`INSERT INTO fts(fts, rank) VALUES('automerge', 0)`,
		`INSERT INTO tech_fts(tech_fts, rank) VALUES('automerge', 0)`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			b.end(context.WithoutCancel(ctx), 0)
			return nil, err
		}
	}
	b.ftsTouched = true
	var exists int
	if err := d.W.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contacts)`).Scan(&exists); err != nil {
		b.end(context.WithoutCancel(ctx), 0)
		return nil, err
	}
	if exists == 0 {
		for name := range db.FilterIndexes() {
			if _, err := d.W.ExecContext(ctx, `DROP INDEX IF EXISTS `+name); err != nil {
				b.end(context.WithoutCancel(ctx), 0)
				return nil, err
			}
		}
		b.dropped = true
	}
	return b, nil
}

// end restores normal settings; rows is the number of rows written (merge only when worth it).
func (b *bulkSession) end(ctx context.Context, rows int64) error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	if b.dropped {
		keep(db.EnsureIndexes(ctx, b.d.W))
	}
	if b.ftsTouched {
		for _, t := range []string{"fts", "tech_fts"} {
			_, err := b.d.W.ExecContext(ctx, `INSERT INTO `+t+`(`+t+`, rank) VALUES('automerge', 4)`)
			keep(err)
			if rows >= 20_000 {
				_, err = b.d.W.ExecContext(ctx, `INSERT INTO `+t+`(`+t+`) VALUES('optimize')`)
				keep(err)
			}
		}
	}
	_, err := b.d.W.ExecContext(ctx, fmt.Sprintf(`PRAGMA cache_size=-%d`, b.d.WriterCacheKB()))
	keep(err)
	_, err = b.d.W.ExecContext(ctx, `PRAGMA wal_autocheckpoint=1000`)
	keep(err)
	_, err = b.d.W.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	keep(err)
	return first
}
