package search

import (
	"bytes"
	"context"
	"math/rand/v2"
	"net/url"
	"sort"
	"strings"
	"testing"

	"markemind/dials/internal/db"
	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
)

func setup(t *testing.T, rows int) (*Service, *db.DB) {
	t.Helper()
	d := dbtest.Open(t)
	im := &importer.Importer{DB: d, Disposable: emailcheck.NewDisposable()}
	var buf bytes.Buffer
	importer.Generate(&buf, rows, 42)
	buf.WriteString("Ann Lee,,CEO,C-Level,Executive,Decision Maker,Acme Corp,acme.com,,ann@acme.com,Verified,,,,Austin,,USA,Software,SaaS,Private,11-50,,HubSpot,,\n")
	if _, err := im.Run(context.Background(), &buf, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	return New(d, NewLRU(1<<20, nil), 4), d
}

func parse(t *testing.T, raw string) filter.Filter {
	q, _ := url.ParseQuery(raw)
	f, err := filter.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func count(t *testing.T, s *Service, raw string) int64 {
	t.Helper()
	r, err := s.Resolve(context.Background(), parse(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Count(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// contact is the in-memory oracle view of one row.
type contact struct {
	id       int64
	dims     map[string]string // dim key → value_n
	hasEmail bool
	hasPhone bool
	tech     []string
}

func loadOracle(t *testing.T, d *db.DB) []contact {
	cols := make([]string, len(filter.Dims))
	for i, dm := range filter.Dims {
		cols[i] = "(SELECT value_n FROM dims WHERE id=c." + dm.Key + ")"
	}
	rows, err := d.R.Query(`SELECT c.id, ` + strings.Join(cols, ",") + `, c.has_email, c.has_phone,
		(SELECT extra FROM contact_info WHERE id=c.id) FROM contacts c`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []contact
	for rows.Next() {
		vals := make([]string, len(filter.Dims))
		dst := []any{new(int64)}
		for i := range vals {
			dst = append(dst, &vals[i])
		}
		var he, hp int
		var extra []byte
		dst = append(dst, &he, &hp, &extra)
		if err := rows.Scan(dst...); err != nil {
			t.Fatal(err)
		}
		c := contact{id: *dst[0].(*int64), dims: map[string]string{}, hasEmail: he == 1, hasPhone: hp == 1}
		for i, dm := range filter.Dims {
			c.dims[dm.Key] = vals[i]
		}
		for _, name := range strings.Split(importer.DecodeExtra(extra)["Technologies"], ",") {
			if t := filter.CleanTech(name); t != "" {
				c.tech = append(c.tech, strings.Fields(t)...) // word tokens, as the tech index sees them
			}
		}
		out = append(out, c)
	}
	return out
}

func (c contact) match(f filter.Filter, exclude string, kw map[int64]bool) bool {
	for _, dm := range filter.Dims {
		if dm.Key == exclude {
			continue
		}
		vs := f.Values[dm.Key]
		if len(vs) == 0 {
			continue
		}
		ok := false
		for _, v := range vs {
			if c.dims[dm.Key] == v {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if kw != nil && !kw[c.id] {
		return false
	}
	if len(f.Tech) > 0 {
		// a term matches when each of its words prefixes some technology word of the contact
		ok := false
		for _, term := range f.Tech {
			all := true
			for _, w := range strings.Fields(term) {
				hit := false
				for _, have := range c.tech {
					hit = hit || strings.HasPrefix(have, w)
				}
				all = all && hit
			}
			ok = ok || all
		}
		if !ok {
			return false
		}
	}
	return (!f.HasEmail || c.hasEmail) && (!f.HasPhone || c.hasPhone)
}

func TestAcceptance1_2_3_Semantics(t *testing.T) {
	s, d := setup(t, 2000)
	// #1: the USA variants are one option whose count is the sum of variants
	us := count(t, s, "country=united+states")
	if us == 0 || us != count(t, s, "country=USA") || us != count(t, s, "country=u.s.a.") {
		t.Fatalf("USA variants differ")
	}
	var raw int64
	d.R.QueryRow(`SELECT COUNT(*) FROM contacts c JOIN dims x ON x.id=c.country WHERE x.value_n='united states'`).Scan(&raw)
	if us != raw {
		t.Fatalf("count %d vs raw %d", us, raw)
	}
	// #2: OR within a dimension, AND across dimensions
	in, sw := count(t, s, "country=india"), count(t, s, "country=india&country=usa")
	if sw != in+us {
		t.Fatalf("OR within: %d != %d + %d", sw, in, us)
	}
	both := count(t, s, "country=india&country=usa&industry=software")
	if both > sw || both == 0 {
		t.Fatalf("AND across: %d", both)
	}
	for _, q := range []string{"industry=software", "country=india", "", "country=atlantis", "country=atlantis&industry=software"} {
		count(t, s, q) // removing filters never errors; unknown values match nothing
	}
	if count(t, s, "country=atlantis") != 0 {
		t.Fatal("unknown value must match nothing")
	}
	// #3: (Blank) state returns only rows with an empty state
	blank := count(t, s, "state=__blank__")
	var want int64
	d.R.QueryRow(`SELECT COUNT(*) FROM contacts WHERE state=2`).Scan(&want)
	if blank == 0 || blank != want {
		t.Fatalf("blank %d want %d", blank, want)
	}
}

func TestAcceptance4Keyword(t *testing.T) {
	s, _ := setup(t, 200)
	if count(t, s, "kw=ac") == 0 || count(t, s, "kw=acme") == 0 {
		t.Fatal("prefix 'ac' must match Acme")
	}
	if count(t, s, "kw=acme+corp") != count(t, s, "kw=corp+acme") {
		t.Fatal("every word must match, order-independent")
	}
	for _, kw := range []string{`"; DROP --`, `%`, `_`, `🚀`, `NEAR(`, `*`, `"`, `'`, `\`, `AND OR NOT`} {
		count(t, s, "kw="+url.QueryEscape(kw)) // must not error
	}
	if count(t, s, "kw=%25") != count(t, s, "") {
		t.Fatal("'%' carries no token, so it is no keyword filter")
	}
}

func TestAcceptance5FacetsBruteForce(t *testing.T) {
	s, d := setup(t, 3000)
	oracle := loadOracle(t, d)
	rng := rand.New(rand.NewPCG(1, 2))
	valuesOf := map[string][]string{}
	for _, c := range oracle {
		for k, v := range c.dims {
			valuesOf[k] = append(valuesOf[k], v)
		}
	}
	techs := []string{"hub", "sales", "node", "c+", ".net", "aws", "zzz", "google an", "node.js"}
	ctx := context.Background()
	for iter := 0; iter < 150; iter++ {
		q := url.Values{}
		for _, dm := range filter.Dims {
			if rng.IntN(5) != 0 {
				continue
			}
			for k := 1 + rng.IntN(3); k > 0; k-- {
				vs := valuesOf[dm.Key]
				q.Add(dm.Key, vs[rng.IntN(len(vs))])
			}
		}
		if rng.IntN(4) == 0 {
			q.Set("tech", techs[rng.IntN(len(techs))])
		}
		if rng.IntN(4) == 0 {
			q.Set("email", "1")
		}
		if rng.IntN(5) == 0 {
			q.Set("phone", "1")
		}
		var kwSet map[int64]bool
		if rng.IntN(6) == 0 {
			q.Set("kw", []string{"vp", "engineer chief", "acme", "cloud"}[rng.IntN(4)])
		}
		f, err := filter.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		if f.Keyword != "" {
			kwSet = map[int64]bool{}
			ids, _ := QueryIDs(ctx, d.R, `SELECT rowid FROM fts WHERE fts MATCH ?`, filter.FTSQuery(f.Keyword))
			for _, id := range ids {
				kwSet[id] = true
			}
		}
		r, err := s.Resolve(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var want int64
		for _, c := range oracle {
			if c.match(f, "", kwSet) {
				want++
			}
		}
		if got, err := s.Count(ctx, r); err != nil || got != want {
			t.Fatalf("iter %d count(%s) = %d want %d err %v", iter, f.Canonical(), got, want, err)
		}
		dm := filter.Dims[rng.IntN(len(filter.Dims))]
		res, err := s.Facets(ctx, r, dm.Key, "")
		if err != nil {
			t.Fatal(err)
		}
		exp := map[string]int64{}
		for _, c := range oracle {
			if c.match(f, dm.Key, kwSet) {
				exp[c.dims[dm.Key]]++
			}
		}
		got := map[string]int64{}
		for _, v := range res.Values {
			got[v.Value] = v.Count
		}
		for v, n := range exp {
			if got[v] != n {
				t.Fatalf("iter %d facet %s(%s)[%s] = %d want %d", iter, dm.Key, f.Canonical(), v, got[v], n)
			}
		}
		for _, sel := range f.Values[dm.Key] {
			found := false
			for _, v := range res.Values {
				if v.Value == sel && v.Selected {
					found = true
				}
			}
			if !found {
				t.Fatalf("selected value %s missing from facet %s", sel, dm.Key)
			}
		}
		if !sort.SliceIsSorted(res.Values, func(i, j int) bool { return res.Values[i].Count > res.Values[j].Count }) {
			t.Fatal("facets not sorted by count")
		}
	}
}

func TestFacetSearchAndLabels(t *testing.T) {
	s, _ := setup(t, 1000)
	ctx := context.Background()
	r, _ := s.Resolve(ctx, parse(t, ""))
	res, err := s.Facets(ctx, r, "city", "san")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Values) == 0 {
		t.Fatal("no cities matching 'san'")
	}
	for _, v := range res.Values {
		if !strings.Contains(v.Value, "san") {
			t.Fatalf("q filter leaked %q", v.Value)
		}
	}
	res, _ = s.Facets(ctx, r, "city", "munchen")
	if len(res.Values) != 1 || res.Values[0].Label != "München" {
		t.Fatalf("diacritic search/label: %+v", res.Values)
	}
	res, _ = s.Facets(ctx, r, "state", "")
	foundBlank := false
	for _, v := range res.Values {
		if v.Value == filter.Blank && v.Label == "(Blank)" {
			foundBlank = true
		}
	}
	if !foundBlank {
		t.Fatal("(Blank) option missing")
	}
}

func TestPreviewMasked(t *testing.T) {
	s, _ := setup(t, 300)
	ctx := context.Background()
	r, _ := s.Resolve(ctx, parse(t, "email=1&phone=1"))
	total, rows, err := s.Preview(ctx, r)
	if err != nil || total == 0 || len(rows) != PreviewLimit {
		t.Fatalf("preview total=%d rows=%d err=%v", total, len(rows), err)
	}
	for _, row := range rows {
		if !row.Masked || strings.Count(row.Email, "@") != 1 || !strings.Contains(row.Email, dot) || strings.Contains(row.Phone, "555") {
			t.Fatalf("not masked: %+v", row)
		}
		if row.Name == "" || row.Country == "" && row.City == "" && row.State == "" && row.Industry == "" {
			t.Fatalf("teaser fields missing: %+v", row)
		}
	}
	if MaskEmail("john.doe@acme.co.uk") != "j"+dot+dot+dot+"@a"+dot+dot+dot+".uk" {
		t.Fatalf("MaskEmail = %s", MaskEmail("john.doe@acme.co.uk"))
	}
}

func TestBusyShedding(t *testing.T) {
	s, _ := setup(t, 50)
	s.WaitTimeout = 1
	for i := 0; i < cap(s.sem); i++ {
		s.sem <- struct{}{}
	}
	r, _ := s.Resolve(context.Background(), parse(t, "country=india&industry=software"))
	if _, err := s.Count(context.Background(), r); err != ErrBusy {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestLRUBounds(t *testing.T) {
	c := NewLRU(10_000, nil)
	for i := 0; i < 1000; i++ {
		c.Set(strings.Repeat("k", i%50)+string(rune('a'+i%26)), make([]byte, 100), 0)
	}
	if st := c.Stats(); st.Bytes > 10_000 {
		t.Fatalf("LRU over budget: %+v", st)
	}
}
