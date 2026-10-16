package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"markemind/dials/internal/config"
	"markemind/dials/internal/db"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/search"
	"markemind/dials/internal/settings"
	"markemind/dials/internal/unlock"
)

// benchCmd measures the hot paths against the current database with caches
// disabled: counts, facets, previews and claims. Run it after big imports or
// schema/index changes and compare with docs/PERFORMANCE.md.
func benchCmd(ctx context.Context, cfg *config.Config, args []string) error {
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	svc := search.New(d, search.NewLRU(1, nil), 4) // 1-byte cache: always miss
	svc.QueryTimeout = 30 * time.Second
	top := func(dim int, n int) []string {
		rows, _ := d.R.QueryContext(ctx, `SELECT value_n FROM dims WHERE dim=? AND id>12 ORDER BY cnt DESC LIMIT ?`, dim, n)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			rows.Scan(&v)
			out = append(out, v)
		}
		return out
	}
	var total int64
	d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts`).Scan(&total)
	countries, industries, cities, sen := top(1, 3), top(4, 3), top(3, 400), top(7, 2)
	if len(countries) < 2 || len(industries) < 2 || len(cities) < 2 {
		return fmt.Errorf("not enough data to benchmark (import a dataset first, e.g. dials gen 1000000 big.csv && dials import big.csv)")
	}
	rare := cities[len(cities)-1]
	filters := map[string]string{
		"empty":                      "",
		"1 dim (fast path)":          "country=" + url.QueryEscape(countries[0]),
		"2 dims":                     "country=" + url.QueryEscape(countries[0]) + "&industry=" + url.QueryEscape(industries[0]),
		"3 dims + email":             "country=" + url.QueryEscape(countries[1]) + "&industry=" + url.QueryEscape(industries[1]) + "&seniority=" + url.QueryEscape(sen[0]) + "&email=1",
		"OR within dim":              "country=" + url.QueryEscape(countries[0]) + "&country=" + url.QueryEscape(countries[1]) + "&industry=" + url.QueryEscape(industries[0]),
		"rare city":                  "city=" + url.QueryEscape(rare),
		"keyword (common prefix)":    "kw=cl",
		"keyword + country":          "kw=analytics&country=" + url.QueryEscape(countries[0]),
		"tech prefix":                "tech=sales",
		"tech + 2 dims + phone":      "tech=hub&country=" + url.QueryEscape(countries[0]) + "&industry=" + url.QueryEscape(industries[0]) + "&phone=1",
		"has email + has phone only": "email=1&phone=1",
	}
	names := make([]string, 0, len(filters))
	for k := range filters {
		names = append(names, k)
	}
	sort.Strings(names)
	reps := 5
	fmt.Printf("dataset: %d contacts · %d CPU · GOMAXPROCS %d\n\n", total, runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Printf("%-28s %10s %10s %10s %10s %10s\n", "FILTER", "MATCHES", "count p50", "preview", "facet ctry", "facet city")
	for _, name := range names {
		q, _ := url.ParseQuery(filters[name])
		f, _ := filter.Parse(q)
		var n int64
		cnt := timeit(reps, func() {
			r, _ := svc.Resolve(ctx, f)
			n, _ = svc.Count(ctx, r)
		})
		prev := timeit(reps, func() { r, _ := svc.Resolve(ctx, f); svc.Preview(ctx, r) })
		fc := timeit(reps, func() { r, _ := svc.Resolve(ctx, f); svc.Facets(ctx, r, "country", "") })
		fcity := timeit(reps, func() { r, _ := svc.Resolve(ctx, f); svc.Facets(ctx, r, "city", "") })
		fmt.Printf("%-28s %10d %10s %10s %10s %10s\n", name, n, cnt, prev, fc, fcity)
	}
	fs := timeit(reps, func() {
		r, _ := svc.Resolve(ctx, filter.Filter{Values: map[string][]string{}})
		svc.Facets(ctx, r, "city", "san")
	})
	fmt.Printf("\nfacet value search (city q=san): %s\n", fs)

	// claims: a throwaway user hands out real batches, then is deleted
	clk := platform.SystemClock{}
	st := &settings.Store{DB: d, Clock: clk}
	st.Load(ctx)
	us := &unlock.Service{DB: d, Search: svc, Settings: st, Clock: clk}
	var uid int64
	if err := d.W.QueryRowContext(ctx, `INSERT INTO users(email, created, last_seen, subscribed, unsub_token) VALUES('bench@localhost.invalid',0,0,0,?) RETURNING id`,
		platform.Token(16)).Scan(&uid); err != nil {
		return err
	}
	defer func() {
		for _, q := range []string{`DELETE FROM unlocked WHERE user_id=?`, `DELETE FROM ad_tokens WHERE user_id=?`, `DELETE FROM user_daily WHERE user_id=?`, `DELETE FROM users WHERE id=?`} {
			d.W.ExecContext(context.Background(), q, uid)
		}
	}()
	var lat []time.Duration
	q, _ := url.ParseQuery(filters["2 dims"])
	f, _ := filter.Parse(q)
	for i := 0; i < 40; i++ {
		tok := platform.Token(18)
		d.W.ExecContext(ctx, `INSERT INTO ad_tokens(id, user_id, created, mode) VALUES(?,?,?, 'sponsor')`, tok, uid, time.Now().Add(-time.Minute).UnixMilli())
		d.W.ExecContext(ctx, `DELETE FROM user_daily WHERE user_id=?`, uid) // bypass the daily cap for the benchmark
		t0 := time.Now()
		if _, err := us.Claim(ctx, uid, tok, f, ""); err != nil {
			return fmt.Errorf("claim %d: %w", i, err)
		}
		lat = append(lat, time.Since(t0))
	}
	fmt.Printf("claim (2 dims, 40 consecutive batches): %s\n", pct(lat))
	vt := timeit(reps, func() { us.View(ctx, uid, f, false) })
	fmt.Printf("personal view (400 unlocked): %s\n", vt)

	if len(args) > 0 && args[0] == "-plans" {
		fmt.Println("\nquery plans:")
		r, _ := svc.Resolve(ctx, f)
		where, wargs := r.Where("")
		explain(ctx, d, "count 2 dims", `SELECT COUNT(*) FROM contacts c WHERE `+where, wargs...)
		where2, wargs2 := r.Where("country")
		explain(ctx, d, "facet country | industry", `SELECT c.country, COUNT(*) FROM contacts c WHERE `+where2+` GROUP BY 1 ORDER BY 2 DESC LIMIT 301`, wargs2...)
		explain(ctx, d, "claim next 10", `SELECT c.id FROM contacts c WHERE `+where+` AND NOT EXISTS (SELECT 1 FROM unlocked u WHERE u.user_id=? AND u.contact_id=c.id) ORDER BY c.id LIMIT 10`, append(wargs, uid)...)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fi, _ := os.Stat(d.Path)
	fmt.Printf("\nheap %d MB · process %d MB · db file %d MB\n", ms.HeapAlloc>>20, ms.Sys>>20, fi.Size()>>20)
	return nil
}

func timeit(n int, fn func()) string {
	var ds []time.Duration
	for i := 0; i < n; i++ {
		t0 := time.Now()
		fn()
		ds = append(ds, time.Since(t0))
	}
	return pct(ds)
}

func pct(ds []time.Duration) string {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p := func(q float64) time.Duration { return ds[min(len(ds)-1, int(q*float64(len(ds))))] }
	if len(ds) < 10 {
		return p(0.5).Round(100 * time.Microsecond).String()
	}
	return fmt.Sprintf("p50 %s p95 %s max %s", p(0.5).Round(100*time.Microsecond), p(0.95).Round(100*time.Microsecond), ds[len(ds)-1].Round(100*time.Microsecond))
}

func explain(ctx context.Context, d *db.DB, label, q string, args ...any) {
	rows, err := d.R.QueryContext(ctx, `EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		fmt.Println(label, "error:", err)
		return
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, nu int
		var detail string
		rows.Scan(&id, &parent, &nu, &detail)
		lines = append(lines, detail)
	}
	fmt.Printf("  %s:\n    %s\n", label, strings.Join(lines, "\n    "))
}
