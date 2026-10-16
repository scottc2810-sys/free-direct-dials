package jobs

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/db/dbtest"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/platform"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func fakeDisk(used *atomic.Int64) platform.DiskStatFunc {
	return func(string) (platform.DiskStat, error) {
		return platform.DiskStat{Total: 100 << 30, Free: uint64(100-used.Load()) << 30}, nil
	}
}

func TestAcceptance11DiskGuard(t *testing.T) {
	var used atomic.Int64
	var alerts []string
	clk := platform.NewFakeClock(time.Unix(0, 0))
	al := &Alerter{Clock: clk, Log: quiet, Send: func(_ context.Context, s, _ string) error { alerts = append(alerts, s); return nil }}
	g := &Guard{Path: ".", Stat: fakeDisk(&used), OnChange: func(o, n int, p float64) {
		if n > o {
			al.Alert(context.Background(), "disk", "Disk at high usage", "")
		}
	}}
	used.Store(50)
	g.Check()
	if g.AllowHeavy() != nil || g.AllowWrite() != nil {
		t.Fatal("50% must allow everything")
	}
	used.Store(85)
	g.Check()
	if len(alerts) != 1 || g.AllowHeavy() != nil {
		t.Fatalf("80%%: alert expected, uploads still allowed (alerts=%v)", alerts)
	}
	used.Store(91)
	g.Check()
	if g.AllowHeavy() != ErrDiskFull || g.AllowWrite() != nil {
		t.Fatal("90%: uploads/backups off, reads + small writes on")
	}
	used.Store(96)
	g.Check()
	if g.AllowWrite() != ErrReadOnly {
		t.Fatal("95%: read-only")
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts must be rate limited, got %d", len(alerts))
	}
	used.Store(40)
	g.Check()
	if g.AllowWrite() != nil || g.AllowHeavy() != nil {
		t.Fatal("recovery must re-enable writes")
	}
	g.Manual = func() bool { return true }
	if g.AllowWrite() != ErrReadOnly {
		t.Fatal("manual maintenance switch")
	}
}

func seeded(t *testing.T) *db.DB {
	d := dbtest.Open(t)
	var buf bytes.Buffer
	importer.Generate(&buf, 500, 3)
	if _, err := (&importer.Importer{DB: d, Disposable: emailcheck.NewDisposable()}).Run(context.Background(), &buf, importer.Options{}); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBackupRestoreAndRetention(t *testing.T) {
	d := seeded(t)
	var used atomic.Int64
	used.Store(10)
	clk := platform.NewFakeClock(time.Date(2026, 10, 15, 3, 30, 0, 0, time.UTC))
	dir := t.TempDir()
	b := &Backup{DBPath: d.Path, Dir: dir, Keep: 2, Stat: fakeDisk(&used), Clock: clk, Guard: &Guard{Stat: fakeDisk(&used)}}
	var last string
	for i := 0; i < 3; i++ {
		p, err := b.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		last = p
		clk.Advance(24 * time.Hour)
	}
	if n := len(b.List()); n != 2 {
		t.Fatalf("retention kept %d backups, want 2", n)
	}
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := Restore(last, restored, nil); err != nil {
		t.Fatal(err)
	}
	r, err := db.Open(context.Background(), restored, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var a, c int
	d.R.QueryRow(`SELECT count(*) FROM contacts`).Scan(&a)
	r.R.QueryRow(`SELECT count(*) FROM contacts`).Scan(&c)
	if a == 0 || a != c {
		t.Fatalf("restored %d contacts, original %d", c, a)
	}
	// guard blocks backups under disk pressure
	used.Store(92)
	b.Guard.Check()
	if _, err := b.Run(context.Background()); err != ErrDiskFull {
		t.Fatalf("want ErrDiskFull, got %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".db" || filepath.Ext(e.Name()) == ".part" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestMaintenanceJobs(t *testing.T) {
	d := seeded(t)
	clk := platform.NewFakeClock(time.Now())
	ctx := context.Background()
	old := clk.Now().Add(-48 * time.Hour)
	d.W.Exec(`INSERT INTO sessions(id, user_id, created, exp, last_refresh) VALUES('s',1,0,?,0)`, old.Unix())
	d.W.Exec(`INSERT INTO ad_tokens(id, user_id, created, mode) VALUES('t',1,?, 'sponsor')`, old.UnixMilli())
	d.W.Exec(`INSERT INTO verdicts(email, ok, reason, ts) VALUES('a@b.c',1,'mx',?)`, old.Add(-40*24*time.Hour).Unix())
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "upload-old"), []byte("x"), 0o600)
	os.Chtimes(filepath.Join(tmp, "upload-old"), old, old)
	if err := Hourly(ctx, d, clk, tmp); err != nil {
		t.Fatal(err)
	}
	if err := Daily(ctx, d, clk); err != nil {
		t.Fatal(err)
	}
	var var1, var2, var3 int
	d.R.QueryRow(`SELECT count(*) FROM sessions`).Scan(&var1)
	d.R.QueryRow(`SELECT count(*) FROM ad_tokens`).Scan(&var2)
	d.R.QueryRow(`SELECT count(*) FROM verdicts`).Scan(&var3)
	if var1+var2+var3 != 0 {
		t.Fatalf("expired rows survived: %d %d %d", var1, var2, var3)
	}
	if _, err := os.Stat(filepath.Join(tmp, "upload-old")); !os.IsNotExist(err) {
		t.Fatal("stale upload not removed")
	}
	var used atomic.Int64
	if err := Monthly(ctx, d, clk, fakeDisk(&used), quiet); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerNoOverlapAndStatus(t *testing.T) {
	d := dbtest.Open(t)
	clk := platform.NewFakeClock(time.Date(2026, 1, 1, 2, 59, 0, 0, time.UTC))
	s := &Scheduler{DB: d, Clock: clk, Log: quiet}
	block := make(chan struct{})
	var runs atomic.Int32
	s.Add(&Job{Name: "slow", Every: time.Minute, Fn: func(ctx context.Context) error { runs.Add(1); <-block; return nil }})
	s.Add(&Job{Name: "nightly", Daily: true, Hour: 3, Fn: func(context.Context) error { runs.Add(10); return nil }})
	ctx := context.Background()
	clk.Advance(2 * time.Minute)
	s.tick(ctx)
	clk.Advance(2 * time.Minute)
	s.tick(ctx) // slow is still running: must not start a second copy
	if ok, _ := s.RunNow(ctx, "slow"); ok {
		t.Fatal("RunNow must refuse while running")
	}
	close(block)
	s.wg.Wait()
	if got := runs.Load(); got != 11 {
		t.Fatalf("runs = %d, want 1 slow + 1 nightly (=11)", got)
	}
	st := s.Status(ctx)
	if len(st) != 2 || !st[0].OK {
		t.Fatalf("status %+v", st)
	}
}

func TestEncryptedBackupRoundTripAndTamper(t *testing.T) {
	d := seeded(t)
	var used atomic.Int64
	key, _ := ParseKey("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	clk := platform.NewFakeClock(time.Date(2026, 10, 15, 3, 30, 0, 0, time.UTC))
	b := &Backup{DBPath: d.Path, Dir: t.TempDir(), Keep: 2, Stat: fakeDisk(&used), Clock: clk, Key: key}
	p, err := b.Run(context.Background())
	if err != nil || filepath.Ext(p) != ".enc" {
		t.Fatalf("encrypted backup: %s %v", p, err)
	}
	raw, _ := os.ReadFile(p)
	if bytes.Contains(raw, []byte("SQLite format 3")) || bytes.Contains(raw, []byte("@")) && bytes.Contains(raw, []byte(".com")) {
		t.Fatal("plaintext visible in encrypted backup")
	}
	if err := Restore(p, filepath.Join(t.TempDir(), "x.db"), nil); err == nil {
		t.Fatal("restore without key must fail")
	}
	wrong := append([]byte(nil), key...)
	wrong[0] ^= 1
	if err := Restore(p, filepath.Join(t.TempDir(), "y.db"), wrong); err == nil {
		t.Fatal("wrong key accepted")
	}
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)/2] ^= 0x40
	tp := filepath.Join(t.TempDir(), "t.enc")
	os.WriteFile(tp, tampered, 0o600)
	if err := Restore(tp, filepath.Join(t.TempDir(), "t.db"), key); err == nil {
		t.Fatal("tampered backup accepted")
	}
	trunc := filepath.Join(t.TempDir(), "tr.enc")
	os.WriteFile(trunc, raw[:len(raw)-20], 0o600)
	if err := Restore(trunc, filepath.Join(t.TempDir(), "tr.db"), key); err == nil {
		t.Fatal("truncated backup accepted")
	}
	out := filepath.Join(t.TempDir(), "ok.db")
	if err := Restore(p, out, key); err != nil {
		t.Fatal(err)
	}
	r, err := db.Open(context.Background(), out, db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var n int
	r.R.QueryRow(`SELECT count(*) FROM contacts`).Scan(&n)
	if n == 0 {
		t.Fatal("restored encrypted backup is empty")
	}
	if _, err := ParseKey("too-short"); err == nil {
		t.Fatal("bad key accepted")
	}
}
