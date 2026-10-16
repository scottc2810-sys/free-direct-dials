package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
)

// pgTarget writes import batches to a Postgres store with a handful of
// set-based statements per batch (round trips dominate over the internet):
// dictionary upsert, spelling counts, one lookup of existing identities,
// an in-memory merge with exactly the SQLite store's rules, then bulk
// UPDATE and INSERT from arrays. Unique identity keys are checked at commit.
type pgTarget struct {
	p       *Postgres
	dimIDs  map[dimKey]int64 // (dimension index, value_n) → id
	valueOf map[int64]string // id → value_n
	touched map[int64]struct{}
}

type dimKey struct {
	i int
	v string
}

type spellKey struct {
	id  int64
	raw string
}

type pgEntry struct {
	st        importer.Stored
	isNew     bool
	dirty     bool
	techDirty bool
	tech      []string
}

func (t *pgTarget) Begin(ctx context.Context) error {
	t.dimIDs, t.valueOf, t.touched = map[dimKey]int64{}, map[int64]string{}, map[int64]struct{}{}
	return nil
}

func (t *pgTarget) Write(ctx context.Context, recs []*importer.Record, now int64) (importer.Counts, error) {
	var c importer.Counts
	if len(recs) == 0 {
		return c, nil
	}
	if len(t.dimIDs) > 300_000 { // bound memory on huge, high-cardinality files
		t.dimIDs, t.valueOf = map[dimKey]int64{}, map[int64]string{}
	}
	tx, err := t.p.pool.Begin(ctx)
	if err != nil {
		return c, err
	}
	defer tx.Rollback(ctx)

	// 1. dictionary values
	var nd []int16
	var nv, nl []string
	want := map[dimKey]bool{}
	for _, r := range recs {
		for i := range filter.Dims {
			v := r.DimN[i]
			k := dimKey{i, v}
			if v == "" || want[k] {
				continue
			}
			if _, ok := t.dimIDs[k]; ok {
				continue
			}
			want[k] = true
			nd, nv, nl = append(nd, int16(i+1)), append(nv, v), append(nl, r.DimRaw[i])
		}
	}
	if len(nd) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO fdd.dims(dim, value_n, label) SELECT * FROM unnest($1::smallint[], $2::text[], $3::text[])
			ON CONFLICT (dim, value_n) DO NOTHING`, nd, nv, nl); err != nil {
			return c, err
		}
		rows, err := tx.Query(ctx, `SELECT d.dim, d.value_n, d.id FROM fdd.dims d
			JOIN unnest($1::smallint[], $2::text[]) AS q(dim, v) ON d.dim = q.dim AND d.value_n = q.v`, nd, nv)
		if err != nil {
			return c, err
		}
		for rows.Next() {
			var dim int16
			var v string
			var id int64
			if err := rows.Scan(&dim, &v, &id); err != nil {
				rows.Close()
				return c, err
			}
			t.dimIDs[dimKey{int(dim) - 1, v}] = id
			t.valueOf[id] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return c, err
		}
	}
	dimsOf := func(r *importer.Record) [12]int64 {
		var d [12]int64
		for i := range filter.Dims {
			if r.DimN[i] == "" {
				d[i] = int64(i + 1)
			} else {
				d[i] = t.dimIDs[dimKey{i, r.DimN[i]}]
			}
		}
		return d
	}

	// 2. spelling counts (the most common spelling becomes the label)
	spell := map[spellKey]int64{}
	for _, r := range recs {
		d := dimsOf(r)
		for i := range filter.Dims {
			if r.DimN[i] != "" {
				spell[spellKey{d[i], r.DimRaw[i]}]++
				t.touched[d[i]] = struct{}{}
			}
		}
	}
	if len(spell) > 0 {
		ids := make([]int64, 0, len(spell))
		raws := make([]string, 0, len(spell))
		cnts := make([]int64, 0, len(spell))
		for k, n := range spell {
			ids, raws, cnts = append(ids, k.id), append(raws, k.raw), append(cnts, n)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO fdd.dim_spell(dim_id, raw, cnt) SELECT * FROM unnest($1::bigint[], $2::text[], $3::bigint[])
			ON CONFLICT (dim_id, raw) DO UPDATE SET cnt = fdd.dim_spell.cnt + excluded.cnt`, ids, raws, cnts); err != nil {
			return c, err
		}
	}

	// 3. contacts these records may merge into
	var ek, lk, hk []string
	for _, r := range recs {
		if r.EmailKey != "" {
			ek = append(ek, r.EmailKey)
		}
		if r.LiKey != "" {
			lk = append(lk, r.LiKey)
		}
		if r.HKey != "" {
			hk = append(hk, r.HKey)
		}
	}
	entries := map[int64]*pgEntry{}
	owner := map[string]int64{} // "e:key" → entry ref (existing id > 0, new < 0)
	own := func(kind byte, key string, ref int64) {
		if key != "" {
			owner[string(kind)+":"+key] = ref
		}
	}
	rows, err := tx.Query(ctx, `SELECT i.id, COALESCE(i.email_key, ''), COALESCE(i.li_key, ''), COALESCE(i.h_key, ''),
		c.country, c.state, c.city, c.industry, c.sector, c.ctype, c.seniority, c.division, c.decision, c.csize, c.revenue, c.econf,
		i.name, i.title, i.company, i.website, i.linkedin, i.email, i.phone, i.extra
		FROM fdd.contact_info i JOIN fdd.contacts c ON c.id = i.id
		WHERE i.email_key = ANY($1::text[]) OR i.li_key = ANY($2::text[]) OR i.h_key = ANY($3::text[])`, nonNil(ek), nonNil(lk), nonNil(hk))
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var id int64
		e := &pgEntry{}
		s := &e.st
		var extra []byte
		if err := rows.Scan(&id, &s.EmailKey, &s.LiKey, &s.HKey, &s.Dims[0], &s.Dims[1], &s.Dims[2], &s.Dims[3], &s.Dims[4], &s.Dims[5],
			&s.Dims[6], &s.Dims[7], &s.Dims[8], &s.Dims[9], &s.Dims[10], &s.Dims[11],
			&s.Name, &s.Title, &s.Company, &s.Website, &s.LinkedIn, &s.Email, &s.Phone, &extra); err != nil {
			rows.Close()
			return c, err
		}
		s.Extra = importer.DecodeExtra(extra)
		entries[id] = e
		own('e', s.EmailKey, id)
		own('l', s.LiKey, id)
		own('h', s.HKey, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}

	// 4. merge in memory, in file order (same outcome as row-by-row upserts)
	var news []int64
	for _, r := range recs {
		dims := dimsOf(r)
		var ref int64
		for _, k := range []struct {
			kind byte
			key  string
		}{{'e', r.EmailKey}, {'l', r.LiKey}, {'h', r.HKey}} {
			if k.key == "" {
				continue
			}
			if o, ok := owner[string(k.kind)+":"+k.key]; ok {
				ref = o
				break
			}
		}
		if ref == 0 {
			ref = -int64(len(news) + 1)
			extra := make(map[string]string, len(r.Extra))
			for k, v := range r.Extra {
				extra[k] = v
			}
			entries[ref] = &pgEntry{isNew: true, dirty: true, techDirty: true, tech: r.Tech, st: importer.Stored{Dims: dims,
				EmailKey: r.EmailKey, LiKey: r.LiKey, HKey: r.HKey, Name: r.Name, Title: r.Title, Company: r.Company,
				Website: r.Website, LinkedIn: r.LinkedIn, Email: r.Email, Phone: r.Phone, Extra: extra}}
			news = append(news, ref)
			own('e', r.EmailKey, ref)
			own('l', r.LiKey, ref)
			own('h', r.HKey, ref)
			c.Added++
			continue
		}
		e := entries[ref]
		oe, ol, oh := e.st.EmailKey, e.st.LiKey, e.st.HKey
		changed, techChanged, _ := importer.Merge(&e.st, r, dims, func(kind byte, key string) (bool, error) {
			o, taken := owner[string(kind)+":"+key]
			return !taken || o == ref, nil
		})
		for _, k := range []struct {
			kind     byte
			old, new string
		}{{'e', oe, e.st.EmailKey}, {'l', ol, e.st.LiKey}, {'h', oh, e.st.HKey}} {
			if k.old != k.new {
				if k.old != "" {
					delete(owner, string(k.kind)+":"+k.old)
				}
				own(k.kind, k.new, ref)
			}
		}
		if techChanged {
			e.tech, e.techDirty = r.Tech, true
		}
		if changed {
			e.dirty = true
			c.Updated++
		} else {
			c.Unchanged++
		}
	}

	// 5. normalised industry/sector of merged rows feed the keyword index
	var unknown []int64
	for _, e := range entries {
		if !e.dirty {
			continue
		}
		for _, id := range []int64{e.st.Dims[3], e.st.Dims[4]} {
			if _, ok := t.valueOf[id]; !ok && id > 12 {
				unknown = append(unknown, id)
			}
		}
	}
	if len(unknown) > 0 {
		rows, err := tx.Query(ctx, `SELECT id, value_n FROM fdd.dims WHERE id = ANY($1::bigint[])`, unknown)
		if err != nil {
			return c, err
		}
		for rows.Next() {
			var id int64
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				rows.Close()
				return c, err
			}
			t.valueOf[id] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return c, err
		}
	}

	// 6. write
	var upd, ins []int64
	for ref, e := range entries {
		if !e.dirty {
			continue
		}
		if e.isNew {
			continue
		}
		upd = append(upd, ref)
	}
	if len(news) > 0 {
		ids, err := pgIDs(ctx, tx, `SELECT nextval('fdd.contact_seq') FROM generate_series(1, $1::int)`, len(news))
		if err != nil {
			return c, err
		}
		for i, ref := range news {
			entries[ids[i]] = entries[ref]
			delete(entries, ref)
		}
		ins = ids
	}
	if len(upd) > 0 {
		b := t.columns(entries, upd)
		if _, err := tx.Exec(ctx, `UPDATE fdd.contacts c SET country = u.country, state = u.state, city = u.city, industry = u.industry,
			sector = u.sector, ctype = u.ctype, seniority = u.seniority, division = u.division, decision = u.decision, csize = u.csize,
			revenue = u.revenue, econf = u.econf, has_email = u.he, has_phone = u.hp
			FROM unnest($1::bigint[], $2::int[], $3::int[], $4::int[], $5::int[], $6::int[], $7::int[], $8::int[], $9::int[], $10::int[],
				$11::int[], $12::int[], $13::int[], $14::bool[], $15::bool[])
				AS u(id, country, state, city, industry, sector, ctype, seniority, division, decision, csize, revenue, econf, he, hp)
			WHERE c.id = u.id`, b.contactArgs()...); err != nil {
			return c, err
		}
		if _, err := tx.Exec(ctx, `UPDATE fdd.contact_info i SET email_key = NULLIF(u.ek, ''), li_key = NULLIF(u.lk, ''), h_key = NULLIF(u.hk, ''),
			name = u.name, title = u.title, company = u.company, website = u.website, linkedin = u.linkedin, email = u.email, phone = u.phone,
			extra = u.extra, updated = $16, kw = array_to_tsvector(string_to_array(u.kw, ' ')),
			tech = CASE WHEN u.td THEN array_to_tsvector(string_to_array(u.tech, ' ')) ELSE i.tech END
			FROM unnest($1::bigint[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[],
				$10::text[], $11::text[], $12::bytea[], $13::text[], $14::text[], $15::bool[])
				AS u(id, ek, lk, hk, name, title, company, website, linkedin, email, phone, extra, kw, tech, td)
			WHERE i.id = u.id`, append(b.infoArgs(), now)...); err != nil {
			return c, err
		}
	}
	if len(ins) > 0 {
		b := t.columns(entries, ins)
		if _, err := tx.Exec(ctx, `INSERT INTO fdd.contacts(id, country, state, city, industry, sector, ctype, seniority, division, decision,
			csize, revenue, econf, has_email, has_phone)
			SELECT * FROM unnest($1::bigint[], $2::int[], $3::int[], $4::int[], $5::int[], $6::int[], $7::int[], $8::int[], $9::int[], $10::int[],
				$11::int[], $12::int[], $13::int[], $14::bool[], $15::bool[])`, b.contactArgs()...); err != nil {
			return c, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO fdd.contact_info(id, email_key, li_key, h_key, name, title, company, website, linkedin, email, phone,
			extra, updated, kw, tech)
			SELECT u.id, NULLIF(u.ek, ''), NULLIF(u.lk, ''), NULLIF(u.hk, ''), u.name, u.title, u.company, u.website, u.linkedin, u.email, u.phone,
				u.extra, $16, array_to_tsvector(string_to_array(u.kw, ' ')), array_to_tsvector(string_to_array(u.tech, ' '))
			FROM unnest($1::bigint[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[],
				$10::text[], $11::text[], $12::bytea[], $13::text[], $14::text[], $15::bool[])
				AS u(id, ek, lk, hk, name, title, company, website, linkedin, email, phone, extra, kw, tech, td)`, append(b.infoArgs(), now)...); err != nil {
			return c, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return importer.Counts{}, err
	}
	return c, nil
}

func pgIDs(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// cols holds one array per column for an unnest() write.
type cols struct {
	id                                                  []int64
	dims                                                [12][]int64
	he, hp                                              []bool
	ek, lk, hk, name, title, company, website, li, mail []string
	phone, kw, tech                                     []string
	extra                                               [][]byte
	td                                                  []bool
}

func (t *pgTarget) columns(entries map[int64]*pgEntry, ids []int64) *cols {
	b := &cols{}
	for _, id := range ids {
		e := entries[id]
		s := &e.st
		b.id = append(b.id, id)
		for i := range b.dims {
			b.dims[i] = append(b.dims[i], s.Dims[i])
		}
		b.he, b.hp = append(b.he, s.Email != ""), append(b.hp, s.Phone != "")
		b.ek, b.lk, b.hk = append(b.ek, s.EmailKey), append(b.lk, s.LiKey), append(b.hk, s.HKey)
		b.name, b.title, b.company = append(b.name, s.Name), append(b.title, s.Title), append(b.company, s.Company)
		b.website, b.li, b.mail, b.phone = append(b.website, s.Website), append(b.li, s.LinkedIn), append(b.mail, s.Email), append(b.phone, s.Phone)
		b.extra = append(b.extra, importer.EncodeExtra(s.Extra))
		kw := filter.IndexTokens(importer.FTSText(s.Name, s.Title, s.Company, t.valueOf[s.Dims[3]], t.valueOf[s.Dims[4]], s.Extra))
		b.kw = append(b.kw, strings.Join(kw, " "))
		if e.techDirty {
			b.tech = append(b.tech, strings.Join(filter.TechTokens(e.tech), " "))
		} else {
			b.tech = append(b.tech, "")
		}
		b.td = append(b.td, e.techDirty)
	}
	return b
}

func (b *cols) contactArgs() []any {
	a := []any{b.id}
	for i := range b.dims {
		a = append(a, b.dims[i])
	}
	return append(a, b.he, b.hp)
}

func (b *cols) infoArgs() []any {
	return []any{b.id, b.ek, b.lk, b.hk, b.name, b.title, b.company, b.website, b.li, b.mail, b.phone, b.extra, b.kw, b.tech, b.td}
}

// End settles labels (most common spelling), per-value counts, headline
// stats and planner statistics, then bumps the store's data version.
func (t *pgTarget) End(ctx context.Context, changed int64) error {
	p := t.p
	if len(t.touched) > 0 {
		ids := make([]int64, 0, len(t.touched))
		for id := range t.touched {
			ids = append(ids, id)
		}
		if _, err := p.pool.Exec(ctx, `UPDATE fdd.dims d SET label = s.raw
			FROM (SELECT DISTINCT ON (dim_id) dim_id, raw FROM fdd.dim_spell WHERE dim_id = ANY($1::bigint[]) ORDER BY dim_id, cnt DESC, raw) s
			WHERE d.id = s.dim_id AND d.label <> s.raw`, ids); err != nil {
			return err
		}
		t.touched = map[int64]struct{}{}
	}
	if err := p.refreshCounts(ctx); err != nil {
		return err
	}
	if err := p.refreshStats(ctx); err != nil {
		return err
	}
	if changed > 0 {
		// fresh statistics for the planner; VACUUM also sets the visibility map,
		// which index-only scans need. Best effort: poolers may refuse VACUUM.
		if _, err := p.pool.Exec(ctx, `VACUUM (ANALYZE) fdd.contacts`); err != nil {
			p.pool.Exec(ctx, `ANALYZE fdd.contacts`)
		}
		p.pool.Exec(ctx, `ANALYZE fdd.contact_info`)
	}
	return p.bump(ctx)
}

func (t *pgTarget) Abort(ctx context.Context, changed int64) { t.End(ctx, changed) }

// refreshCounts recomputes dims.cnt in one pass over the narrow table.
func (p *Postgres) refreshCounts(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `WITH g AS (
			SELECT v.x AS id, count(*) AS n FROM fdd.contacts c
			CROSS JOIN LATERAL (VALUES (c.country), (c.state), (c.city), (c.industry), (c.sector), (c.ctype),
				(c.seniority), (c.division), (c.decision), (c.csize), (c.revenue), (c.econf)) AS v(x)
			GROUP BY v.x)
		UPDATE fdd.dims d SET cnt = COALESCE(g.n, 0)
		FROM fdd.dims d0 LEFT JOIN g ON g.id = d0.id
		WHERE d.id = d0.id AND d.cnt IS DISTINCT FROM COALESCE(g.n, 0)`)
	return err
}
