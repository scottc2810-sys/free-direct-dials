package emailcheck

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/platform"
)

func TestSyntax(t *testing.T) {
	good := map[string]string{
		"John.Doe@Example.COM":      "john.doe@example.com",
		" a+tag@sub.example.co.uk ": "a+tag@sub.example.co.uk",
		"user@münchen.de":           "user@xn--mnchen-3ya.de",
		"o'neil@example.io":         "o'neil@example.io",
	}
	for in, want := range good {
		got, ok, why := Syntax(in)
		if !ok || got != want {
			t.Errorf("Syntax(%q) = %q,%v,%s want %q", in, got, ok, why, want)
		}
	}
	bad := []string{"", "plain", "a@b", "a@@b.com", ".a@b.com", "a.@b.com", "a..b@c.com", "a b@c.com",
		"a@-b.com", "a@b-.com", "a@[1.2.3.4]", "a@b.c", "a@b.123", "\"q\"@b.com", "a@b..com",
		"x@" + string(make([]byte, 300)), "\"; DROP TABLE users; --@x.com"}
	for _, in := range bad {
		if _, ok, _ := Syntax(in); ok {
			t.Errorf("Syntax(%q) accepted", in)
		}
	}
}

func TestDisposable(t *testing.T) {
	d := NewDisposable()
	// Acceptance #8
	for _, dom := range []string{"mailinator.com", "sub.guerrillamail.com", "x.y.yopmail.fr", "my-tempmail-box.net", "10minutemail.co.uk"} {
		if !d.IsDisposable(dom) {
			t.Errorf("%s should be disposable", dom)
		}
	}
	for _, dom := range []string{"gmail.com", "hotmail.com", "markemindai.com", "mailbox.org", "tempo.com"} {
		if d.IsDisposable(dom) {
			t.Errorf("%s falsely disposable", dom)
		}
	}
}

type fakeResolver struct {
	mx    map[string][]*net.MX
	hosts map[string][]string
	err   error
	calls int
}

func (f *fakeResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.mx[name]; ok {
		return m, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeResolver) LookupHost(ctx context.Context, name string) ([]string, error) {
	if h, ok := f.hosts[name]; ok {
		return h, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func newChecker(t *testing.T, r Resolver) *Checker {
	return &Checker{
		DB: dbtest.Open(t), Disposable: NewDisposable(), Resolver: r,
		Clock: platform.NewFakeClock(time.Unix(1_800_000_000, 0)),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestCheckerRules(t *testing.T) {
	r := &fakeResolver{
		mx: map[string][]*net.MX{
			"good.com":   {{Host: "mx.good.com.", Pref: 10}},
			"nullmx.com": {{Host: ".", Pref: 0}},
		},
		hosts: map[string][]string{"amx.com": {"1.2.3.4"}},
	}
	c := newChecker(t, r)
	ctx := context.Background()
	cases := map[string]bool{
		"a@good.com":              true,
		"a@nullmx.com":            false,
		"a@amx.com":               true, // implicit MX
		"a@nodomain.com":          false,
		"test@mailinator.com":     false,
		"x@sub.guerrillamail.com": false,
		"bad syntax@x.com":        false,
	}
	for in, want := range cases {
		if _, v := c.Check(ctx, in); v.OK != want {
			t.Errorf("Check(%q) = %+v want ok=%v", in, v, want)
		}
	}
	// cached: no new DNS call for the same domain
	before := r.calls
	c.Check(ctx, "b@good.com")
	if r.calls != before {
		t.Errorf("mx cache not used")
	}
	// business-email rule
	c.RequireBusiness = func() bool { return true }
	r.mx["gmail.com"] = []*net.MX{{Host: "gmail-smtp-in.l.google.com."}}
	if _, v := c.Check(ctx, "someone@gmail.com"); v.OK || v.Reason != "free_provider" {
		t.Errorf("free provider not blocked: %+v", v)
	}
}

func TestDNSFailOpen(t *testing.T) {
	c := newChecker(t, &fakeResolver{err: &net.DNSError{Err: "timeout", IsTimeout: true}})
	if _, v := c.Check(context.Background(), "a@flaky.com"); !v.OK {
		t.Fatalf("DNS timeout must fail open, got %+v", v)
	}
	var n int
	c.DB.R.QueryRow(`SELECT count(*) FROM mx_cache`).Scan(&n)
	if n != 0 {
		t.Fatal("inconclusive DNS must not be cached")
	}
}

func TestReacher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-reacher-secret") != "s3" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"is_reachable":"invalid","misc":{"is_disposable":false},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`))
	}))
	defer srv.Close()
	rc := NewReacher(srv.URL, "s3")
	v, err := rc.Check(context.Background(), "a@good.com")
	if err != nil || v.OK || v.Reason != "undeliverable" {
		t.Fatalf("reacher verdict %+v err %v", v, err)
	}
	c := newChecker(t, &fakeResolver{mx: map[string][]*net.MX{"good.com": {{Host: "mx."}}}})
	c.Reacher = NewReacher("http://127.0.0.1:1", "") // unreachable → fail open
	if _, v := c.Check(context.Background(), "z@good.com"); !v.OK {
		t.Fatalf("reacher down must fail open: %+v", v)
	}
}
