package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/search"
)

// genParts writes one synthetic file (3% duplicate identities, messy values)
// and the same rows split into n consecutive parts.
func genParts(t *testing.T, rows int, seed uint64, n int) ([]byte, [][]byte) {
	t.Helper()
	var all bytes.Buffer
	if err := importer.Generate(&all, rows, seed); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(bytes.NewReader(all.Bytes())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	head, body := recs[0], recs[1:]
	parts := make([][]byte, n)
	per := (len(body) + n - 1) / n
	for i := range parts {
		var b bytes.Buffer
		w := csv.NewWriter(&b)
		w.Write(head)
		w.WriteAll(body[min(i*per, len(body)):min((i+1)*per, len(body))])
		parts[i] = b.Bytes()
	}
	return all.Bytes(), parts
}

func newImporter(f *Federation) *importer.Importer {
	return &importer.Importer{DB: f.Control, Disposable: emailcheck.NewDisposable()}
}

func runImport(t *testing.T, im *importer.Importer, f *Federation, target int, data []byte) importer.Stats {
	t.Helper()
	st, err := im.Run(context.Background(), bytes.NewReader(data), importer.Options{Target: f.ImportTarget(target)})
	if err != nil {
		t.Fatalf("import into %d: %v", target, err)
	}
	return st
}

// newFed is a federation with the built-in store plus extra SQLite stores.
func newFed(t *testing.T, extra int) (*Federation, []int) {
	t.Helper()
	ctx := context.Background()
	d := dbtest.Open(t)
	f := &Federation{Control: d, Key: KeyFrom([]byte("test-secret")), DataDir: t.TempDir(), Cache: search.NewLRU(1<<20, nil), Budget: -1}
	if err := f.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	ids := []int{0}
	for i := 0; i < extra; i++ {
		m, err := f.Add(ctx, fmt.Sprintf("extra %d", i+1), KindSQLite, fmt.Sprintf("extra%d.db", i+1), 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	return f, ids
}

// reference imports the whole file into one plain database.
func reference(t *testing.T, all []byte) *Federation {
	t.Helper()
	ref := NewLocal(dbtest.Open(t))
	runImport(t, newImporter(ref), ref, Auto, all)
	return ref
}

func flt(t *testing.T, raw string) filter.Filter {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	f, err := filter.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

var probeFilters = []string{
	"", "country=usa", "country=india&industry=software", "country=usa&country=india", "kw=chief", "kw=chief&country=usa",
	"tech=node.js", "tech=c%2B%2B", "tech=google+ana", "email=1", "phone=1&country=germany", "seniority=c-level&csize=11-50",
	"city=__blank__", "industry=__blank__&email=1", "country=atlantis", "kw=zzzzqqq", "state=california&kw=sales",
	"econf=verified&revenue=%241b%2B", "kw=garcia", "kw=muller",
}

var probeDims = []string{"country", "state", "industry", "seniority", "csize", "econf", "ctype"}

// sameAsReference checks counts, facets, stats and preview paging against a
// single database holding the same rows.
func sameAsReference(t *testing.T, f, ref *Federation) {
	t.Helper()
	ctx := context.Background()
	if a, b := f.Stats(ctx), ref.Stats(ctx); a.Contacts != b.Contacts || a.WithEmail != b.WithEmail || a.WithPhone != b.WithPhone || a.Countries != b.Countries {
		t.Fatalf("stats: federated %+v, single %+v", a, b)
	}
	for _, raw := range probeFilters {
		x := flt(t, raw)
		got, st, err := f.Count(ctx, x)
		if err != nil || len(st.Partial) > 0 || st.Pending {
			t.Fatalf("count %q: %v %+v", raw, err, st)
		}
		want, _, _ := ref.Count(ctx, x)
		if got != want {
			t.Fatalf("count %q: federated %d, single %d", raw, got, want)
		}
		for _, dim := range probeDims {
			a, err := f.Facets(ctx, x, dim, "")
			if err != nil {
				t.Fatalf("facets %s %q: %v", dim, raw, err)
			}
			b, _ := ref.Facets(ctx, x, dim, "")
			am, bm := map[string]int64{}, map[string]int64{}
			for _, v := range a.Values {
				am[v.Value] = v.Count
			}
			for _, v := range b.Values {
				bm[v.Value] = v.Count
			}
			if fmt.Sprint(am) != fmt.Sprint(bm) {
				t.Fatalf("facets %s %q:\n federated %v\n single    %v", dim, raw, am, bm)
			}
			for _, v := range x.Values[dim] {
				if _, ok := am[v]; !ok {
					t.Fatalf("facets %s %q: selected %q missing", dim, raw, v)
				}
			}
		}
		// preview pages: 10 masked rows each, no overlap, totals agree
		seen := map[int64]bool{}
		for p := 0; p < 5; p++ {
			pv, err := f.Preview(ctx, x, p, 10, 5)
			if err != nil {
				t.Fatalf("preview %q p%d: %v", raw, p, err)
			}
			if pv.Total != want || pv.Pages != int(min((want+9)/10, 5)) {
				t.Fatalf("preview %q: total %d pages %d (want %d)", raw, pv.Total, pv.Pages, want)
			}
			if p >= pv.Pages {
				continue
			}
			wantRows := int(min(10, want-int64(p*10)))
			if len(pv.Rows) != wantRows {
				t.Fatalf("preview %q p%d: %d rows, want %d", raw, p, len(pv.Rows), wantRows)
			}
			for _, r := range pv.Rows {
				if !r.Masked || seen[r.ID] || (r.Email != "" && !strings.Contains(r.Email, "\xe2\x80\xa2")) {
					t.Fatalf("preview %q p%d: bad row %+v", raw, p, r)
				}
				seen[r.ID] = true
			}
		}
	}
	// facet value search narrows the same way
	a, _ := f.Facets(ctx, flt(t, "country=usa"), "city", "san")
	b, _ := ref.Facets(ctx, flt(t, "country=usa"), "city", "san")
	if len(a.Values) != len(b.Values) || len(a.Values) == 0 {
		t.Fatalf("facet search: %d vs %d", len(a.Values), len(b.Values))
	}
}

// claimsAndPaging claims everything for a filter, then pages the view.
func claimsAndPaging(t *testing.T, f *Federation) {
	t.Helper()
	ctx := context.Background()
	x := flt(t, "country=usa")
	total, _, _ := f.Count(ctx, x)
	if total < 30 {
		t.Fatalf("need a bigger sample, have %d", total)
	}
	const uid = 42
	got := map[int64]bool{}
	stores := map[int]bool{}
	var batch int64
	for {
		batch++
		rows, err := f.Claim(ctx, uid, batch, x, 10, 1000+batch)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if got[r.ID] || r.Masked || r.Batch != batch || !r.New {
				t.Fatalf("claim batch %d: bad or repeated row %+v", batch, r)
			}
			got[r.ID] = true
			s, _ := SplitID(r.ID)
			stores[s] = true
		}
		if batch > 1000 {
			t.Fatal("claims never ran out")
		}
	}
	if int64(len(got)) != total {
		t.Fatalf("claimed %d of %d", len(got), total)
	}
	if n, _ := f.Unlocked(ctx, uid, x); n != total {
		t.Fatalf("unlocked count %d, want %d", n, total)
	}
	if n, _ := f.Unlocked(ctx, uid, filter.Filter{}); n != total {
		t.Fatalf("unlocked (no filter) %d, want %d", n, total)
	}
	if len(f.readable()) > 1 && len(stores) < 2 {
		t.Fatalf("claims came from %d store(s); the sample spans several", len(stores))
	}
	// keyset pages: every row once, newest batch first, stable order
	var cur Cursor
	seen := map[int64]bool{}
	lastBatch := int64(1 << 62)
	pages := 0
	for {
		rows, next, err := f.ViewPage(ctx, uid, filter.Filter{}, cur, 0, 7, false)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range rows {
			if seen[r.ID] || r.Batch > lastBatch {
				t.Fatalf("page %d: repeated or out of order %d (batch %d after %d)", pages, r.ID, r.Batch, lastBatch)
			}
			seen[r.ID], lastBatch = true, r.Batch
		}
		if next.IsZero() {
			break
		}
		if pc := ParseCursor(next.String()); pc != next {
			t.Fatalf("cursor round trip %v → %q → %v", next, next.String(), pc)
		}
		cur = next
	}
	if int64(len(seen)) != total || pages != int((total+6)/7) {
		t.Fatalf("paged %d rows in %d pages, want %d in %d", len(seen), pages, total, (total+6)/7)
	}
	newest, _, _ := f.ViewPage(ctx, uid, x, Cursor{}, batch-1, 100, false)
	for _, r := range newest {
		if r.Batch != batch-1 {
			t.Fatalf("only-batch view returned batch %d", r.Batch)
		}
	}
	if err := f.ForgetUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.Unlocked(ctx, uid, filter.Filter{}); n != 0 {
		t.Fatalf("forgotten user still has %d unlocks", n)
	}
}

func TestFederationMatchesOneDatabase(t *testing.T) {
	all, parts := genParts(t, 1500, 7, 3)
	f, ids := newFed(t, 2)
	im := newImporter(f)
	for i, p := range parts {
		runImport(t, im, f, ids[i], p) // each third into its own store
	}
	ref := reference(t, all)
	for _, m := range f.Members() {
		if c, _ := m.B.Count(context.Background(), filter.Filter{}); c == 0 {
			t.Fatalf("store %s got no contacts", m.Name)
		}
	}
	sameAsReference(t, f, ref)
	claimsAndPaging(t, f)
}

func TestReimportNeverDuplicatesAcrossStores(t *testing.T) {
	all, parts := genParts(t, 600, 3, 2)
	f, ids := newFed(t, 1)
	im := newImporter(f)
	runImport(t, im, f, ids[0], parts[0])
	runImport(t, im, f, ids[1], parts[1])
	before := f.Stats(context.Background()).Contacts
	// the whole file again, every row aimed at the second store: existing people
	// must merge where they already are
	st := runImport(t, im, f, ids[1], all)
	if st.Added != 0 {
		t.Fatalf("re-import added %d contacts", st.Added)
	}
	if after := f.Stats(context.Background()).Contacts; after != before {
		t.Fatalf("contacts %d → %d after re-import", before, after)
	}
	ref := reference(t, all)
	sameAsReference(t, f, ref)
}

func TestRemoveReachesEveryStore(t *testing.T) {
	_, parts := genParts(t, 400, 11, 2)
	f, ids := newFed(t, 1)
	im := newImporter(f)
	runImport(t, im, f, ids[0], parts[0])
	runImport(t, im, f, ids[1], parts[1])
	ctx := context.Background()
	second := f.Member(ids[1])
	rows, err := second.B.Page(ctx, flt(t, "email=1"), 0, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("page: %v %v", rows, err)
	}
	email := rows[0].Email
	n, err := f.Remove(ctx, "email", email, 1)
	if err != nil || n < 1 {
		t.Fatalf("remove: %d %v", n, err)
	}
	for _, m := range f.Members() {
		if ids, _ := m.B.FindByIdentity(ctx, "email", email); len(ids) != 0 {
			t.Fatalf("still in %s", m.Name)
		}
	}
	st := runImport(t, im, f, Auto, parts[1])
	if st.Reasons["suppressed"] < 1 {
		t.Fatalf("re-upload brought the removed person back: %+v", st.Reasons)
	}
}

// failing simulates an unreachable store.
type failing struct{ Backend }

var errDown = errors.New("connection refused")

func (failing) Count(context.Context, filter.Filter) (int64, error) { return 0, errDown }
func (failing) Facet(context.Context, filter.Filter, string, string, int) ([]FacetCount, bool, error) {
	return nil, false, errDown
}
func (failing) Claim(context.Context, int64, int64, filter.Filter, int, int64) ([]search.Row, error) {
	return nil, errDown
}
func (failing) Ping(context.Context) error { return errDown }

func TestUnreachableStoreGivesPartialResults(t *testing.T) {
	_, parts := genParts(t, 400, 5, 2)
	f, ids := newFed(t, 1)
	im := newImporter(f)
	runImport(t, im, f, ids[0], parts[0])
	runImport(t, im, f, ids[1], parts[1])
	ctx := context.Background()
	// a two-condition filter the mirror can't answer, so the remote store must be asked
	q := flt(t, "country=india&email=1")
	localOnly, _ := f.Member(0).B.Count(ctx, q)
	m := f.Member(ids[1])
	m.B = failing{m.B}
	n, st, err := f.Count(ctx, q)
	if err != nil || len(st.Partial) == 0 || n != localOnly {
		t.Fatalf("count with a store down: %d %+v %v (want %d, partial)", n, st, err, localOnly)
	}
	fr, err := f.Facets(ctx, flt(t, "country=india"), "industry", "")
	if err != nil || !fr.Partial {
		t.Fatalf("facets with a store down: %v partial=%v", err, fr.Partial)
	}
	// circuit breaker: the store is skipped (no new error) until its back-off ends
	if m.usable(f.now()) {
		t.Fatal("failed store should be skipped for a while")
	}
	rows, err := f.Claim(ctx, 9, 1, filter.Filter{}, 10, 1)
	if err != nil || len(rows) != 10 {
		t.Fatalf("claims must continue from healthy stores: %d %v", len(rows), err)
	}
	if _, err := im.Run(ctx, bytes.NewReader(parts[0]), importer.Options{Target: f.ImportTarget(Auto)}); !errors.Is(err, ErrStoresDown) {
		t.Fatalf("uploads must refuse while a store is down (duplicates), got %v", err)
	}
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	f, ids := newFed(t, 1)
	if _, err := f.Add(ctx, "again", KindSQLite, "extra1.db", 0); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same database twice: %v", err)
	}
	for _, bad := range []string{"../evil.db", "sub/../../evil.db", "dials.db", "x.txt"} {
		if _, err := f.Add(ctx, "bad", KindSQLite, bad, 0); err == nil {
			t.Fatalf("path %q accepted", bad)
		}
	}
	_, parts := genParts(t, 100, 2, 1)
	runImport(t, newImporter(f), f, ids[1], parts[0])
	f.refreshSize(ctx, f.Member(ids[1]))
	if err := f.Detach(ctx, ids[1]); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("detach a serving store with contacts: %v", err)
	}
	if err := f.Update(ctx, 0, "This server", false, true, 0); err == nil {
		t.Fatal("built-in store must always serve searches")
	}
	if err := f.Update(ctx, ids[1], "Archive", false, false, 100<<20); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := f.Count(ctx, filter.Filter{}); n != 0 {
		t.Fatalf("a store that doesn't serve searches still counted: %d", n)
	}
	if err := f.Detach(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	m, err := f.Add(ctx, "Back again", KindSQLite, "extra1.db", 0) // re-attach keeps its data
	if err != nil {
		t.Fatal(err)
	}
	if m.count.Load() == 0 {
		t.Fatal("re-attached store lost its contacts")
	}
	// a restart reloads the registry, sealed connection strings included
	g := &Federation{Control: f.Control, Key: f.Key, DataDir: f.DataDir, Cache: search.NewLRU(1<<20, nil)}
	if err := g.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if len(g.Members()) != 2 || g.Member(m.ID) == nil || g.Member(m.ID).B == nil {
		t.Fatalf("registry after restart: %+v", g.Statuses(ctx))
	}
	var sealed string
	f.Control.R.QueryRow(`SELECT dsn FROM stores WHERE id=?`, m.ID).Scan(&sealed)
	if strings.Contains(sealed, "extra1") {
		t.Fatal("connection string stored in clear")
	}
	wrong := &Federation{Control: f.Control, Key: KeyFrom([]byte("other")), DataDir: f.DataDir}
	wrong.Open(ctx)
	defer wrong.Close()
	if wrong.Member(m.ID).B != nil {
		t.Fatal("wrong key must not open the store")
	}
}

func TestSealAndDisplay(t *testing.T) {
	key := KeyFrom([]byte("k"))
	dsn := "postgresql://postgres.abc:s3cr%23t@aws-0-ap-south-1.pooler.supabase.com:5432/postgres"
	s, err := seal(key, dsn)
	if err != nil || strings.Contains(s, "s3cr") {
		t.Fatalf("seal: %q %v", s, err)
	}
	if p, err := unseal(key, s); err != nil || p != dsn {
		t.Fatalf("unseal: %q %v", p, err)
	}
	if _, err := unseal(KeyFrom([]byte("x")), s); err == nil {
		t.Fatal("wrong key unsealed")
	}
	if h := DisplayHost(KindPostgres, dsn); strings.Contains(h, "s3cr") || !strings.Contains(h, "pooler.supabase.com") {
		t.Fatalf("display host %q", h)
	}
	if h := DisplayHost(KindPostgres, "host=db.example.com user=app password=hunter2 dbname=x"); strings.Contains(h, "hunter2") {
		t.Fatalf("display host %q", h)
	}
	err = redactErr("postgres://u:hunter2@h/db", errors.New(`failed to connect to user=u password=hunter2 host=h`))
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("password leaked: %v", err)
	}
	if got := ensureTLS("postgres://u:p@h/db"); !strings.HasSuffix(got, "?sslmode=require") {
		t.Fatalf("ensureTLS: %s", got)
	}
	if _, err := pgConfig("postgres://u:p@db.example.com/db?sslmode=disable"); err == nil {
		t.Fatal("plain-text connection over the internet accepted")
	}
	for _, c := range []Cursor{{}, {5, 0, 9}, {12, 3, 1 << 39}} {
		if ParseCursor(c.String()) != c {
			t.Fatalf("cursor %v", c)
		}
	}
	for _, bad := range []string{"x", "1.2", "-1.0.0", "1.a.2"} {
		if !ParseCursor(bad).IsZero() {
			t.Fatalf("bad cursor %q parsed", bad)
		}
	}
	id := GlobalID(8191, 1<<40-1)
	if s, l := SplitID(id); s != 8191 || l != 1<<40-1 || id >= 1<<53 {
		t.Fatalf("ids: %d %d %d", id, s, l)
	}
}

// ---------- Postgres (Supabase) integration ----------

// pgDSN returns the test database: DIALS_TEST_PG_DSN, or that variable in
// data/test-supabase.env at the repository root (git-ignored). The test drops
// and recreates schema fdd, so point it at a throwaway project only.
func pgDSN(t *testing.T) string {
	if v := os.Getenv("DIALS_TEST_PG_DSN"); v != "" {
		return v
	}
	dir, _ := os.Getwd()
	for i := 0; i < 5 && dir != ""; i++ {
		if fh, err := os.Open(filepath.Join(dir, "data", "test-supabase.env")); err == nil {
			defer fh.Close()
			sc := bufio.NewScanner(fh)
			for sc.Scan() {
				if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "DIALS_TEST_PG_DSN="); ok {
					return strings.Trim(v, `"'`)
				}
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("no test Postgres: set DIALS_TEST_PG_DSN or put it in data/test-supabase.env")
	return ""
}

func resetPG(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.pool.Exec(ctx, `DROP SCHEMA IF EXISTS fdd CASCADE`); err != nil {
		t.Fatal(err)
	}
}

func newPGFed(t *testing.T) (*Federation, int) {
	t.Helper()
	dsn := pgDSN(t)
	resetPG(t, dsn)
	t.Cleanup(func() { resetPG(t, dsn) })
	f, _ := newFed(t, 0)
	m, err := f.Add(context.Background(), "supabase test", KindPostgres, dsn, 500<<20)
	if err != nil {
		t.Fatal(err)
	}
	return f, m.ID
}

func TestPostgresMatchesOneDatabase(t *testing.T) {
	f, pg := newPGFed(t)
	all, parts := genParts(t, 1200, 13, 2)
	im := newImporter(f)
	runImport(t, im, f, 0, parts[0])
	runImport(t, im, f, pg, parts[1])
	ref := reference(t, all)
	sameAsReference(t, f, ref)
	claimsAndPaging(t, f)
	// whole file again into Postgres: nothing new, nothing duplicated
	before := f.Stats(context.Background()).Contacts
	if st := runImport(t, im, f, pg, all); st.Added != 0 {
		t.Fatalf("re-import added %d", st.Added)
	}
	if after := f.Stats(context.Background()).Contacts; after != before {
		t.Fatalf("contacts %d → %d", before, after)
	}
	sameAsReference(t, f, ref)
}

func TestPostgresOnlyStoreAndRemoval(t *testing.T) {
	f, pg := newPGFed(t)
	all, _ := genParts(t, 500, 21, 1)
	im := newImporter(f)
	runImport(t, im, f, pg, all)
	ctx := context.Background()
	if n, _ := f.Member(0).B.Count(ctx, filter.Filter{}); n != 0 {
		t.Fatalf("built-in store got %d contacts", n)
	}
	sameAsReference(t, f, reference(t, all))
	rows, err := f.Member(pg).B.Page(ctx, flt(t, "email=1&phone=1"), 3, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("page: %v %v", rows, err)
	}
	for _, kind := range []struct{ k, v string }{{"email", rows[0].Email}, {"phone", rows[0].Phone}} {
		ids, err := f.Member(pg).B.FindByIdentity(ctx, kind.k, kind.v)
		if err != nil || len(ids) == 0 {
			t.Fatalf("find by %s: %v %v", kind.k, ids, err)
		}
	}
	if n, err := f.Remove(ctx, "email", rows[0].Email, 1); err != nil || n != 1 {
		t.Fatalf("remove: %d %v", n, err)
	}
	if st := runImport(t, im, f, pg, all); st.Reasons["suppressed"] < 1 || st.Added != 0 {
		t.Fatalf("after removal: %+v", st)
	}
	size, err := f.Member(pg).B.Size(ctx)
	if err != nil || size <= 0 {
		t.Fatalf("size: %d %v", size, err)
	}
	if err := f.Member(pg).B.Keepalive(ctx, 123); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range f.Statuses(ctx) {
		keys = append(keys, fmt.Sprint(k))
	}
	sort.Strings(keys)
}

// counting is a backend that counts the calls it receives (from any goroutine:
// background searches and prefetches run concurrently).
type counting struct {
	Backend
	calls *atomic.Int64
}

func (c counting) Count(ctx context.Context, f filter.Filter) (int64, error) {
	c.calls.Add(1)
	return c.Backend.Count(ctx, f)
}

func (c counting) Page(ctx context.Context, f filter.Filter, offset, limit int) ([]search.Row, error) {
	c.calls.Add(1)
	return c.Backend.Page(ctx, f, offset, limit)
}

func (c counting) Facet(ctx context.Context, f filter.Filter, dim, q string, limit int) ([]FacetCount, bool, error) {
	c.calls.Add(1)
	return c.Backend.Facet(ctx, f, dim, q, limit)
}

func TestRemoteStoreAskedOnlyWhenNeeded(t *testing.T) {
	_, parts := genParts(t, 900, 17, 2)
	f, ids := newFed(t, 1)
	im := newImporter(f)
	runImport(t, im, f, ids[0], parts[0])
	runImport(t, im, f, ids[1], parts[1])
	ctx := context.Background()
	var counter atomic.Int64
	m := f.Member(ids[1])
	m.B = counting{m.B, &counter}
	calls := func() int64 { return counter.Load() }
	// totals, single filters, plain dropdowns, stats and labels: all from the local mirror
	for _, raw := range []string{"", "country=usa", "industry=software", "country=usa&country=india"} {
		if _, st, err := f.Count(ctx, flt(t, raw)); err != nil || st.Pending {
			t.Fatal(err)
		}
	}
	f.Facets(ctx, filter.Filter{}, "country", "")
	f.Facets(ctx, flt(t, "country=usa"), "country", "")
	f.Stats(ctx)
	f.Labels(ctx, "country", []string{"united states"})
	// a filter value the remote store doesn't have: never asked
	f.Count(ctx, flt(t, "country=atlantis&kw=chief"))
	if calls() != 0 {
		t.Fatalf("remote store asked %d times for answers the mirror has", calls())
	}
	// page 1 comes from the built-in store alone when it has enough rows
	pv, err := f.Preview(ctx, flt(t, "country=usa"), 0, 10, 5)
	if err != nil || len(pv.Rows) != 10 || calls() != 0 {
		t.Fatalf("first page touched the remote store: calls=%d rows=%d %v", calls(), len(pv.Rows), err)
	}
	// a two-condition count needs it once; the same question again is cached
	f.Count(ctx, flt(t, "country=usa&email=1"))
	f.Count(ctx, flt(t, "country=usa&email=1"))
	if calls() != 1 {
		t.Fatalf("remote count calls = %d, want 1", calls())
	}
	// a page past the built-in store's matches searches the remote store
	local, _ := f.Member(0).B.Count(ctx, flt(t, "kw=chief"))
	last := int(local / 10)
	before := calls()
	pv, err = f.Preview(ctx, flt(t, "kw=chief"), last, 10, 1000)
	if err != nil || pv.Pending || calls() == before {
		t.Fatalf("the next store was not searched: %+v %v", pv, err)
	}
	for _, r := range pv.Rows[int(local%10):] {
		if s, _ := SplitID(r.ID); s != ids[1] {
			t.Fatalf("row past the built-in store's matches came from store %d", s)
		}
	}
}

func TestPendingWhileNextStoreSearches(t *testing.T) {
	_, parts := genParts(t, 400, 19, 2)
	f, ids := newFed(t, 1)
	im := newImporter(f)
	runImport(t, im, f, ids[0], parts[0])
	runImport(t, im, f, ids[1], parts[1])
	ctx := context.Background()
	m := f.Member(ids[1])
	release := make(chan struct{})
	m.B = slow{m.B, release}
	f.Budget = 30 * time.Millisecond
	q := flt(t, "country=usa&email=1")
	local, _ := f.Member(0).B.Count(ctx, q)
	n, st, err := f.Count(ctx, q)
	if err != nil || !st.Pending || n != local {
		t.Fatalf("slow store: %d %+v %v (want %d, pending)", n, st, err, local)
	}
	close(release) // the background search finishes
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, st, _ = f.Count(ctx, q)
		if !st.Pending {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.Budget = -1
	want, _, _ := f.Count(ctx, q)
	if st.Pending || n != want || n <= local {
		t.Fatalf("after the background search: %d pending=%v (want %d)", n, st.Pending, want)
	}
}

// slow blocks remote counts until released.
type slow struct {
	Backend
	release chan struct{}
}

func (s slow) Count(ctx context.Context, f filter.Filter) (int64, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return s.Backend.Count(ctx, f)
}

func TestRenormalizeAcrossStores(t *testing.T) {
	_, parts := genParts(t, 400, 23, 2)
	f, ids := newFed(t, 1)
	im := newImporter(f)
	runImport(t, im, f, ids[0], parts[0])
	runImport(t, im, f, ids[1], parts[1])
	ctx := context.Background()
	extra := f.Member(ids[1]).B.(*SQLite)
	// data from before "vice president" became an alias of "vp"
	var oldID int64
	if err := extra.DB.W.QueryRow(`INSERT INTO dims(dim, value_n, label) VALUES(7, 'vice president', 'Vice President') RETURNING id`).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	extra.DB.W.Exec(`UPDATE contacts SET seniority=? WHERE id IN (SELECT id FROM contacts ORDER BY id LIMIT 5)`, oldID)
	extra.DB.W.Exec(`DELETE FROM meta WHERE k='norm_version'`)
	importer.Refresh(ctx, extra.DB)
	f.syncMirror(ctx, f.Member(ids[1]))
	before, _, _ := f.Count(ctx, flt(t, "seniority=vp"))
	n, err := f.Renormalize(ctx)
	if err != nil || n != 1 {
		t.Fatalf("renormalize: %d %v", n, err)
	}
	fr, err := f.Facets(ctx, filter.Filter{}, "seniority", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range fr.Values {
		if v.Value == "vice president" {
			t.Fatalf("old value still offered: %+v", v)
		}
	}
	after, _, _ := f.Count(ctx, flt(t, "seniority=Vice President")) // parses to "vp"
	if after != before+5 {
		t.Fatalf("vp contacts %d → %d, want +5", before, after)
	}
	if n, _ := f.Renormalize(ctx); n != 0 {
		t.Fatal("must run once per normalize.Version")
	}
}
