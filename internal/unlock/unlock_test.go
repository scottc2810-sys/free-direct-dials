package unlock

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/settings"
	"markemind/dials/internal/store"
)

type env struct {
	svc *Service
	clk *platform.FakeClock
}

func newEnv(t *testing.T, rows int) env {
	d := dbtest.Open(t)
	var buf bytes.Buffer
	importer.Generate(&buf, rows, 9)
	buf.WriteString(`Ann,=HYPERLINK("x"),CEO,,,,Acme,acme.com,,ann@acme.com,,+1 555 123 4567,,,Austin,,USA,Software,,,,,,,` + "\n")
	im := &importer.Importer{DB: d, Disposable: emailcheck.NewDisposable()}
	if _, err := im.Run(context.Background(), &buf, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	clk := platform.NewFakeClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	st := &settings.Store{DB: d, Clock: clk}
	st.Load(context.Background())
	for i := 1; i <= 3; i++ {
		d.W.Exec(`INSERT INTO users(id, email, created, last_seen, unsub_token) VALUES(?,?,0,0,?)`, i, string(rune('a'+i))+"@x.com", "tok"+string(rune('a'+i))+"0123456789abcdef")
	}
	return env{svc: &Service{DB: d, Fed: store.NewLocal(d), Settings: st, Clock: clk}, clk: clk}
}

func f(t *testing.T, raw string) filter.Filter {
	q, _ := url.ParseQuery(raw)
	x, err := filter.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (e env) watch(t *testing.T, uid int64) string {
	t.Helper()
	tok, err := e.svc.Start(context.Background(), uid, "")
	if err != nil {
		t.Fatal(err)
	}
	return tok.ID
}

func TestAcceptance9TokenRules(t *testing.T) {
	e := newEnv(t, 200)
	ctx := context.Background()
	tok := e.watch(t, 1)
	e.clk.Advance(5 * time.Second)
	var ts *TooSoonError
	if _, err := e.svc.Claim(ctx, 1, tok, f(t, ""), ""); !errors.As(err, &ts) {
		t.Fatalf("token < 9 s must be rejected, got %v", err)
	}
	e.clk.Advance(4 * time.Second) // 9 s: allowed (MinSeconds-1 for clock skew)
	if _, err := e.svc.Claim(ctx, 2, tok, f(t, ""), ""); err != ErrTokenInvalid {
		t.Fatalf("another user's token must be rejected, got %v", err)
	}
	res, err := e.svc.Claim(ctx, 1, tok, f(t, ""), "")
	if err != nil || len(res.Rows) != 10 || res.Batch != 1 {
		t.Fatalf("claim: %v rows=%d batch=%d", err, len(res.Rows), res.Batch)
	}
	if _, err := e.svc.Claim(ctx, 1, tok, f(t, ""), ""); err != ErrTokenUsed {
		t.Fatalf("reused token must be rejected, got %v", err)
	}
	if _, err := e.svc.Claim(ctx, 1, "nonexistent-token-xx", f(t, ""), ""); err != ErrTokenInvalid {
		t.Fatal("unknown token accepted")
	}
	for _, row := range res.Rows {
		if row.Masked || !row.New {
			t.Fatalf("claimed rows must be real and NEW: %+v", row)
		}
	}
}

func TestAcceptance6ConcurrentTabsNoOverlap(t *testing.T) {
	e := newEnv(t, 300)
	ctx := context.Background()
	toks := []string{e.watch(t, 1), e.watch(t, 1), e.watch(t, 1)}
	e.clk.Advance(10 * time.Second)
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	errs := make(chan error, len(toks))
	for _, tok := range toks {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			res, err := e.svc.Claim(ctx, 1, tok, f(t, ""), "")
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			for _, r := range res.Rows {
				seen[r.ID]++
			}
			mu.Unlock()
		}(tok)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(seen) != 30 {
		t.Fatalf("got %d distinct contacts, want 30", len(seen))
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("contact %d handed out %d times", id, n)
		}
	}
}

func TestAcceptance7FilterChangeNeverReissues(t *testing.T) {
	e := newEnv(t, 400)
	ctx := context.Background()
	tok := e.watch(t, 1)
	e.clk.Advance(10 * time.Second)
	first, err := e.svc.Claim(ctx, 1, tok, f(t, "country=usa"), "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range first.Rows {
		got[r.ID] = true
	}
	// a broader filter that includes the already-unlocked USA rows
	v, err := e.svc.View(ctx, 1, f(t, "industry=software"), false, "")
	if err != nil {
		t.Fatal(err)
	}
	tok = e.watch(t, 1)
	e.clk.Advance(10 * time.Second)
	second, err := e.svc.Claim(ctx, 1, tok, f(t, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range second.Rows {
		if got[r.ID] {
			t.Fatalf("contact %d re-issued after filter change", r.ID)
		}
	}
	if second.Batch != 2 {
		t.Fatalf("batch = %d", second.Batch)
	}
	all, _ := e.svc.View(ctx, 1, f(t, ""), false, "")
	if all.Unlocked != 20 || len(all.Rows) != 20 || all.Rows[0].Batch != 2 || !all.Rows[0].New || all.Rows[19].New {
		t.Fatalf("view: unlocked=%d rows=%d first=%+v", all.Unlocked, len(all.Rows), all.Rows[0])
	}
	newest, _ := e.svc.View(ctx, 1, f(t, ""), true, "")
	if len(newest.Rows) != 10 {
		t.Fatalf("only-newest view = %d rows", len(newest.Rows))
	}
	_ = v
}

func TestNothingLeftKeepsToken(t *testing.T) {
	e := newEnv(t, 50)
	ctx := context.Background()
	narrow := f(t, "kw=ann&company=acme&country=usa&industry=software")
	tok := e.watch(t, 1)
	e.clk.Advance(10 * time.Second)
	res, err := e.svc.Claim(ctx, 1, tok, narrow, "")
	if err != nil || len(res.Rows) == 0 || res.Remaining != 0 {
		t.Fatalf("claim narrow: %v %+v", err, res)
	}
	tok = e.watch(t, 1)
	e.clk.Advance(10 * time.Second)
	if _, err := e.svc.Claim(ctx, 1, tok, narrow, ""); err != ErrNothingLeft {
		t.Fatalf("want ErrNothingLeft, got %v", err)
	}
	if _, err := e.svc.Claim(ctx, 1, tok, f(t, ""), ""); err != nil {
		t.Fatalf("token must survive a nothing-left claim: %v", err)
	}
	if _, err := e.svc.Claim(ctx, 1, e.watch(t, 1), f(t, "country=atlantis"), ""); err == nil {
		t.Fatal("impossible filter must not claim")
	}
}

func TestDailyCaps(t *testing.T) {
	e := newEnv(t, 300)
	ctx := context.Background()
	e.svc.Settings.Save(ctx, "limits", []byte(`{"contacts_per_ad":5,"max_ads_per_day":2,"max_ads_per_day_ip":3,"page_size":50,"preview_pages":5,"signups_enabled":true}`), "t")
	for i := 0; i < 2; i++ {
		tok := e.watch(t, 1)
		e.clk.Advance(10 * time.Second)
		if res, err := e.svc.Claim(ctx, 1, tok, f(t, ""), "9.9.9.9"); err != nil || len(res.Rows) != 5 {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	if _, err := e.svc.Start(ctx, 1, "9.9.9.9"); err != ErrDailyLimit {
		t.Fatalf("user cap: %v", err)
	}
	tok := e.watch(t, 2)
	e.clk.Advance(10 * time.Second)
	e.svc.Claim(ctx, 2, tok, f(t, ""), "9.9.9.9") // 3rd from this IP
	if _, err := e.svc.Start(ctx, 3, "9.9.9.9"); err != ErrIPLimit {
		t.Fatalf("ip cap: %v", err)
	}
	e.clk.Advance(24 * time.Hour)
	if _, err := e.svc.Start(ctx, 1, "9.9.9.9"); err != nil {
		t.Fatalf("caps reset next day: %v", err)
	}
}

func TestExportFormulaSafe(t *testing.T) {
	e := newEnv(t, 30)
	ctx := context.Background()
	tok := e.watch(t, 1)
	e.clk.Advance(10 * time.Second)
	if _, err := e.svc.Claim(ctx, 1, tok, f(t, "kw=ann"), ""); err != nil {
		t.Fatal(err)
	}
	tok = e.watch(t, 1) // plus generated rows, which carry a Zip Code extra column
	e.clk.Advance(10 * time.Second)
	if _, err := e.svc.Claim(ctx, 1, tok, f(t, ""), ""); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, err := e.svc.Export(ctx, 1, f(t, ""), &buf)
	if err != nil || n == 0 {
		t.Fatalf("export: %d %v", n, err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs[1:] {
		for _, cell := range rec {
			if cell != "" && strings.ContainsRune("=+-@", rune(cell[0])) {
				t.Fatalf("unsafe cell %q", cell)
			}
		}
	}
	if !strings.Contains(strings.Join(recs[0], ","), "Zip Code") {
		t.Fatalf("extra columns missing from header: %v", recs[0])
	}
	if SafeCell("+1 555") != "'+1 555" || SafeCell("ok") != "ok" {
		t.Fatal("SafeCell")
	}
}

func TestViewPagesThroughEverything(t *testing.T) {
	e := newEnv(t, 300)
	ctx := context.Background()
	e.svc.Settings.Save(ctx, "limits", []byte(`{"contacts_per_ad":10,"max_ads_per_day":50,"max_ads_per_day_ip":150,"page_size":12,"preview_pages":5,"signups_enabled":true}`), "t")
	for i := 0; i < 4; i++ {
		tok := e.watch(t, 1)
		e.clk.Advance(10 * time.Second)
		if _, err := e.svc.Claim(ctx, 1, tok, f(t, ""), ""); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int64]bool{}
	cur, pages := "", 0
	lastBatch := int64(1 << 60)
	for {
		v, err := e.svc.View(ctx, 1, f(t, ""), false, cur)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if v.Unlocked != 40 || v.PageSize != 12 {
			t.Fatalf("unlocked=%d page=%d", v.Unlocked, v.PageSize)
		}
		for _, r := range v.Rows {
			if seen[r.ID] {
				t.Fatalf("row %d on two pages", r.ID)
			}
			if r.Batch > lastBatch {
				t.Fatalf("pages out of order: batch %d after %d", r.Batch, lastBatch)
			}
			seen[r.ID], lastBatch = true, r.Batch
		}
		if v.Next == "" {
			break
		}
		cur = v.Next
	}
	if len(seen) != 40 || pages != 4 {
		t.Fatalf("paged %d rows in %d pages, want 40 in 4", len(seen), pages)
	}
}
