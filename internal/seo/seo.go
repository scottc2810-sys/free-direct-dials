// Package seo selects the landing pages worth indexing (country, industry and
// country × industry with real counts) and tracks pages that should 410.
// Pages are keyed by normalised values, which are the same in every store.
package seo

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"sort"

	"markemind/dials/internal/db"
	"markemind/dials/internal/normalize"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/store"
)

type Page struct {
	Slug     string `json:"slug"`
	Kind     string `json:"kind"`     // country | industry | combo
	Country  string `json:"country"`  // value_n ("" if none)
	Industry string `json:"industry"` // value_n
	Count    int64  `json:"count"`
	Updated  int64  `json:"updated"`
	Gone     bool   `json:"gone"`
}

// Source provides merged counts across every contact store.
type Source interface {
	TopValues(ctx context.Context, dim string, minCount, limit int) ([]store.FacetCount, error)
	TopCombos(ctx context.Context, minCount, limit int) ([]store.Combo, error)
}

// Refresh recomputes the indexable page set: the top `top` candidates with at
// least minCount contacts. Pages that drop out are kept as gone (HTTP 410)
// for 90 days so crawlers de-index them quickly.
func Refresh(ctx context.Context, d *db.DB, src Source, clk platform.Clock, top, minCount int) (int, error) {
	now := clk.Now().Unix()
	var cands []Page
	for _, kind := range []string{"country", "industry"} {
		vals, err := src.TopValues(ctx, kind, minCount, top)
		if err != nil {
			return 0, err
		}
		for _, v := range vals {
			p := Page{Kind: kind, Count: v.Count}
			if kind == "country" {
				p.Country = v.Value
			} else {
				p.Industry = v.Value
			}
			cands = append(cands, p)
		}
	}
	combos, err := src.TopCombos(ctx, minCount, top)
	if err != nil {
		return 0, err
	}
	for _, c := range combos {
		cands = append(cands, Page{Kind: "combo", Country: c.Country, Industry: c.Industry, Count: c.Count})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Count > cands[j].Count })
	if len(cands) > top {
		cands = cands[:top]
	}
	// slugs come from value_n (stable), never from the display label, which can
	// change when the most common spelling shifts after an import
	owner := map[string]string{} // "country/india" → value_n: resolves slug collisions
	slugFor := func(kind, value string) string {
		s := normalize.Slug(value)
		if s == "" {
			s = "x"
		}
		if o, ok := owner[kind+"/"+s]; ok && o != value {
			h := sha1.Sum([]byte(value))
			s += "-" + hex.EncodeToString(h[:3])
		}
		owner[kind+"/"+s] = value
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO seo_pages(slug, kind, country, industry, country_v, industry_v, cnt, updated, gone)
				VALUES(?,?,0,0,?,?,?,?,0)
				ON CONFLICT(slug) DO UPDATE SET kind=excluded.kind, country=0, industry=0, country_v=excluded.country_v,
				industry_v=excluded.industry_v, cnt=excluded.cnt, updated=excluded.updated, gone=0`,
				p.Slug, p.Kind, p.Country, p.Industry, p.Count, now); err != nil {
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
	err := q.QueryRowContext(ctx, `SELECT slug, kind, country_v, industry_v, cnt, updated, gone FROM seo_pages WHERE slug=?`, slug).
		Scan(&p.Slug, &p.Kind, &p.Country, &p.Industry, &p.Count, &p.Updated, &gone)
	p.Gone = gone == 1
	return p, err == nil
}

// List returns live pages (optionally narrowed) ordered by size.
func List(ctx context.Context, q db.Querier, kind, country, industry string, limit int) ([]Page, error) {
	query := `SELECT slug, kind, country_v, industry_v, cnt, updated FROM seo_pages WHERE gone=0`
	var args []any
	if kind != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	if country != "" {
		query += ` AND country_v=?`
		args = append(args, country)
	}
	if industry != "" {
		query += ` AND industry_v=?`
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
