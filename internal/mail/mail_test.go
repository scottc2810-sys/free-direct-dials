package mail

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/platform"
)

func newMailer(t *testing.T, caps ...int) (*Mailer, []*LogProvider, *platform.FakeClock, *db.DB) {
	d := dbtest.Open(t)
	clk := platform.NewFakeClock(time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC))
	var routes []Route
	var ps []*LogProvider
	for i, c := range caps {
		p := &LogProvider{}
		ps = append(ps, p)
		routes = append(routes, Route{P: named{p, []string{"a", "b", "c"}[i]}, Cap: c})
	}
	m := &Mailer{DB: d, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), From: Address{"FDD", "no-reply@x.com"},
		Routes: routes, ReservePct: 30, Footer: func() Footer { return Footer{LegalName: "MarkeMind", DonateURL: "https://x/#donate"} }}
	return m, ps, clk, d
}

type named struct {
	*LogProvider
	n string
}

func (n named) Name() string { return n.n }

func TestQuotaReserveKeepsOTPsFlowing(t *testing.T) {
	m, _, clk, _ := newMailer(t, 10)
	ctx := context.Background()
	bulk := 0
	for i := 0; i < 20; i++ {
		if _, err := m.Send(ctx, Bulk, Message{To: "u@x.com"}); err == nil {
			bulk++
		} else if !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	if bulk != 7 {
		t.Fatalf("bulk sends = %d, want 7 (70%% of 10)", bulk)
	}
	for i := 0; i < 3; i++ {
		if err := m.SendOTP(ctx, "u@x.com", "123456", "login"); err != nil {
			t.Fatalf("OTP %d blocked by campaign: %v", i, err)
		}
	}
	if err := m.SendOTP(ctx, "u@x.com", "123456", "login"); !errors.Is(err, ErrQuota) {
		t.Fatalf("cap must hold, got %v", err)
	}
	clk.Advance(24 * time.Hour)
	if err := m.SendOTP(ctx, "u@x.com", "1", "login"); err != nil {
		t.Fatalf("quota resets daily: %v", err)
	}
}

func TestFailover(t *testing.T) {
	m, ps, _, _ := newMailer(t, 100, 100)
	ps[0].Fail = errors.New("503 upstream")
	if p, err := m.Send(context.Background(), Priority, Message{To: "a@b.com"}); err != nil || p != "b" {
		t.Fatalf("failover: %s %v", p, err)
	}
	ps[0].Fail = &PermanentError{errors.New("400 invalid recipient")}
	if _, err := m.Send(context.Background(), Priority, Message{To: "bad"}); !IsPermanent(err) || len(ps[1].Sent()) != 1 {
		t.Fatal("permanent errors must not fail over")
	}
	u := m.Usage(context.Background())
	if u[0].Sent != 0 || u[1].Sent != 1 {
		t.Fatalf("failed sends must release quota: %+v", u)
	}
}

func TestOTPEmailContent(t *testing.T) {
	m, ps, _, _ := newMailer(t, 100)
	m.SendOTP(context.Background(), "a@b.com", "424242", "login")
	msg := ps[0].Sent()[0]
	if !strings.Contains(msg.Subject, "424242") || !strings.Contains(msg.HTML, "424242") || !strings.Contains(msg.Text, "424242") {
		t.Fatal("code missing from OTP email")
	}
	if !strings.Contains(msg.HTML, "https://x/#donate") {
		t.Fatal("donate footer missing")
	}
}

func TestOutboxNotifyOnceAndUnsubscribeHeaders(t *testing.T) {
	m, ps, clk, d := newMailer(t, 1000)
	ctx := context.Background()
	for i, e := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		d.W.Exec(`INSERT INTO users(email, created, last_seen, subscribed, unsub_token) VALUES(?,?,?,?,?)`, e, 0, i, 1, "tok_"+e+"_0123456789")
	}
	d.W.Exec(`UPDATE users SET subscribed=0 WHERE email='c@x.com'`)
	d.W.Exec(`INSERT INTO uploads(id, ts, filename, added, status) VALUES(1, 0, 'f.csv', 1513, 'done')`)
	ob := &Outbox{DB: d, Mailer: m, Clock: clk, Log: m.Log, URL: func(p string) string { return "https://site/fdd" + p }, Pace: -1}
	cid, total, err := ob.Enqueue(ctx, 1, "", "")
	if err != nil || total != 2 {
		t.Fatalf("enqueue: %v total=%d", err, total)
	}
	if _, _, err := ob.Enqueue(ctx, 1, "", ""); !errors.Is(err, ErrAlreadyNotified) {
		t.Fatalf("double notify allowed: %v", err)
	}
	n, err := ob.Tick(ctx, 10)
	if err != nil || n != 2 {
		t.Fatalf("tick sent %d err %v", n, err)
	}
	msg := ps[0].Sent()[0]
	if !strings.Contains(msg.Subject, "1,513") {
		t.Fatalf("subject %q must carry uploads.added (acceptance #10)", msg.Subject)
	}
	if msg.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" || !strings.Contains(msg.Headers["List-Unsubscribe"], "/api/unsub?t=tok_") {
		t.Fatalf("one-click unsubscribe headers missing: %+v", msg.Headers)
	}
	if !strings.Contains(msg.HTML, "utm_campaign=c") || cid == 0 {
		t.Fatal("campaign link missing")
	}
	st := ob.Status(ctx)
	if st.Sent != 2 || st.Queued != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestOutboxQuotaPausesUntilTomorrow(t *testing.T) {
	m, _, clk, d := newMailer(t, 10) // bulk cap 7
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		d.W.Exec(`INSERT INTO users(email, created, last_seen, subscribed, unsub_token) VALUES(?,0,0,1,?)`, string(rune('a'+i))+"@x.com", "t0123456789abcdef"+string(rune('a'+i)))
	}
	d.W.Exec(`INSERT INTO uploads(id, ts, filename, added, status) VALUES(1, 0, 'f.csv', 5, 'done')`)
	ob := &Outbox{DB: d, Mailer: m, Clock: clk, Log: m.Log, URL: func(p string) string { return p }, Pace: -1}
	ob.Enqueue(ctx, 1, "", "")
	n, _ := ob.Tick(ctx, 50)
	if n != 7 {
		t.Fatalf("sent %d, want 7 before quota", n)
	}
	if n, _ := ob.Tick(ctx, 50); n != 0 {
		t.Fatal("must wait for tomorrow")
	}
	if err := m.SendOTP(ctx, "z@x.com", "1", "login"); err != nil {
		t.Fatal("OTP must still work after the campaign hit its share")
	}
	clk.Advance(15 * time.Hour)
	if n, _ := ob.Tick(ctx, 50); n != 5 {
		t.Fatalf("next day sent %d, want remaining 5", n)
	}
}

func TestMIMEHeaderInjection(t *testing.T) {
	b := string(BuildMIME(Address{"A", "a@x.com"}, Message{To: "b@x.com", Subject: "hi\r\nBcc: evil@x.com", Text: "t", HTML: "<p>h</p>",
		Headers: map[string]string{"List-Unsubscribe": "<https://x>\r\nBcc: evil@x.com"}}, time.Unix(0, 0)))
	if strings.Contains(b, "\r\nBcc:") {
		t.Fatal("header injection")
	}
}
