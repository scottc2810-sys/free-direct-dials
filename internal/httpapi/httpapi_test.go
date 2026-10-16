package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"markemind/dials/internal/auth"
	"markemind/dials/internal/config"
	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/jobs"
	"markemind/dials/internal/mail"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/search"
	"markemind/dials/internal/settings"
	"markemind/dials/internal/store"
	"markemind/dials/internal/unlock"
)

type testApp struct {
	*App
	h    http.Handler
	mail *mail.LogProvider
	clk  *platform.FakeClock
}

func newTestApp(t *testing.T, extraEnv map[string]string) *testApp {
	t.Helper()
	e := map[string]string{"DATA_DIR": t.TempDir(), "ADMIN_ALLOW_CIDRS": "203.0.113.7"}
	for k, v := range extraEnv {
		e[k] = v
	}
	cfg, err := config.Load(func(k string) string { return e[k] })
	if err != nil {
		t.Fatal(err)
	}
	d := dbtest.Open(t)
	var buf bytes.Buffer
	importer.Generate(&buf, 300, 11)
	disp := emailcheck.NewDisposable()
	if _, err := (&importer.Importer{DB: d, Disposable: disp}).Run(context.Background(), &buf, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := platform.NewFakeClock(time.Now())
	st := &settings.Store{DB: d, Clock: clk}
	st.Load(context.Background())
	lp := &mail.LogProvider{}
	mailer := &mail.Mailer{DB: d, Clock: clk, Log: log, From: mail.Address{Email: "no-reply@x.com"}, Routes: []mail.Route{{P: lp, Cap: 1000}}, ReservePct: 30}
	fed := &store.Federation{Control: d, Clock: clk, Log: log, Key: store.KeyFrom([]byte("s")), DataDir: cfg.DataDir, Cache: search.NewLRU(1<<20, nil)}
	if err := fed.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fed.Close)
	guard := &jobs.Guard{Path: ".", Stat: func(string) (platform.DiskStat, error) { return platform.DiskStat{Total: 100, Free: 90}, nil }}
	guard.Check()
	a := &App{Cfg: cfg, DB: d, Log: log, Clock: clk, Store: fed, Settings: st,
		Unlock: &unlock.Service{DB: d, Fed: fed, Settings: st, Clock: clk}, Users: &auth.Users{DB: d, Clock: clk},
		Sessions: &auth.Sessions{DB: d, Clock: clk, TTL: 720 * time.Hour}, OTP: &auth.OTP{DB: d, Clock: clk, Secret: []byte("s"), Sender: mailer},
		Admins: &auth.Admins{DB: d, Clock: clk, Idle: 30 * time.Minute, Absolute: 12 * time.Hour}, Turnstile: &auth.Turnstile{},
		Checker: &emailcheck.Checker{DB: d, Disposable: disp, Clock: clk, Log: log}, Disposable: disp, Mailer: mailer,
		Outbox: &mail.Outbox{DB: d, Mailer: mailer, Clock: clk, Log: log, URL: cfg.URL}, Importer: &importer.Importer{DB: d, Disposable: disp},
		Guard: guard, Alerter: &jobs.Alerter{Clock: clk, Log: log},
		Backup: &jobs.Backup{DBPath: d.Path, Dir: t.TempDir(), Stat: guard.Stat, Clock: clk},
		Sched:  &jobs.Scheduler{DB: d, Clock: clk, Log: log}}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	return &testApp{App: a, h: a.Handler(), mail: lp, clk: clk}
}

type req struct {
	method, path, body, ip string
	peer                   string // TCP peer; default loopback (Caddy)
	headers                map[string]string
	cookies                []*http.Cookie
}

func (ta *testApp) do(r req) *httptest.ResponseRecorder {
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	hr.RemoteAddr = "127.0.0.1:5555"
	if r.peer != "" {
		hr.RemoteAddr = r.peer
	}
	if r.ip != "" {
		hr.Header.Set("X-Real-IP", r.ip)
	}
	if r.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	for _, c := range r.cookies {
		hr.AddCookie(c)
	}
	w := httptest.NewRecorder()
	ta.h.ServeHTTP(w, hr)
	return w
}

const B = "/free-direct-dials"

func TestAdminIPAllowList(t *testing.T) {
	ta := newTestApp(t, nil)
	admin := B + "/" + ta.Cfg.AdminPath + "/"
	adminJS := ta.Assets.URL("admin.js")
	// allowed office IP (as forwarded by Caddy over loopback)
	if w := ta.do(req{method: "GET", path: admin, ip: "203.0.113.7"}); w.Code != 200 || !strings.Contains(w.Body.String(), "admin.") {
		t.Fatalf("allowed IP got %d", w.Code)
	}
	if w := ta.do(req{method: "GET", path: adminJS, ip: "203.0.113.7"}); w.Code != 200 {
		t.Fatalf("admin.js for allowed IP: %d", w.Code)
	}
	for name, r := range map[string]req{
		"other IP":           {method: "GET", path: admin, ip: "198.51.100.9"},
		"login API":          {method: "POST", path: admin + "api/login", ip: "198.51.100.9", body: `{"username":"x","password":"y","code":"1"}`},
		"session API":        {method: "GET", path: admin + "api/session", ip: "198.51.100.9"},
		"admin.js":           {method: "GET", path: adminJS, ip: "198.51.100.9"},
		"no forwarded IP":    {method: "GET", path: admin},
		"spoofed header":     {method: "GET", path: admin, ip: "203.0.113.7", peer: "198.51.100.9:443"}, // not from Caddy: header ignored
		"IPv6 neighbour":     {method: "GET", path: admin, ip: "2001:db8::1"},
		"redirect from bare": {method: "GET", path: strings.TrimSuffix(admin, "/"), ip: "198.51.100.9"},
	} {
		w := ta.do(r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", name, w.Code)
		}
		if strings.Contains(w.Body.String(), "Superadmin") {
			t.Errorf("%s: admin page leaked", name)
		}
	}
	// the public site is unaffected
	if w := ta.do(req{method: "GET", path: B + "/", ip: "198.51.100.9"}); w.Code != 200 {
		t.Fatalf("public page %d", w.Code)
	}
}

func TestAdminLoginAndCSRF(t *testing.T) {
	ta := newTestApp(t, nil)
	secret, err := ta.Admins.Create(context.Background(), "boss", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	code, _ := auth.TOTPCode(secret, ta.clk.Now().Unix()/30)
	base := B + "/" + ta.Cfg.AdminPath + "/api/"
	w := ta.do(req{method: "POST", path: base + "login", ip: "203.0.113.7", body: `{"username":"boss","password":"correct horse battery staple","code":"` + code + `"}`})
	if w.Code != 200 {
		t.Fatalf("login %d %s", w.Code, w.Body)
	}
	var s struct{ CSRF string }
	json.Unmarshal(w.Body.Bytes(), &s)
	cookies := w.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("admin cookie flags %+v", cookies)
	}
	if w := ta.do(req{method: "GET", path: base + "stats", ip: "203.0.113.7", cookies: cookies}); w.Code != 200 {
		t.Fatalf("stats %d", w.Code)
	}
	// stolen cookie from another network is useless
	if w := ta.do(req{method: "GET", path: base + "stats", ip: "198.51.100.9", cookies: cookies}); w.Code != 404 {
		t.Fatalf("cookie from foreign IP got %d", w.Code)
	}
	// mutation without the CSRF token is refused
	if w := ta.do(req{method: "POST", path: base + "cache/purge", ip: "203.0.113.7", cookies: cookies, body: `{}`}); w.Code != 403 {
		t.Fatalf("no csrf: %d", w.Code)
	}
	if w := ta.do(req{method: "POST", path: base + "cache/purge", ip: "203.0.113.7", cookies: cookies, body: `{}`, headers: map[string]string{"X-CSRF": s.CSRF}}); w.Code != 200 {
		t.Fatalf("with csrf: %d", w.Code)
	}
	if w := ta.do(req{method: "GET", path: B + "/" + ta.Cfg.AdminPath + "/", ip: "203.0.113.7"}); !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self';") {
		t.Fatalf("admin CSP %q", w.Header().Get("Content-Security-Policy"))
	}
}

func TestPublicResponsesAreCDNSafe(t *testing.T) {
	ta := newTestApp(t, nil)
	for _, p := range []string{"/", "/api/config", "/api/stats", "/api/preview?country=usa", "/api/facets?dim=country", "/sitemap.xml", "/llms.txt", "/privacy"} {
		w := ta.do(req{method: "GET", path: B + p})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
		if len(w.Result().Cookies()) > 0 {
			t.Fatalf("%s sets a cookie: a CDN would cache it for everyone", p)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "public") || !strings.Contains(cc, "s-maxage") {
			t.Fatalf("%s cache-control %q", p, cc)
		}
	}
	for _, p := range []string{"/api/me", "/api/me/view"} {
		w := ta.do(req{method: "GET", path: B + p})
		if w.Code != 401 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("%s: %d %q", p, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	w := ta.do(req{method: "GET", path: B + "/"})
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
	body := w.Body.String()
	for _, want := range []string{`"@type":"FAQPage"`, `"@type":"Dataset"`, `rel="canonical"`, "Direct dials for"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %s", want)
		}
	}
}

func TestVisitorFlowOverHTTP(t *testing.T) {
	ta := newTestApp(t, nil)
	post := func(path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		return ta.do(req{method: "POST", path: B + path, body: body, ip: "192.0.2.10", cookies: cookies, headers: map[string]string{"X-FDD": "1", "Origin": ta.Cfg.PublicOrigin}})
	}
	// cross-site POST without our header is refused
	if w := ta.do(req{method: "POST", path: B + "/api/auth/start", body: `{"email":"a@b.com"}`}); w.Code != 403 {
		t.Fatalf("csrf: %d", w.Code)
	}
	if w := post("/api/auth/start", `{"email":"x@mailinator.com","turnstile":""}`, nil); w.Code != 422 || !strings.Contains(w.Body.String(), "disposable") {
		t.Fatalf("disposable: %d %s", w.Code, w.Body)
	}
	if w := post("/api/auth/start", `{"email":"lead@acme-test.io","turnstile":""}`, nil); w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	msgs := ta.mail.Sent()
	code := strings.TrimPrefix(msgs[len(msgs)-1].Subject, "Your Free Direct Dials code: ")
	w := post("/api/auth/verify", `{"email":"lead@acme-test.io","code":"`+code+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("verify: %d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	var hasHint, hasSession bool
	for _, c := range cookies {
		hasSession = hasSession || (c.Name == sessionCookie && c.HttpOnly && c.Path == B+"/")
		hasHint = hasHint || (c.Name == hintCookie && !c.HttpOnly)
	}
	if !hasSession || !hasHint {
		t.Fatalf("cookies %+v", cookies)
	}
	w = post("/api/ad/start", `{}`, cookies)
	var tok struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &tok)
	if w := post("/api/ad/claim", `{"token":"`+tok.Token+`","filter":""}`, cookies); w.Code != http.StatusTooEarly {
		t.Fatalf("early claim: %d", w.Code)
	}
	ta.clk.Advance(11 * time.Second)
	w = post("/api/ad/claim", `{"token":"`+tok.Token+`","filter":"`+url.Values{"country": {"usa"}}.Encode()+`"}`, cookies)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"batch":1`) {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	if w := ta.do(req{method: "GET", path: B + "/api/me/export?country=usa", cookies: cookies}); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "\xef\xbb\xbfName,") {
		t.Fatalf("export: %d %.40q", w.Code, w.Body.String())
	}
}

func TestRedirectsAndAdFrame(t *testing.T) {
	ta := newTestApp(t, nil)
	if w := ta.do(req{method: "GET", path: "/free-direct-dails/country/india/?x=1"}); w.Code != 301 || w.Header().Get("Location") != "/free-direct-dials/country/india/?x=1" {
		t.Fatalf("typo redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := ta.do(req{method: "GET", path: B + "/ad-frame?slot=top"}); w.Code != 204 {
		t.Fatalf("empty slot: %d", w.Code)
	}
	ta.Settings.Save(context.Background(), "ads", []byte(`{"mode":"sponsor","smartlink":"https://asiafilm.org/4/da200810262f12a03472e20ac1c98e2e","min_seconds":10,"adblock":"nudge",
		"slots":{"top":{"enabled":true,"html":"<script>atOptions={}</script>","isolation":"sandbox","height":90}},"placements":{"header":true}}`), "t")
	w := ta.do(req{method: "GET", path: B + "/ad-frame?slot=top"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "atOptions") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Fatalf("ad frame: %d %s", w.Code, w.Header())
	}
}
