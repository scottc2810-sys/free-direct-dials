package importer

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"markemind/dials/internal/db"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/normalize"
)

// RenormalizeDims re-applies normalize.Dim to every dictionary value of a SQLite
// store once per normalize.Version, merging values that now mean the same
// ("vice president" into "vp"). Contacts, spelling counts, labels and
// per-value counts follow. It returns how many values changed.
func RenormalizeDims(ctx context.Context, d *db.DB) (int, error) {
	if v, _ := strconv.Atoi(db.GetMeta(ctx, d.R, "norm_version")); v >= normalize.Version {
		return 0, nil
	}
	type row struct {
		id     int64
		dim    int
		v, raw string
	}
	var list []row
	rows, err := d.R.QueryContext(ctx, `SELECT id, dim, value_n, label FROM dims WHERE id > 12 ORDER BY id`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.dim, &r.v, &r.raw); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	changed := 0
	touched := map[int64]bool{}
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		for _, r := range list {
			if r.dim < 1 || r.dim > len(filter.Dims) {
				continue
			}
			key := filter.Dims[r.dim-1].Key
			n := normalize.Dim(key, r.raw) // the label is a raw spelling: list and noise rules apply to it
			if r.raw == "" {
				n = normalize.Dim(key, r.v)
			}
			if n == r.v {
				continue
			}
			changed++
			target := int64(r.dim) // blank
			if n != "" {
				err := tx.QueryRowContext(ctx, `SELECT id FROM dims WHERE dim=? AND value_n=?`, r.dim, n).Scan(&target)
				if errors.Is(err, sql.ErrNoRows) { // nothing to merge with: rename in place
					if _, err := tx.ExecContext(ctx, `UPDATE dims SET value_n=? WHERE id=?`, n, r.id); err != nil {
						return err
					}
					continue
				}
				if err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE contacts SET `+key+`=? WHERE `+key+`=?`, target, r.id); err != nil {
				return err
			}
			if target > 12 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO dim_spell(dim_id, raw, cnt) SELECT ?, raw, cnt FROM dim_spell WHERE dim_id=?
					ON CONFLICT(dim_id, raw) DO UPDATE SET cnt=dim_spell.cnt+excluded.cnt`, target, r.id); err != nil {
					return err
				}
				touched[target] = true
			}
			for _, q := range []string{`DELETE FROM dim_spell WHERE dim_id=?`, `DELETE FROM dims WHERE id=?`} {
				if _, err := tx.ExecContext(ctx, q, r.id); err != nil {
					return err
				}
			}
		}
		return db.SetMeta(ctx, tx, "norm_version", strconv.Itoa(normalize.Version))
	})
	if err != nil || changed == 0 {
		return changed, err
	}
	for id := range touched {
		if _, err := d.W.ExecContext(ctx, `UPDATE dims SET label = COALESCE((SELECT raw FROM dim_spell s
			WHERE s.dim_id = dims.id ORDER BY s.cnt DESC, s.raw ASC LIMIT 1), label) WHERE id=?`, id); err != nil {
			return changed, err
		}
	}
	return changed, Refresh(ctx, d)
}
