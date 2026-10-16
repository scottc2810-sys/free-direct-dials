package store

import (
	"context"
	"database/sql"
	"strings"

	"markemind/dials/internal/db"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/search"
)

// SQLite is a contacts store in a SQLite file: the built-in database (which
// also holds the control plane) or an extra file. Reads go through the tuned
// search engine (covering indexes, FTS5, dictionary caches, load shedding).
type SQLite struct {
	DB   *db.DB
	S    *search.Service
	kind string
	own  bool // close DB on Close (extra files only)
}

// NewSQLite wraps an open database. local marks the built-in one.
func NewSQLite(d *db.DB, local bool) *SQLite {
	k := KindSQLite
	if local {
		k = KindLocal
	}
	return &SQLite{DB: d, S: search.New(d, nil, 4), kind: k, own: !local}
}

// OpenSQLite opens (creating if needed) an extra SQLite store.
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	d, err := db.Open(ctx, path, db.Options{Readers: 2, WriterCacheKB: 8 * 1024, ReaderCacheKB: 8 * 1024})
	if err != nil {
		return nil, err
	}
	return NewSQLite(d, false), nil
}

func (s *SQLite) Kind() string   { return s.kind }
func (s *SQLite) Version() int64 { return s.DB.DatasetVersion() }

func (s *SQLite) Count(ctx context.Context, f filter.Filter) (int64, error) {
	r, err := s.S.Resolve(ctx, f)
	if err != nil {
		return 0, err
	}
	return s.S.Count(ctx, r)
}

func (s *SQLite) Facet(ctx context.Context, f filter.Filter, dim, q string, limit int) ([]FacetCount, bool, error) {
	r, err := s.S.Resolve(ctx, f)
	if err != nil {
		return nil, false, err
	}
	res, err := s.S.Facets(ctx, r, dim, q)
	if err != nil {
		return nil, false, err
	}
	out := make([]FacetCount, len(res.Values))
	for i, v := range res.Values {
		out[i] = FacetCount{Value: v.Value, Label: v.Label, Count: v.Count}
	}
	return out, res.Truncated, nil
}

func (s *SQLite) Page(ctx context.Context, f filter.Filter, offset, limit int) ([]search.Row, error) {
	r, err := s.S.Resolve(ctx, f)
	if err != nil {
		return nil, err
	}
	return s.S.Page(ctx, r, offset, limit)
}

func (s *SQLite) Stats(ctx context.Context) (importer.HeadlineStats, error) {
	return importer.LoadStats(ctx, s.DB.R), nil
}

func (s *SQLite) Values(ctx context.Context, dim string, minCount, limit int) ([]FacetCount, error) {
	d, ok := filter.DimByKey(dim)
	if !ok {
		return nil, nil
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT value_n, label, cnt FROM dims WHERE dim=? AND id>12 AND cnt>=? ORDER BY cnt DESC, id LIMIT ?`,
		d.ID, max(minCount, 1), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FacetCount
	for rows.Next() {
		var v FacetCount
		if err := rows.Scan(&v.Value, &v.Label, &v.Count); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *SQLite) Combos(ctx context.Context, minCount, limit int) ([]Combo, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT dc.value_n, di.value_n, g.n FROM (SELECT country, industry, COUNT(*) AS n FROM contacts
		WHERE country>12 AND industry>12 GROUP BY country, industry HAVING n>=? ORDER BY n DESC LIMIT ?) g
		JOIN dims dc ON dc.id=g.country JOIN dims di ON di.id=g.industry ORDER BY g.n DESC`, max(minCount, 1), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Combo
	for rows.Next() {
		var c Combo
		if err := rows.Scan(&c.Country, &c.Industry, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLite) Labels(ctx context.Context, dim string, values []string) (map[string]string, error) {
	out := map[string]string{}
	d, ok := filter.DimByKey(dim)
	if !ok || len(values) == 0 {
		return out, nil
	}
	for len(values) > 0 {
		n := min(400, len(values))
		args := []any{d.ID}
		for _, v := range values[:n] {
			args = append(args, v)
		}
		values = values[n:]
		rows, err := s.DB.R.QueryContext(ctx, `SELECT value_n, label FROM dims WHERE dim=? AND value_n IN (`+db.Placeholders(n)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v, l string
			if err := rows.Scan(&v, &l); err != nil {
				rows.Close()
				return nil, err
			}
			out[v] = l
		}
		rows.Close()
	}
	return out, nil
}

func (s *SQLite) Dims(ctx context.Context) ([]DimCount, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT dim, value_n, label, cnt FROM dims WHERE cnt>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DimCount
	for rows.Next() {
		var d DimCount
		if err := rows.Scan(&d.Dim, &d.Value, &d.Label, &d.Count); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *SQLite) UnlockSummary(ctx context.Context) ([]UserUnlocks, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT user_id, COUNT(*), MIN(batch), MAX(batch) FROM unlocked GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserUnlocks
	for rows.Next() {
		var u UserUnlocks
		if err := rows.Scan(&u.UserID, &u.N, &u.MinBatch, &u.MaxBatch); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *SQLite) Claim(ctx context.Context, uid, batch int64, f filter.Filter, n int, now int64) ([]search.Row, error) {
	r, err := s.S.Resolve(ctx, f)
	if err != nil {
		return nil, err
	}
	if r.Impossible() || n <= 0 {
		return []search.Row{}, nil
	}
	where, args := r.Where("")
	var ids []int64
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		ids, err = search.QueryIDs(ctx, tx, `SELECT c.id FROM contacts c WHERE `+where+`
			AND NOT EXISTS (SELECT 1 FROM unlocked u WHERE u.user_id=? AND u.contact_id=c.id) ORDER BY c.id LIMIT ?`, append(args, uid, n)...)
		if err != nil || len(ids) == 0 {
			return err
		}
		vals := make([]string, len(ids))
		iargs := make([]any, 0, len(ids)*4)
		for i, id := range ids {
			vals[i] = "(?,?,?,?)"
			iargs = append(iargs, uid, id, batch, now)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO unlocked(user_id, contact_id, batch, ts) VALUES `+strings.Join(vals, ","), iargs...)
		return err
	})
	if err != nil || len(ids) == 0 {
		return []search.Row{}, err
	}
	rows, err := s.S.FetchRows(ctx, s.DB.R, ids, false)
	for i := range rows {
		rows[i].Batch, rows[i].New = batch, true
	}
	return rows, err
}

func (s *SQLite) Unlocked(ctx context.Context, uid int64, f filter.Filter) (int64, error) {
	var n int64
	if f.IsEmpty() {
		err := s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM unlocked WHERE user_id=?`, uid).Scan(&n)
		return n, err
	}
	r, err := s.S.Resolve(ctx, f)
	if err != nil || r.Impossible() {
		return 0, err
	}
	where, args := r.Where("")
	err = s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM unlocked u JOIN contacts c ON c.id=u.contact_id WHERE u.user_id=? AND `+where,
		append([]any{uid}, args...)...).Scan(&n)
	return n, err
}

func (s *SQLite) ViewPage(ctx context.Context, uid int64, f filter.Filter, after Cursor, self int, onlyBatch int64, limit int, withExtra bool) ([]search.Row, error) {
	r, err := s.S.Resolve(ctx, f)
	if err != nil {
		return nil, err
	}
	if r.Impossible() || limit <= 0 {
		return []search.Row{}, nil
	}
	where, wargs := r.Where("")
	q := `SELECT u.contact_id, u.batch FROM unlocked u JOIN contacts c ON c.id=u.contact_id WHERE u.user_id=? AND ` + where
	args := append([]any{uid}, wargs...)
	if onlyBatch > 0 {
		q += ` AND u.batch=?`
		args = append(args, onlyBatch)
	}
	if cond := after.cond(self, "u.batch", "u.contact_id", func(v any) string { args = append(args, v); return "?" }); cond != "" {
		q += " AND " + cond
	}
	q += ` ORDER BY u.batch DESC, u.contact_id LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	batches := map[int64]int64{}
	for rows.Next() {
		var id, b int64
		if err := rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		batches[id] = b
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out, err := s.S.FetchRows(ctx, s.DB.R, ids, withExtra)
	for i := range out {
		out[i].Batch = batches[out[i].ID]
	}
	return out, err
}

func (s *SQLite) ForgetUser(ctx context.Context, uid int64) error {
	_, err := s.DB.W.ExecContext(ctx, `DELETE FROM unlocked WHERE user_id=?`, uid)
	return err
}

func (s *SQLite) Target() importer.Target { return &importer.SQLiteTarget{DB: s.DB} }

func (s *SQLite) ExistingKeys(ctx context.Context, email, li, h []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, set := range []struct {
		col, pre string
		keys     []string
	}{{"email_key", "e:", email}, {"li_key", "l:", li}, {"h_key", "h:", h}} {
		keys := set.keys
		for len(keys) > 0 {
			n := min(500, len(keys))
			args := make([]any, n)
			for i, k := range keys[:n] {
				args[i] = k
			}
			keys = keys[n:]
			rows, err := s.DB.R.QueryContext(ctx, `SELECT `+set.col+` FROM contact_info WHERE `+set.col+` IN (`+db.Placeholders(n)+`)`, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var k string
				if err := rows.Scan(&k); err != nil {
					rows.Close()
					return nil, err
				}
				out[set.pre+k] = true
			}
			rows.Close()
		}
	}
	return out, nil
}

func (s *SQLite) FindByIdentity(ctx context.Context, kind, value string) ([]int64, error) {
	return importer.FindByIdentity(ctx, s.DB.R, kind, value)
}

func (s *SQLite) SuppressKeys(ctx context.Context, ids []int64) ([]string, error) {
	return importer.ContactKeys(ctx, s.DB.R, ids)
}

func (s *SQLite) Delete(ctx context.Context, ids []int64) (int, error) {
	return importer.DeleteContacts(ctx, s.DB, ids)
}

func (s *SQLite) Prune(ctx context.Context) error {
	_, err := s.DB.W.ExecContext(ctx, `DELETE FROM unlocked WHERE contact_id NOT IN (SELECT id FROM contacts)`)
	return err
}

func (s *SQLite) Ping(ctx context.Context) error {
	var one int
	return s.DB.R.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

func (s *SQLite) Keepalive(ctx context.Context, now int64) error { return s.Ping(ctx) }

func (s *SQLite) Size(ctx context.Context) (int64, error) {
	var pages, size int64
	if err := s.DB.R.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.DB.R.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}

func (s *SQLite) Close() error {
	if s.own {
		return s.DB.Close()
	}
	return nil
}
