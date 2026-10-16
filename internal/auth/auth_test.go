package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/platform"
)

type captureSender struct {
	mu    sync.Mutex
	codes map[string]string
}

func (c *captureSender) SendOTP(_ context.Context, to, code, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codes == nil {
		c.codes = map[string]string{}
	}
	c.codes[to] = code
	return nil
}

func newOTP(t *testing.T) (*OTP, *captureSender, *platform.FakeClock) {
	clk := platform.NewFakeClock(time.Unix(1_800_000_000, 0))
	s := &captureSender{}
	return &OTP{DB: dbtest.Open(t), Clock: clk, Secret: []byte("k"), Sender: s}, s, clk
}

func TestOTPLockoutAcceptance8(t *testing.T) {
	o, s, _ := newOTP(t)
	ctx := context.Background()
	if err := o.Start(ctx, "a@b.com", "login"); err != nil {
		t.Fatal(err)
	}
	good := s.codes["a@b.com"]
	wrong := "000000"
	if good == wrong {
		wrong = "111111"
	}
	for i := 1; i <= 5; i++ {
		var ice *InvalidCodeError
		if err := o.Verify(ctx, "a@b.com", "login", wrong); !errors.As(err, &ice) || ice.Remaining != 5-i {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := o.Verify(ctx, "a@b.com", "login", good); err != ErrLocked {
		t.Fatalf("6th attempt must be locked even with the right code, got %v", err)
	}
}

func TestOTPSuccessExpiryAndSendLimit(t *testing.T) {
	o, s, clk := newOTP(t)
	ctx := context.Background()
	o.Start(ctx, "x@y.com", "login")
	if err := o.Verify(ctx, "x@y.com", "login", " "+s.codes["x@y.com"][:3]+"-"+s.codes["x@y.com"][3:]); err != nil {
		t.Fatalf("formatted code should verify: %v", err)
	}
	if err := o.Verify(ctx, "x@y.com", "login", s.codes["x@y.com"]); err != ErrNoCode {
		t.Fatalf("code must be single-use, got %v", err)
	}
	o.Start(ctx, "x@y.com", "login")
	clk.Advance(11 * time.Minute)
	if err := o.Verify(ctx, "x@y.com", "login", s.codes["x@y.com"]); err != ErrExpired {
		t.Fatalf("want expired, got %v", err)
	}
	if err := o.Start(ctx, "x@y.com", "login"); err != nil { // 3rd send in the hour: allowed
		t.Fatal(err)
	}
	if err := o.Start(ctx, "x@y.com", "login"); err != ErrTooManySends {
		t.Fatalf("4th send within the hour must be refused, got %v", err)
	}
	clk.Advance(61 * time.Minute)
	if err := o.Start(ctx, "x@y.com", "login"); err != nil {
		t.Fatalf("window should reset: %v", err)
	}
	// purposes are independent
	if err := o.Start(ctx, "x@y.com", "remove"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsSlide(t *testing.T) {
	d := dbtest.Open(t)
	clk := platform.NewFakeClock(time.Unix(1_800_000_000, 0))
	ss := &Sessions{DB: d, Clock: clk, TTL: 30 * 24 * time.Hour}
	us := &Users{DB: d, Clock: clk}
	u, err := us.Upsert(context.Background(), "a@b.com", true)
	if err != nil || !u.Subscribed || u.UnsubToken == "" {
		t.Fatalf("upsert %+v %v", u, err)
	}
	u2, _ := us.Upsert(context.Background(), "a@b.com", false)
	if u2.ID != u.ID || !u2.Subscribed {
		t.Fatal("re-sign-in must not change identity or subscription")
	}
	tok, _, _ := ss.Create(context.Background(), u.ID)
	if id, _, ref, err := ss.Lookup(context.Background(), tok); err != nil || id != u.ID || ref {
		t.Fatalf("lookup: %v %v", err, ref)
	}
	clk.Advance(2 * 24 * time.Hour)
	if _, exp, ref, _ := ss.Lookup(context.Background(), tok); !ref || exp.Before(clk.Now().Add(29*24*time.Hour)) {
		t.Fatal("session should slide after a day")
	}
	clk.Advance(31 * 24 * time.Hour)
	if _, _, _, err := ss.Lookup(context.Background(), tok); err == nil {
		t.Fatal("expired session accepted")
	}
	if _, _, _, err := ss.Lookup(context.Background(), "short"); err == nil {
		t.Fatal("garbage token accepted")
	}
	if email, err := us.Unsubscribe(context.Background(), u.UnsubToken); err != nil || email != "a@b.com" {
		t.Fatal("unsubscribe by token")
	}
}

func TestTOTPRFC6238(t *testing.T) {
	// RFC 6238 test vector (SHA-1, secret "12345678901234567890"), last 6 digits.
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		got, _ := TOTPCode(secret, ts/30)
		if got != want {
			t.Errorf("TOTP(%d) = %s want %s", ts, got, want)
		}
	}
	now := time.Unix(1234567890, 0)
	step, ok := VerifyTOTP(secret, "005924", now, 0)
	if !ok {
		t.Fatal("valid code rejected")
	}
	if _, ok := VerifyTOTP(secret, "005924", now, step); ok {
		t.Fatal("replayed code accepted")
	}
}

func TestAdminLoginLockout(t *testing.T) {
	d := dbtest.Open(t)
	clk := platform.NewFakeClock(time.Unix(1_900_000_000, 0))
	a := &Admins{DB: d, Clock: clk, Idle: 30 * time.Minute, Absolute: 12 * time.Hour}
	ctx := context.Background()
	if _, err := a.Create(ctx, "boss", "short"); err == nil {
		t.Fatal("weak password accepted")
	}
	secret, err := a.Create(ctx, "Boss", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	code := func() string { c, _ := TOTPCode(secret, clk.Now().Unix()/30); return c }
	for i := 0; i < 5; i++ {
		if _, _, err := a.Login(ctx, "boss", "wrong password!!", code(), "1.1.1.1"); err != ErrBadLogin {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, _, err := a.Login(ctx, "boss", "correct horse battery staple", code(), "1.1.1.1"); err != ErrAdminLocked {
		t.Fatalf("want locked, got %v", err)
	}
	clk.Advance(16 * time.Minute)
	tok, s, err := a.Login(ctx, "BOSS", "correct horse battery staple", code(), "1.1.1.1")
	if err != nil || s.CSRF == "" {
		t.Fatalf("login after lockout: %v", err)
	}
	if _, err := a.Session(ctx, tok); err != nil {
		t.Fatal(err)
	}
	clk.Advance(31 * time.Minute)
	if _, err := a.Session(ctx, tok); err == nil {
		t.Fatal("idle session must expire")
	}
	if _, _, err := a.Login(ctx, "nobody", "whatever-password", "123456", "1.1.1.1"); err != ErrBadLogin {
		t.Fatal("unknown user must look like a bad login")
	}
}

func TestLimiter(t *testing.T) {
	clk := platform.NewFakeClock(time.Unix(0, 0))
	l := NewLimiter(3, time.Hour, 2, clk.Now)
	for i := 0; i < 3; i++ {
		if !l.Allow("ip") {
			t.Fatal("burst refused")
		}
	}
	ok, wait := l.Take("ip")
	if ok || wait <= 0 {
		t.Fatal("4th must be refused with a wait")
	}
	clk.Advance(20 * time.Minute)
	if !l.Allow("ip") {
		t.Fatal("token should have refilled")
	}
	l.Allow("a")
	l.Allow("b") // exceeds maxKeys → gc keeps memory bounded
	if len(l.m) > 2 {
		t.Fatalf("limiter grew to %d keys", len(l.m))
	}
}

func TestTurnstile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("response") == "good" {
			w.Write([]byte(`{"success":true,"hostname":"markemindai.com","action":"signup"}`))
			return
		}
		w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	defer srv.Close()
	ts := NewTurnstile("secret", "markemindai.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts.Endpoint = srv.URL
	ctx := context.Background()
	if err := ts.Verify(ctx, "good", "1.2.3.4", "signup"); err != nil {
		t.Fatal(err)
	}
	if err := ts.Verify(ctx, "bad", "", "signup"); err != ErrTurnstile {
		t.Fatal("bad token accepted")
	}
	if err := ts.Verify(ctx, "good", "", "other"); err != ErrTurnstile {
		t.Fatal("wrong action accepted")
	}
	ts.Endpoint = "http://127.0.0.1:1"
	if err := ts.Verify(ctx, "x", "", ""); err != nil {
		t.Fatal("outage must fail open")
	}
	if err := (&Turnstile{}).Verify(ctx, "", "", ""); err != nil || strings.Contains("", "x") {
		t.Fatal("disabled turnstile must pass")
	}
}

type flakySender struct{ fail bool }

func (f *flakySender) SendOTP(context.Context, string, string, string) error {
	if f.fail {
		return errors.New("provider down")
	}
	return nil
}

func TestOTPSendFailureDoesNotConsumeAllowance(t *testing.T) {
	fs := &flakySender{fail: true}
	o := &OTP{DB: dbtest.Open(t), Clock: platform.NewFakeClock(time.Unix(1_800_000_000, 0)), Secret: []byte("k"), Sender: fs}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := o.Start(ctx, "a@b.com", "login"); err == nil || err == ErrTooManySends {
			t.Fatalf("attempt %d: want provider error, got %v", i, err)
		}
	}
	fs.fail = false
	if err := o.Start(ctx, "a@b.com", "login"); err != nil {
		t.Fatalf("after the outage the visitor must still get a code: %v", err)
	}
}
