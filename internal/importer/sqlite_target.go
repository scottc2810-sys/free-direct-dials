package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"markemind/dials/internal/db"
	"markemind/dials/internal/filter"
)

// SQLiteTarget writes records into one SQLite contacts database (the
// built-in store or an extra SQLite file). One import at a time.
type SQLiteTarget struct {
	DB *db.DB

	bulk     *bulkSession
	dimCache map[int]map[string]int64 // dim index → value_n → id
	touched  map[int64]struct{}       // dim ids whose label may change
	spell    map[int64]map[string]int64
}

func (t *SQLiteTarget) Begin(ctx context.Context) error {
	t.dimCache, t.touched = map[int]map[string]int64{}, map[int64]struct{}{}
	b, err := beginBulk(ctx, t.DB)
	if err != nil {
		return err
	}
	t.bulk = b
	return nil
}

func (t *SQLiteTarget) Write(ctx context.Context, recs []*Record, now int64) (Counts, error) {
	var c Counts
	t.spell = map[int64]map[string]int64{}
	err := t.DB.Tx(ctx, func(tx *sql.Tx) error {
		s, err := prepare(ctx, tx)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if err := t.upsert(ctx, s, rec, now, &c); err != nil {
				return err
			}
		}
		for id, m := range t.spell {
			for raw, n := range m {
				if _, err := s.spell.ExecContext(ctx, id, raw, n); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return Counts{}, err
	}
	return c, nil
}

// End restores normal settings, settles labels and recomputes counts.
func (t *SQLiteTarget) End(ctx context.Context, changed int64) error {
	if t.bulk != nil {
		if err := t.bulk.end(ctx, changed); err != nil {
			return err
		}
		t.bulk = nil
	}
	return t.finalize(ctx)
}

// Abort is End for a failed or cancelled run: committed batches stay and the
// counts, caches and settings are made consistent with them.
func (t *SQLiteTarget) Abort(ctx context.Context, changed int64) {
	t.End(ctx, changed)
}

type stmts struct {
	byEmail, byLi, byH, load, label                          *sql.Stmt
	insC, insI, updC, updI, ftsDel, ftsIns, techDel, techIns *sql.Stmt
	dimGet, dimIns, spell                                    *sql.Stmt
}

func prepare(ctx context.Context, tx *sql.Tx) (*stmts, error) {
	s := &stmts{}
	dimCols := make([]string, len(filter.Dims))
	for i, d := range filter.Dims {
		dimCols[i] = d.Key
	}
	cols := strings.Join(dimCols, ",")
	sets := make([]string, len(dimCols))
	for i, c := range dimCols {
		sets[i] = c + "=?"
	}
	defs := []struct {
		dst *(*sql.Stmt)
		q   string
	}{
		{&s.byEmail, `SELECT id FROM contact_info WHERE email_key=?`},
		{&s.byLi, `SELECT id FROM contact_info WHERE li_key=?`},
		{&s.byH, `SELECT id FROM contact_info WHERE h_key=?`},
		{&s.load, `SELECT ` + prefixed("c.", dimCols) + `, i.email_key, i.li_key, i.h_key, i.name, i.title, i.company,
			i.website, i.linkedin, i.email, i.phone, i.extra FROM contacts c JOIN contact_info i ON i.id=c.id WHERE c.id=?`},
		{&s.label, `SELECT value_n FROM dims WHERE id=?`},
		{&s.insC, `INSERT INTO contacts(` + cols + `, has_email, has_phone) VALUES(` + db.Placeholders(len(dimCols)+2) + `)`},
		{&s.insI, `INSERT INTO contact_info(id, email_key, li_key, h_key, name, title, company, website, linkedin, email, phone, extra, updated)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`},
		{&s.updC, `UPDATE contacts SET ` + strings.Join(sets, ",") + `, has_email=?, has_phone=? WHERE id=?`},
		{&s.updI, `UPDATE contact_info SET email_key=?, li_key=?, h_key=?, name=?, title=?, company=?, website=?, linkedin=?,
			email=?, phone=?, extra=?, updated=? WHERE id=?`},
		{&s.ftsDel, `DELETE FROM fts WHERE rowid=?`},
		{&s.ftsIns, `INSERT INTO fts(rowid, txt) VALUES(?,?)`},
		{&s.techDel, `DELETE FROM tech_fts WHERE rowid=?`},
		{&s.techIns, `INSERT INTO tech_fts(rowid, txt) VALUES(?,?)`},
		{&s.dimGet, `SELECT id FROM dims WHERE dim=? AND value_n=?`},
		{&s.dimIns, `INSERT INTO dims(dim, value_n, label) VALUES(?,?,?) RETURNING id`},
		{&s.spell, `INSERT INTO dim_spell(dim_id, raw, cnt) VALUES(?,?,?) ON CONFLICT(dim_id, raw) DO UPDATE SET cnt=cnt+excluded.cnt`},
	}
	for _, d := range defs {
		st, err := tx.PrepareContext(ctx, d.q)
		if err != nil {
			return nil, fmt.Errorf("prepare %q: %w", d.q[:min(40, len(d.q))], err)
		}
		*d.dst = st
	}
	return s, nil
}

func prefixed(p string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = p + c
	}
	return strings.Join(out, ",")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (t *SQLiteTarget) dimID(ctx context.Context, s *stmts, i int, valueN, raw string) (int64, error) {
	if valueN == "" {
		return int64(i + 1), nil // blank ids 1..12
	}
	m := t.dimCache[i]
	if m == nil {
		m = map[string]int64{}
		t.dimCache[i] = m
	}
	id, ok := m[valueN]
	if !ok {
		err := s.dimGet.QueryRowContext(ctx, i+1, valueN).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = s.dimIns.QueryRowContext(ctx, i+1, valueN, raw).Scan(&id)
		}
		if err != nil {
			return 0, err
		}
		if len(m) < 200_000 {
			m[valueN] = id
		}
	}
	t.touched[id] = struct{}{}
	sp := t.spell[id]
	if sp == nil {
		sp = map[string]int64{}
		t.spell[id] = sp
	}
	sp[raw]++
	return id, nil
}

func find(ctx context.Context, s *stmts, r *Record) (int64, error) {
	try := func(st *sql.Stmt, k string) (int64, error) {
		if k == "" {
			return 0, nil
		}
		var id int64
		err := st.QueryRowContext(ctx, k).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return id, err
	}
	for _, p := range []struct {
		st *sql.Stmt
		k  string
	}{{s.byEmail, r.EmailKey}, {s.byLi, r.LiKey}, {s.byH, r.HKey}} {
		id, err := try(p.st, p.k)
		if err != nil || id != 0 {
			return id, err
		}
	}
	return 0, nil
}

func (t *SQLiteTarget) upsert(ctx context.Context, s *stmts, r *Record, now int64, c *Counts) error {
	var dims [12]int64
	for i := range filter.Dims {
		id, err := t.dimID(ctx, s, i, r.DimN[i], r.DimRaw[i])
		if err != nil {
			return err
		}
		dims[i] = id
	}
	id, err := find(ctx, s, r)
	if err != nil {
		return err
	}
	if id == 0 {
		args := make([]any, 0, 14)
		for _, d := range dims {
			args = append(args, d)
		}
		args = append(args, b2i(r.Email != ""), b2i(r.Phone != ""))
		res, err := s.insC.ExecContext(ctx, args...)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := s.insI.ExecContext(ctx, id, nullable(r.EmailKey), nullable(r.LiKey), nullable(r.HKey), r.Name, r.Title,
			r.Company, r.Website, r.LinkedIn, r.Email, r.Phone, EncodeExtra(r.Extra), now); err != nil {
			return err
		}
		if _, err := s.ftsIns.ExecContext(ctx, id, FTSText(r.Name, r.Title, r.Company, r.DimN[3], r.DimN[4], r.Extra)); err != nil {
			return err
		}
		if err := writeTech(ctx, s, id, r.Tech, false); err != nil {
			return err
		}
		c.Added++
		return nil
	}
	return t.merge(ctx, s, id, r, dims, now, c)
}

func (t *SQLiteTarget) merge(ctx context.Context, s *stmts, id int64, r *Record, dims [12]int64, now int64, c *Counts) error {
	var cur Stored
	var ek, lk, hk sql.NullString
	var extraBlob []byte
	dst := make([]any, 0, 24)
	for i := range cur.Dims {
		dst = append(dst, &cur.Dims[i])
	}
	dst = append(dst, &ek, &lk, &hk, &cur.Name, &cur.Title, &cur.Company, &cur.Website, &cur.LinkedIn, &cur.Email, &cur.Phone, &extraBlob)
	if err := s.load.QueryRowContext(ctx, id).Scan(dst...); err != nil {
		return err
	}
	cur.EmailKey, cur.LiKey, cur.HKey, cur.Extra = ek.String, lk.String, hk.String, DecodeExtra(extraBlob)
	changed, techChanged, err := Merge(&cur, r, dims, func(kind byte, key string) (bool, error) {
		st := map[byte]*sql.Stmt{'e': s.byEmail, 'l': s.byLi, 'h': s.byH}[kind]
		var owner int64
		err := st.QueryRowContext(ctx, key).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return owner == id, err
	})
	if err != nil {
		return err
	}
	if !changed {
		c.Unchanged++
		return nil
	}
	args := make([]any, 0, 15)
	for _, d := range cur.Dims {
		args = append(args, d)
	}
	args = append(args, b2i(cur.Email != ""), b2i(cur.Phone != ""), id)
	if _, err := s.updC.ExecContext(ctx, args...); err != nil {
		return err
	}
	if _, err := s.updI.ExecContext(ctx, nullable(cur.EmailKey), nullable(cur.LiKey), nullable(cur.HKey), cur.Name, cur.Title, cur.Company,
		cur.Website, cur.LinkedIn, cur.Email, cur.Phone, EncodeExtra(cur.Extra), now, id); err != nil {
		return err
	}
	ind, err := valueN(ctx, s, cur.Dims[3])
	if err != nil {
		return err
	}
	sec, err := valueN(ctx, s, cur.Dims[4])
	if err != nil {
		return err
	}
	if _, err := s.ftsDel.ExecContext(ctx, id); err != nil {
		return err
	}
	if _, err := s.ftsIns.ExecContext(ctx, id, FTSText(cur.Name, cur.Title, cur.Company, ind, sec, cur.Extra)); err != nil {
		return err
	}
	if techChanged {
		if err := writeTech(ctx, s, id, r.Tech, true); err != nil {
			return err
		}
	}
	c.Updated++
	return nil
}

func valueN(ctx context.Context, s *stmts, id int64) (string, error) {
	if id <= 12 {
		return "", nil
	}
	var v string
	err := s.label.QueryRowContext(ctx, id).Scan(&v)
	return v, err
}

func writeTech(ctx context.Context, s *stmts, id int64, tech []string, replace bool) error {
	if replace {
		if _, err := s.techDel.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	if len(tech) == 0 {
		return nil
	}
	_, err := s.techIns.ExecContext(ctx, id, strings.Join(tech, " "))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (t *SQLiteTarget) finalize(ctx context.Context) error {
	ids := make([]int64, 0, len(t.touched))
	for id := range t.touched {
		ids = append(ids, id)
	}
	t.touched = map[int64]struct{}{}
	for len(ids) > 0 {
		n := min(500, len(ids))
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}
		if _, err := t.DB.W.ExecContext(ctx, `UPDATE dims SET label = COALESCE((SELECT raw FROM dim_spell s
			WHERE s.dim_id = dims.id ORDER BY s.cnt DESC, s.raw ASC LIMIT 1), label) WHERE id IN (`+db.Placeholders(n)+`)`, args...); err != nil {
			return err
		}
	}
	return Refresh(ctx, t.DB)
}
