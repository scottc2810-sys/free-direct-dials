// Package seo selects the landing pages worth indexing (country, industry and
// country × industry with real counts) and tracks pages that should 410.
package seo

import (
	"context"
	"database/sql"
	"sort"
	"strconv"

	"markemind/dials/internal/db"
	"markemind/dials/internal/normalize"
	"markemind/dials/internal/platform"
)

type Page struct {
	Slug     string `json:"slug"`
	Kind     string `json:"kind"` // country | industry | combo
	Country  int64  `json:"country"`
	Industry int64  `json:"industry"`
	Count    int64  `json:"count"`
	Updated  int64  `json:"updated"`
	Gone     bool   `json:"gone"`
}

// Refresh recomputes the indexable page set: the top `top` candidates with at
// least minCount contacts. Pages that drop out are kept as gone (HTTP 410)
// for 90 days so crawlers de-index them quickly.
func Refresh(ctx context.Context, d *db.DB, clk platform.Clock, top, minCount int) (int, error) {
	now := clk.Now().Unix()
	labels := map[int64]string{}
	var cands []Page
	for _, dim := range []struct {
		id   int
		kind string
	}{{1, "country"}, {4, "industry"}} {
		// slugs come from value_n (stable), never from the display label, which can
		// change when the most common spelling shifts after an import.
		rows, err := d.R.QueryContext(ctx, `SELECT id, value_n, cnt FROM dims WHERE dim=? AND id>12 AND cnt>=? ORDER BY cnt DESC LIMIT ?`,
			dim.id, minCount, top)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id, n int64
			var label string
			if err := rows.Scan(&id, &label, &n); err != nil {
				rows.Close()
				return 0, err
			}
			labels[id] = label
			p := Page{Kind: dim.kind, Count: n}
			if dim.kind == "country" {
				p.Country = id
			} else {
				p.Industry = id
			}
			cands = append(cands, p)
		}
		rows.Close()
	}
	rows, err := d.R.QueryContext(ctx, `SELECT country, industry, COUNT(*) AS n FROM contacts WHERE country>12 AND industry>12
		GROUP BY country, industry HAVING n>=? ORDER BY n DESC LIMIT ?`, minCount, top)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		p := Page{Kind: "combo"}
		if err := rows.Scan(&p.Country, &p.Industry, &p.Count); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, p)
	}
	rows.Close()
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Count > cands[j].Count })
	if len(cands) > top {
		cands = cands[:top]
	}

	label := func(id int64) string {
		if l, ok := labels[id]; ok {
			return l
		}
		var l string
		d.R.QueryRowContext(ctx, `SELECT value_n FROM dims WHERE id=?`, id).Scan(&l)
		labels[id] = l
		return l
	}
	owner := map[string]int64{} // "country/india" → dim id: resolves slug collisions
	slugFor := func(kind string, id int64) string {
		s := normalize.Slug(label(id))
		if s == "" {
			s = "x" + strconv.FormatInt(id, 10)
		}
		if o, ok := owner[kind+"/"+s]; ok && o != id {
			s += "-" + strconv.FormatInt(id, 10)
		}
		owner[kind+"/"+s] = id
		return s
	}
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE seo_pages SET gone=1, updated=? WHERE gone=0`, now); err != nil {
			return err
		}
		for _, p := range cands {
			switch p.Kind {
			case "country":
				p.Slug = "country/" + slugFor("country", p.Country)
			case "industry":
				p.Slug = "industry/" + slugFor("industry", p.Industry)
			default:
				p.Slug = "country/" + slugFor("country", p.Country) + "/" + slugFor("industry", p.Industry)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO seo_pages(slug, kind, country, industry, cnt, updated, gone) VALUES(?,?,?,?,?,?,0)
				ON CONFLICT(slug) DO UPDATE SET kind=excluded.kind, country=excluded.country, industry=excluded.industry,
				cnt=excluded.cnt, updated=excluded.updated, gone=0`, p.Slug, p.Kind, p.Country, p.Industry, p.Count, now); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM seo_pages WHERE gone=1 AND updated < ?`, now-90*86400)
		return err
	})
	return len(cands), err
}

// Lookup finds a page by slug ("country/india/software").
func Lookup(ctx context.Context, q db.Querier, slug string) (Page, bool) {
	var p Page
	var gone int
	err := q.QueryRowContext(ctx, `SELECT slug, kind, country, industry, cnt, updated, gone FROM seo_pages WHERE slug=?`, slug).
		Scan(&p.Slug, &p.Kind, &p.Country, &p.Industry, &p.Count, &p.Updated, &gone)
	p.Gone = gone == 1
	return p, err == nil
}

// List returns live pages (optionally narrowed) ordered by size.
func List(ctx context.Context, q db.Querier, kind string, country, industry int64, limit int) ([]Page, error) {
	query := `SELECT slug, kind, country, industry, cnt, updated FROM seo_pages WHERE gone=0`
	var args []any
	if kind != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	if country > 0 {
		query += ` AND country=?`
		args = append(args, country)
	}
	if industry > 0 {
		query += ` AND industry=?`
		args = append(args, industry)
	}
	query += ` ORDER BY cnt DESC LIMIT ?`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var p Page
		if err := rows.Scan(&p.Slug, &p.Kind, &p.Country, &p.Industry, &p.Count, &p.Updated); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
