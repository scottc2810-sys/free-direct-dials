// Command dials is the Free Direct Dials server and operator CLI.
//
//	dials serve                 run the web server + background workers
//	dials migrate               apply database migrations and exit
//	dials admin create <user>   create/reset a superadmin (prints the TOTP enrolment QR)
//	dials import <file> [-sheet N]   import a CSV / TSV / .csv.gz / .xlsx file from disk
//	dials gen <rows> <out.csv>  write a synthetic test file
//	dials backup                take a backup now
//	dials restore <backup.db.zst> <dest.db>
//	dials inspect <file> [-sheet N]  profile a CSV/xlsx before importing (PII-masked)
//	dials seo                   recompute SEO landing pages
//	dials blocklist <file>      add disposable domains (one per line)
//	dials check                 validate configuration and print a summary
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"rsc.io/qr"

	"markemind/dials/internal/auth"
	"markemind/dials/internal/config"
	"markemind/dials/internal/db"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/httpapi"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/jobs"
	"markemind/dials/internal/mail"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/search"
	"markemind/dials/internal/seo"
	"markemind/dials/internal/settings"
	"markemind/dials/internal/unlock"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	envFile := os.Getenv("DIALS_ENV_FILE")
	if envFile == "" {
		envFile = "dials.env"
	}
	if err := config.LoadDotenv(envFile); err != nil {
		fatal("reading %s: %v", envFile, err)
	}
	cfg, err := config.Load(nil)
	if err != nil {
		fatal("configuration:\n%v", err)
	}
	log := platform.NewLogger(cfg.LogLevel, cfg.Prod())
	slog.SetDefault(log)
	if cfg.Prod() && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(150 << 20)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		err = serve(ctx, cfg, log)
	case "migrate":
		var d *db.DB
		if d, err = openDB(ctx, cfg); err == nil {
			fmt.Println("schema version", d.SchemaVersion(ctx))
			d.Close()
		}
	case "admin":
		err = adminCmd(ctx, cfg, args)
	case "import":
		err = importCmd(ctx, cfg, args)
	case "gen":
		err = genCmd(args)
	case "backup":
		err = backupCmd(ctx, cfg)
	case "restore":
		if len(args) != 2 {
			usage()
		}
		var key []byte
		if key, err = jobs.ParseKey(cfg.BackupKey); err == nil {
			err = jobs.Restore(args[0], args[1], key)
		}
	case "seo":
		err = seoCmd(ctx, cfg)
	case "inspect":
		err = inspectCmd(cfg, args)
	case "bench":
		err = benchCmd(ctx, cfg, args)
	case "blocklist":
		err = blocklistCmd(ctx, cfg, args)
	case "check":
		err = checkCmd(ctx, cfg)
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		fatal("%v", err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dials serve|migrate|admin create <user>|import <file> [-sheet N]|gen <rows> <out>|backup|restore <src> <dst>|seo|blocklist <file>|check|version")
	os.Exit(2)
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "dials: "+f+"\n", a...)
	os.Exit(1)
}

func openDB(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	for _, d := range []string{cfg.DataDir, cfg.BackupDir, cfg.TmpDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	return db.Open(ctx, cfg.DBPath(), db.Options{})
}

func buildMailer(cfg *config.Config, d *db.DB, clk platform.Clock, log *slog.Logger, st *settings.Store) *mail.Mailer {
	var routes []mail.Route
	for _, p := range cfg.MailProviders {
		var prov mail.Provider
		switch p.Name {
		case "brevo":
			prov = &mail.Brevo{Key: cfg.BrevoKey}
		case "resend":
			prov = &mail.Resend{Key: cfg.ResendKey}
		case "smtp":
			prov = &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass}
		default:
			prov = &mail.LogProvider{Log: log}
		}
		routes = append(routes, mail.Route{P: prov, Cap: p.Cap})
	}
	return &mail.Mailer{DB: d, Clock: clk, Log: log, From: mail.Address{Name: cfg.MailFromName, Email: cfg.MailFrom}, Routes: routes,
		ReservePct: cfg.MailReservePct, Footer: func() mail.Footer {
			s := st.Get().Site
			return mail.Footer{LegalName: s.LegalName, Address: s.Address, SiteURL: cfg.URL("/"), DonateURL: cfg.URL("/#donate")}
		}}
}

// devOTPLogger shows sign-in codes in the console when mail goes to the log provider.
type devOTPLogger struct {
	inner auth.Sender
	log   *slog.Logger
	on    bool
}

func (d devOTPLogger) SendOTP(ctx context.Context, to, code, purpose string) error {
	if d.on {
		d.log.Warn("DEV sign-in code", "to", to, "code", code, "purpose", purpose)
	}
	return d.inner.SendOTP(ctx, to, code, purpose)
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	clk := platform.SystemClock{}
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	st := &settings.Store{DB: d, Clock: clk}
	if err := st.Load(ctx); err != nil {
		return err
	}
	disp := emailcheck.NewDisposable()
	if err := disp.Reload(ctx, d.R); err != nil {
		return err
	}
	mailer := buildMailer(cfg, d, clk, log, st)
	alerter := &jobs.Alerter{Clock: clk, Log: log, Send: func(ctx context.Context, subject, body string) error {
		return mailer.Alert(ctx, cfg.AdminEmail, subject, body)
	}}
	guard := &jobs.Guard{Path: cfg.DataDir, Stat: platform.Disk, Manual: func() bool { return st.Get().Site.ReadOnly }}
	guard.OnChange = func(old, nw int, pct float64) {
		if nw > old && nw >= jobs.LevelWarn {
			alerter.Alert(context.Background(), fmt.Sprintf("disk%d", nw), fmt.Sprintf("Disk %.0f%% full", pct),
				"Level "+fmt.Sprint(nw)+": at 90% uploads/backups stop, at 95% the site goes read-only. Free space: see RUNBOOK.md → Disk.")
		}
	}
	guard.Check()
	backupKey, err := jobs.ParseKey(cfg.BackupKey)
	if err != nil {
		return err
	}
	if backupKey == nil && cfg.Prod() {
		log.Warn("BACKUP_KEY is not set: backups are compressed but NOT encrypted; set it before copying backups off the server")
	}
	searchSvc := search.New(d, search.NewLRU(30<<20, nil), 4)
	host := ""
	if u, err := url.Parse(cfg.PublicOrigin); err == nil {
		host = u.Hostname()
	}
	hasLog := false
	for _, p := range cfg.MailProviders {
		hasLog = hasLog || p.Name == "log"
	}
	app := &httpapi.App{
		Cfg: cfg, DB: d, Log: log, Clock: clk, Search: searchSvc, Settings: st,
		Unlock:     &unlock.Service{DB: d, Search: searchSvc, Settings: st, Clock: clk},
		Users:      &auth.Users{DB: d, Clock: clk},
		Sessions:   &auth.Sessions{DB: d, Clock: clk, TTL: 30 * 24 * time.Hour},
		OTP:        &auth.OTP{DB: d, Clock: clk, Secret: cfg.SecretKey, Sender: devOTPLogger{mailer, log, hasLog && !cfg.Prod()}},
		Admins:     &auth.Admins{DB: d, Clock: clk, Idle: 30 * time.Minute, Absolute: 12 * time.Hour},
		Turnstile:  auth.NewTurnstile(cfg.TurnstileSecret, host, log),
		Disposable: disp,
		Checker: &emailcheck.Checker{DB: d, Disposable: disp, Resolver: net.DefaultResolver, Reacher: emailcheck.NewReacher(cfg.ReacherURL, cfg.ReacherToken),
			Clock: clk, Log: log, RequireBusiness: func() bool { return st.Get().Limits.RequireBusinessEmail },
			UseReacher: func() bool { return st.Get().Limits.UseReacher }},
		Mailer:   mailer,
		Outbox:   &mail.Outbox{DB: d, Mailer: mailer, Clock: clk, Log: log, URL: cfg.URL},
		Importer: &importer.Importer{DB: d, Disposable: disp},
		Guard:    guard,
		Alerter:  alerter,
		Backup:   &jobs.Backup{DBPath: d.Path, Dir: cfg.BackupDir, Keep: 2, Guard: guard, Stat: platform.Disk, Clock: clk, Key: backupKey},
	}
	sched := &jobs.Scheduler{DB: d, Clock: clk, Log: log, OnError: func(job string, err error) {
		alerter.Alert(context.Background(), "job_"+job, "Background job failed: "+job, err.Error())
	}}
	app.Sched = sched
	if err := app.Init(); err != nil {
		return err
	}
	sched.Add(&jobs.Job{Name: "outbox", Every: 20 * time.Second, Fn: func(ctx context.Context) error {
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			n, err := app.Outbox.Tick(ctx, 50)
			if err != nil || n == 0 {
				return err
			}
		}
		return nil
	}})
	sched.Add(&jobs.Job{Name: "disk", Every: 10 * time.Minute, Fn: func(ctx context.Context) error { _, _, err := guard.Check(); return err }})
	sched.Add(&jobs.Job{Name: "hourly", Every: time.Hour, Fn: func(ctx context.Context) error { return jobs.Hourly(ctx, d, clk, cfg.TmpDir) }})
	sched.Add(&jobs.Job{Name: "daily", Daily: true, Hour: 3, Fn: func(ctx context.Context) error { return jobs.Daily(ctx, d, clk) }})
	sched.Add(&jobs.Job{Name: "backup", Daily: true, Hour: 3, Minute: 30, Fn: func(ctx context.Context) error {
		_, err := app.Backup.Run(ctx)
		return err
	}})
	sched.Add(&jobs.Job{Name: "seo", Daily: true, Hour: 4, Fn: func(ctx context.Context) error {
		if _, err := seo.Refresh(ctx, d, clk, 500, 25); err != nil {
			return err
		}
		app.Search.Cache.Purge()
		return nil
	}})
	sched.Add(&jobs.Job{Name: "monthly", Daily: true, Hour: 4, Minute: 30, Fn: func(ctx context.Context) error {
		if clk.Now().UTC().Day() != 1 {
			return nil
		}
		return jobs.Monthly(ctx, d, clk, platform.Disk, log)
	}})
	bg, cancelBG := context.WithCancel(context.Background())
	schedDone := make(chan struct{})
	go func() { sched.Run(bg); close(schedDone) }()
	if n, _ := countSEO(ctx, d); n == 0 {
		sched.RunNow(bg, "seo")
	}

	srv := &http.Server{
		Addr: cfg.ListenAddr, Handler: app.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "dials %s listening on http://%s%s/ (admin: %s/%s/)\n", version, cfg.ListenAddr, cfg.BasePath, cfg.BasePath, maskPath(cfg.AdminPath, cfg.Prod()))
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		cancelBG()
		return err
	case <-ctx.Done():
	}
	log.Warn("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	cancelBG()
	select {
	case <-schedDone:
	case <-sctx.Done():
	}
	return nil
}

func maskPath(p string, prod bool) string {
	if prod && len(p) > 4 {
		return p[:4] + strings.Repeat("*", len(p)-4)
	}
	return p
}

func countSEO(ctx context.Context, d *db.DB) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM seo_pages`).Scan(&n)
	return n, err
}

func adminCmd(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) != 2 || args[0] != "create" {
		return errors.New("usage: dials admin create <username>")
	}
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	fmt.Fprint(os.Stderr, "New password (min 12 chars): ")
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && pw == "" {
		return err
	}
	pw = strings.TrimRight(pw, "\r\n")
	a := &auth.Admins{DB: d, Clock: platform.SystemClock{}}
	secret, err := a.Create(ctx, args[1], pw)
	if err != nil {
		return err
	}
	u := auth.OTPAuthURL("Free Direct Dials", strings.ToLower(args[1]), secret)
	fmt.Printf("\nAdmin %q saved. Scan this with Google Authenticator / 1Password / Authy:\n\n", strings.ToLower(args[1]))
	printQR(u)
	fmt.Printf("\nOr enter the key manually: %s\n\nSign in at %s/%s/\n", secret, cfg.URL(""), cfg.AdminPath)
	return nil
}

func printQR(text string) {
	c, err := qr.Encode(text, qr.L)
	if err != nil {
		fmt.Println(text)
		return
	}
	for y := -2; y < c.Size+2; y += 2 {
		var b strings.Builder
		for x := -2; x < c.Size+2; x++ {
			top, bot := c.Black(x, y), c.Black(x, y+1)
			switch {
			case top && bot:
				b.WriteString(" ")
			case top:
				b.WriteString("\xe2\x96\x84") // ▄
			case bot:
				b.WriteString("\xe2\x96\x80") // ▀
			default:
				b.WriteString("\xe2\x96\x88") // █
			}
		}
		fmt.Println(b.String())
	}
}

func importCmd(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	sheet := fs.Int("sheet", 1, "worksheet number for .xlsx files (1 = first)")
	if len(args) == 0 {
		return errors.New("usage: dials import <file> [-sheet N]")
	}
	file := args[0]
	fs.Parse(args[1:])
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	disp := emailcheck.NewDisposable()
	disp.Reload(ctx, d.R)
	src, err := importer.OpenSource(file, cfg.TmpDir, *sheet-1)
	if err != nil {
		return err
	}
	defer src.Close()
	start := time.Now()
	st, err := (&importer.Importer{DB: d, Disposable: disp}).Run(ctx, src, importer.Options{Progress: func(s importer.Stats) {
		fmt.Fprintf(os.Stderr, "\r%d rows, %d added, %d updated, %d skipped", s.Rows, s.Added, s.Updated, s.Skipped)
	}})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fi, _ := os.Stat(file)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	d.W.ExecContext(ctx, `INSERT INTO uploads(ts, filename, bytes, rows, added, updated, unchanged, skipped, status, by, finished)
		VALUES(?,?,?,?,?,?,?,?,'done','cli',?)`, start.Unix(), filepath.Base(file), size, st.Rows, st.Added, st.Updated, st.Unchanged, st.Skipped, time.Now().Unix())
	fmt.Printf("done in %s: rows=%d added=%d updated=%d unchanged=%d skipped=%d %v cleaned=%v\n", time.Since(start).Round(time.Millisecond),
		st.Rows, st.Added, st.Updated, st.Unchanged, st.Skipped, st.Reasons, st.Cleaned)
	if _, err := seo.Refresh(ctx, d, platform.SystemClock{}, 500, 25); err != nil {
		return err
	}
	fmt.Println("Restart is not needed; the running server picks up changes (caches expire within a minute). Use Superadmin → Imports → Notify to email subscribers.")
	return nil
}

func inspectCmd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	sheet := fs.Int("sheet", 1, "worksheet number for .xlsx files (1 = first)")
	if len(args) == 0 {
		return errors.New("usage: dials inspect <file> [-sheet N]")
	}
	fs.Parse(args[1:])
	os.MkdirAll(cfg.TmpDir, 0o750)
	src, err := importer.OpenSource(args[0], cfg.TmpDir, *sheet-1)
	if err != nil {
		return err
	}
	defer src.Close()
	rep, err := importer.Inspect(src)
	if err != nil {
		return err
	}
	if src.Kind == "xlsx" {
		names := []string{}
		for _, s := range src.Sheets {
			names = append(names, s.Name)
		}
		fmt.Println("workbook sheets:", strings.Join(names, ", "), "(reading #"+fmt.Sprint(*sheet)+")")
	} else {
		fmt.Printf("csv: %+v\n", rep.Sniffed)
	}
	fmt.Printf("rows: %d, columns: %d\n\n", rep.Rows, len(rep.Columns))
	fmt.Printf("%-34s %-14s %7s %8s %6s  %s\n", "COLUMN", "MAPS TO", "FILLED", "DISTINCT", "MAXLEN", "SAMPLES (masked)")
	for _, c := range rep.Columns {
		field := c.Field
		if field == "" {
			field = "(extra)"
		}
		pct := 0.0
		if rep.Rows > 0 {
			pct = 100 * float64(c.Filled) / float64(rep.Rows)
		}
		d := fmt.Sprint(c.Distinct)
		if c.Distinct >= 5000 {
			d = "5000+"
		}
		fmt.Printf("%-34.34s %-14s %6.1f%% %8s %6d  %s\n", c.Header, field, pct, d, c.MaxLen, strings.Join(c.Samples, " | "))
	}
	return nil
}

func genCmd(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: dials gen <rows> <out.csv>")
	}
	var n int
	if _, err := fmt.Sscan(args[0], &n); err != nil || n <= 0 {
		return errors.New("rows must be a positive number")
	}
	f, err := os.Create(args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	if err := importer.Generate(w, n, uint64(time.Now().UnixNano())); err != nil {
		return err
	}
	return w.Flush()
}

func backupCmd(ctx context.Context, cfg *config.Config) error {
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	d.Close()
	key, err := jobs.ParseKey(cfg.BackupKey)
	if err != nil {
		return err
	}
	b := &jobs.Backup{DBPath: cfg.DBPath(), Dir: cfg.BackupDir, Keep: 2, Stat: platform.Disk, Clock: platform.SystemClock{}, Key: key}
	p, err := b.Run(ctx)
	if err == nil {
		fmt.Println("backup written:", p)
	}
	return err
}

func seoCmd(ctx context.Context, cfg *config.Config) error {
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	n, err := seo.Refresh(ctx, d, platform.SystemClock{}, 500, 25)
	if err == nil {
		fmt.Println("SEO pages:", n)
	}
	return err
}

func blocklistCmd(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: dials blocklist <file>")
	}
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	added := 0
	for sc.Scan() {
		if dom, ok := emailcheck.NormalizeDomain(sc.Text()); ok {
			if res, err := d.W.ExecContext(ctx, `INSERT OR IGNORE INTO blocklist(domain, source) VALUES(?, 'import')`, dom); err == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					added++
				}
			}
		}
	}
	fmt.Println("added", added, "domains (restart or save any blocklist change in Superadmin to reload)")
	return sc.Err()
}

func checkCmd(ctx context.Context, cfg *config.Config) error {
	d, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	st := importer.LoadStats(ctx, d.R)
	ds, _ := platform.Disk(cfg.DataDir)
	fmt.Printf("env=%s listen=%s base=%q origin=%s\nschema=%d contacts=%d disk=%.0f%% used\nmail=%v turnstile=%v reacher=%v\n",
		cfg.Env, cfg.ListenAddr, cfg.BasePath, cfg.PublicOrigin, d.SchemaVersion(ctx), st.Contacts, ds.UsedPct(),
		cfg.MailProviders, cfg.TurnstileSecret != "", cfg.ReacherURL != "")
	a := &auth.Admins{DB: d}
	if a.Count(ctx) == 0 {
		fmt.Println("WARNING: no superadmin yet. Run: dials admin create <username>")
	}
	return nil
}
