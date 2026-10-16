package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"markemind/dials/internal/auth"
	"markemind/dials/internal/db"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/mail"
	"markemind/dials/internal/payments"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/settings"
	"markemind/dials/internal/store"
)

const adminCookie = "fdd_a"

func (a *App) adminPrefix() string { return a.Cfg.BasePath + "/" + a.Cfg.AdminPath }

type adminHandler func(w http.ResponseWriter, r *http.Request, s auth.AdminSession)

func (a *App) adminRoutes(m *http.ServeMux) {
	p := a.adminPrefix()
	open := func(pattern string, fn http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		m.HandleFunc(method+" "+p+path, a.adminGate(fn))
	}
	authed := func(pattern string, fn adminHandler) {
		open(pattern, a.adminAuth(fn))
	}
	m.HandleFunc("GET "+p, a.adminGate(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, p+"/", http.StatusFound) }))
	open("GET /{$}", a.adminPage)
	open("POST /api/login", a.adminLogin)
	authed("POST /api/logout", a.adminLogout)
	authed("GET /api/session", a.adminSession)
	authed("GET /api/stats", a.adminStats)
	authed("GET /api/fields", a.adminFields)

	authed("POST /api/uploads", a.adminUploadInit)
	authed("PUT /api/uploads/{id}/chunks/{n}", a.adminUploadChunk)
	authed("GET /api/uploads/{id}", a.adminUploadStatus)
	authed("DELETE /api/uploads/{id}", a.adminUploadDelete)
	authed("POST /api/uploads/{id}/preview", a.adminUploadPreview)
	authed("POST /api/uploads/{id}/dryrun", a.adminUploadDryRun)
	authed("POST /api/uploads/{id}/import", a.adminUploadImport)
	authed("GET /api/imports", a.adminImports)
	authed("GET /api/imports/{id}", a.adminImport)
	authed("POST /api/imports/{id}/cancel", a.adminImportCancel)
	authed("POST /api/imports/{id}/notify", a.adminNotify)

	authed("GET /api/outbox", a.adminOutbox)
	authed("POST /api/outbox/pause", a.adminOutboxPause)
	authed("POST /api/test-email", a.adminTestEmail)

	authed("GET /api/settings", a.adminSettings)
	authed("POST /api/settings/{section}", a.adminSettingsSave)

	authed("GET /api/blocklist", a.adminBlocklist)
	authed("POST /api/blocklist", a.adminBlocklistAdd)
	authed("DELETE /api/blocklist/{domain}", a.adminBlocklistDelete)

	authed("GET /api/removals", a.adminRemovals)
	authed("POST /api/removals/{id}", a.adminRemovalProcess)
	authed("POST /api/contacts/remove", a.adminContactRemove)

	authed("GET /api/users", a.adminUsers)
	authed("POST /api/users/{id}/block", a.adminUserBlock)
	authed("DELETE /api/users/{id}", a.adminUserDelete)

	authed("GET /api/stores", a.adminStores)
	authed("POST /api/stores/test", a.adminStoreTest)
	authed("POST /api/stores", a.adminStoreAdd)
	authed("POST /api/stores/{id}", a.adminStoreUpdate)
	authed("DELETE /api/stores/{id}", a.adminStoreDetach)
	authed("POST /api/stores/check", a.adminStoreCheck)

	authed("GET /api/audit", a.adminAudit)
	authed("GET /api/backups", a.adminBackups)
	authed("POST /api/jobs/{name}/run", a.adminJobRun)
	authed("POST /api/cache/purge", a.adminCachePurge)
}

// adminAllowed reports whether the client IP is on the superadmin allow-list.
// The list is never empty (config requires it in prod; dev = localhost only).
func (a *App) adminAllowed(r *http.Request) bool {
	ip, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range a.Cfg.AdminCIDRs {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// adminGate: IP allow-list (plain 404 to everyone else, so the panel's
// existence isn't revealed), strict CSP, never cached.
func (a *App) adminGate(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.adminAllowed(r) {
			if a.limits.adminDenied.Allow(clientIP(r)) { // log, but don't let a scanner flood the log
				a.Log.Warn("superadmin access denied: IP not on allow-list", "ip", clientIP(r), "path", r.URL.Path)
			}
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; "+
			"frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		noStore(w)
		fn(w, r)
	}
}

func (a *App) adminAuth(fn adminHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(adminCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Please sign in.")
			return
		}
		s, err := a.Admins.Session(r.Context(), c.Value)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Your admin session expired. Please sign in again.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF")), []byte(s.CSRF)) != 1 {
				writeErr(w, http.StatusForbidden, "csrf", "Security token mismatch. Reload the page.")
				return
			}
		}
		fn(w, r, s)
	}
}

func (a *App) audit(ctx context.Context, actor, ip, action, detail string) {
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	a.DB.W.ExecContext(context.WithoutCancel(ctx), `INSERT INTO audit(ts, actor, ip, action, detail) VALUES(?,?,?,?,?)`,
		a.Clock.Now().Unix(), actor, ip, action, detail)
}

func (a *App) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.Tpl.Render(w, "admin", map[string]any{"Prefix": a.adminPrefix(), "Base": a.Cfg.BasePath}); err != nil {
		a.Log.Error("admin template", "err", err)
	}
}

type loginIn struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (a *App) adminLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limited(w, a.limits.adminLogin, ip, "Too many sign-in attempts. Wait 15 minutes.") {
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != a.Cfg.PublicOrigin && !(!a.Cfg.Prod() && isLocalOrigin(o)) {
		writeErr(w, http.StatusForbidden, "forbidden", "Request blocked.")
		return
	}
	var in loginIn
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	tok, s, err := a.Admins.Login(r.Context(), in.Username, in.Password, in.Code, ip)
	if err != nil {
		a.audit(r.Context(), strings.ToLower(in.Username), ip, "login_failed", err.Error())
		if errors.Is(err, auth.ErrAdminLocked) {
			writeErr(w, http.StatusTooManyRequests, "locked", "This account is temporarily locked after failed attempts.")
			return
		}
		writeErr(w, http.StatusUnauthorized, "bad_login", "Wrong username, password or authenticator code.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: tok, Path: a.adminPrefix() + "/", HttpOnly: true,
		Secure: strings.HasPrefix(a.Cfg.PublicOrigin, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: int(a.Admins.Absolute.Seconds())})
	a.audit(r.Context(), s.Username, ip, "login", "")
	writeJSON(w, http.StatusOK, map[string]string{"username": s.Username, "csrf": s.CSRF})
}

func (a *App) adminLogout(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	if c, err := r.Cookie(adminCookie); err == nil {
		a.Admins.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: "", Path: a.adminPrefix() + "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) adminSession(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	writeJSON(w, http.StatusOK, map[string]string{"username": s.Username, "csrf": s.CSRF})
}

func (a *App) adminFields(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	writeJSON(w, http.StatusOK, map[string]any{"fields": importer.FieldOptions, "networks": payments.Networks,
		"slots": settings.SlotNames, "placements": settings.PlacementNames, "aff_placements": settings.AffiliatePlacements})
}

func scalarQ(ctx context.Context, q db.Querier, query string, args ...any) int64 {
	var n int64
	q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n
}

func (a *App) adminStats(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	ctx := r.Context()
	now := a.Clock.Now()
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC).Unix()
	today := platform.Day(now)
	lv, pct, _ := a.Guard.Check()
	ds, _ := platform.Disk(a.Cfg.DataDir)
	var dbSize, walSize int64
	if fi, err := os.Stat(a.DB.Path); err == nil {
		dbSize = fi.Size()
	}
	if fi, err := os.Stat(a.DB.Path + "-wal"); err == nil {
		walSize = fi.Size()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	cfg := a.Settings.Get()
	var warns []string
	if !a.Turnstile.Enabled() {
		warns = append(warns, "Turnstile is off: sign-ups are not bot-protected (allowed in dev only).")
	}
	for _, p := range a.Cfg.MailProviders {
		if p.Name == "log" {
			warns = append(warns, "Email provider is 'log': codes are printed to the server log, not emailed.")
		}
	}
	if a.Cfg.AdminEmail == "" {
		warns = append(warns, "ADMIN_EMAIL is not set: you won't receive disk, backup or failure alerts.")
	}
	if strings.TrimSpace(cfg.Site.Address) == "" {
		warns = append(warns, "Add your postal address (Settings → Site). Marketing emails legally require it.")
	}
	if cfg.Site.ContactEmail == "" {
		warns = append(warns, "Add a contact email for privacy requests (Settings → Site).")
	}
	if !cfg.WhatsApp.Enabled {
		warns = append(warns, "WhatsApp bulk-data button is off: add your number in Settings → WhatsApp.")
	}
	if !cfg.Donations.UPI.Enabled && !cfg.Donations.PayPal.Enabled {
		warns = append(warns, "UPI and PayPal donations are not set up yet (Settings → Donations).")
	}
	slots := 0
	for _, sl := range cfg.Ads.Slots {
		if sl.Enabled {
			slots++
		}
	}
	if slots == 0 {
		warns = append(warns, "No Adsterra banner codes yet: slots show the Smart Link fallback banner (Settings → Ads).")
	}
	if lv >= 1 {
		warns = append(warns, fmt.Sprintf("Disk is %.0f%% full.", pct))
	}
	if n := scalarQ(ctx, a.DB.R, `SELECT COUNT(*) FROM removals WHERE status='pending'`); n > 0 {
		warns = append(warns, fmt.Sprintf("%d removal request(s) are waiting (30-day legal deadline).", n))
	}
	stores := a.Store.Statuses(ctx)
	for _, st := range stores {
		switch {
		case st.State == "down":
			warns = append(warns, fmt.Sprintf("Database %q is unreachable: its contacts are missing from results and uploads are paused. %s", st.Name, st.LastErr))
		case st.Pct >= 80:
			warns = append(warns, fmt.Sprintf("Database %q is %d%% of its quota: add another database (Databases) before it fills.", st.Name, st.Pct))
		}
	}
	if cfg.WhatsApp.Number == "" || !cfg.WhatsApp.Enabled {
		warns = append(warns, "WhatsApp contact is off: bulk-data leads have nowhere to go (Settings → WhatsApp).")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"headline": a.Store.Stats(ctx),
		"stores":   stores,
		"users": map[string]int64{
			"total":      scalarQ(ctx, a.DB.R, `SELECT COUNT(*) FROM users`),
			"subscribed": scalarQ(ctx, a.DB.R, `SELECT COUNT(*) FROM users WHERE subscribed=1 AND bounced=0 AND blocked=0`),
			"today":      scalarQ(ctx, a.DB.R, `SELECT COUNT(*) FROM users WHERE created>=?`, dayStart),
			"week":       scalarQ(ctx, a.DB.R, `SELECT COUNT(*) FROM users WHERE created>=?`, dayStart-6*86400),
			"active_7d":  scalarQ(ctx, a.DB.R, `SELECT COUNT(*) FROM users WHERE last_seen>=?`, dayStart-6*86400),
		},
		"unlocks": map[string]int64{
			"ads_today":    scalarQ(ctx, a.DB.R, `SELECT COALESCE(SUM(ads),0) FROM user_daily WHERE day=?`, today),
			"users_today":  scalarQ(ctx, a.DB.R, `SELECT COUNT(*) FROM user_daily WHERE day=?`, today),
			"breaks_total": scalarQ(ctx, a.DB.R, `SELECT COALESCE(SUM(ads),0) FROM user_daily`),
		},
		"outbox":  a.Outbox.Status(ctx),
		"disk":    map[string]any{"pct": pct, "level": lv, "free": ds.Free, "total": ds.Total},
		"db":      map[string]any{"bytes": dbSize, "wal_bytes": walSize, "dataset_version": a.Store.Version(), "schema": a.DB.SchemaVersion(ctx)},
		"backups": a.Backup.List(),
		"jobs":    a.Sched.Status(ctx),
		"cache":   a.Store.Cache.Stats(),
		"runtime": map[string]any{"goroutines": runtime.NumGoroutine(), "heap_mb": ms.HeapAlloc >> 20, "sys_mb": ms.Sys >> 20,
			"uptime_s": int64(now.Sub(a.Started).Seconds())},
		"importing": a.Imports.Running(),
		"read_only": a.Guard.ManualOn(),
		"warnings":  warns,
		"blocklist": func() map[string]int { b, ad := a.Disposable.Size(); return map[string]int{"builtin": b, "admin": ad} }(),
	})
}

// ---------- uploads & imports ----------

type uploadInitIn struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

func (a *App) adminUploadInit(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in uploadInitIn
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	if err := a.Guard.AllowHeavy(); err != nil {
		writeErr(w, http.StatusInsufficientStorage, "disk_full", err.Error())
		return
	}
	if ds, err := platform.Disk(a.Cfg.DataDir); err == nil && ds.Free < uint64(in.Size)*3+(1<<30) {
		writeErr(w, http.StatusInsufficientStorage, "disk_full", fmt.Sprintf("Not enough free disk for a %d MB file (need about 3× its size plus 1 GB).", in.Size>>20))
		return
	}
	m, err := a.Uploads.Init(in.Filename, in.Size)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_upload", err.Error())
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "upload.init", fmt.Sprintf("%s (%d bytes)", m.Filename, m.Size))
	writeJSON(w, http.StatusOK, m)
}

func (a *App) adminUploadChunk(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_chunk", "Bad chunk number.")
		return
	}
	body := http.MaxBytesReader(w, r.Body, ChunkSize+1)
	if err := a.Uploads.WriteChunk(r.PathValue("id"), n, body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_chunk", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) adminUploadStatus(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	m, err := a.Uploads.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upload": m, "missing": m.Missing()})
}

func (a *App) adminUploadDelete(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	a.Uploads.Remove(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type sheetIn struct {
	Sheet    int              `json:"sheet"`
	Mapping  importer.Mapping `json:"mapping"`
	Attest   bool             `json:"attest"`
	Remember bool             `json:"remember"`
}

func (a *App) openUpload(w http.ResponseWriter, id string, sheet int) (*importer.Source, bool) {
	m, err := a.Uploads.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return nil, false
	}
	if miss := m.Missing(); len(miss) > 0 {
		writeErrExtra(w, http.StatusConflict, "incomplete", fmt.Sprintf("%d chunk(s) still missing.", len(miss)), map[string]any{"missing": miss})
		return nil, false
	}
	p, _ := a.Uploads.DataPath(id)
	src, err := importer.OpenSource(p, a.Cfg.TmpDir, sheet)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "bad_file", err.Error())
		return nil, false
	}
	return src, true
}

var sigRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

func (a *App) adminUploadPreview(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in sheetIn
	if !decodeJSON(w, r, &in, 64<<10) {
		return
	}
	src, ok := a.openUpload(w, r.PathValue("id"), in.Sheet)
	if !ok {
		return
	}
	defer src.Close()
	pv, err := importer.Preview(src, 20)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "bad_file", "Couldn't read this file: "+err.Error())
		return
	}
	sig := importer.Signature(pv.Headers)
	remembered := false
	if raw := db.GetMeta(r.Context(), a.DB.R, "mapping:"+sig); raw != "" {
		var m importer.Mapping
		if json.Unmarshal([]byte(raw), &m) == nil && len(m) == len(pv.Headers) && m.Validate() == nil {
			pv.Mapping, remembered = m, true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": src.Kind, "sheets": src.Sheets, "sheet": in.Sheet, "preview": pv,
		"signature": sig, "remembered": remembered, "warnings": pv.Mapping.Warnings()})
}

func (a *App) adminUploadDryRun(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in sheetIn
	if !decodeJSON(w, r, &in, 256<<10) {
		return
	}
	src, ok := a.openUpload(w, r.PathValue("id"), in.Sheet)
	if !ok {
		return
	}
	defer src.Close()
	res, err := importer.DryRun(src, in.Mapping, 200, a.Disposable)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "bad_mapping", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type importIn struct {
	sheetIn
	Signature string `json:"signature"`
	Target    string `json:"target"` // "auto" or a database id
}

func (a *App) adminUploadImport(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in importIn
	if !decodeJSON(w, r, &in, 256<<10) {
		return
	}
	if !in.Attest {
		writeErr(w, http.StatusBadRequest, "attest", "Confirm that you have the right to publish this data.")
		return
	}
	if in.Mapping != nil {
		if err := in.Mapping.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_mapping", err.Error())
			return
		}
		if in.Remember && sigRe.MatchString(in.Signature) {
			b, _ := json.Marshal(in.Mapping)
			db.SetMeta(r.Context(), a.DB.W, "mapping:"+in.Signature, string(b))
		}
	}
	target := store.Auto
	if in.Target != "" && in.Target != "auto" {
		n, err := strconv.Atoi(in.Target)
		if err != nil || a.Store.Member(n) == nil {
			writeErr(w, http.StatusBadRequest, "bad_target", "Unknown database.")
			return
		}
		target = n
	}
	id, err := a.Imports.Start(r.PathValue("id"), in.Sheet, in.Mapping, s.Username, target)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrImportRunning) {
			code = http.StatusConflict
		}
		writeErr(w, code, "import_failed", err.Error())
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "import.start", fmt.Sprintf("upload #%d (rights attested)", id))
	writeJSON(w, http.StatusOK, map[string]int64{"import_id": id})
}

type uploadRow struct {
	ID        int64           `json:"id"`
	TS        int64           `json:"ts"`
	Filename  string          `json:"filename"`
	Bytes     int64           `json:"bytes"`
	Rows      int64           `json:"rows"`
	Added     int64           `json:"added"`
	Updated   int64           `json:"updated"`
	Unchanged int64           `json:"unchanged"`
	Skipped   int64           `json:"skipped"`
	Reasons   json.RawMessage `json:"reasons"`
	Status    string          `json:"status"`
	Error     string          `json:"error"`
	Notified  int64           `json:"notified"`
	By        string          `json:"by"`
	Finished  int64           `json:"finished"`
	Target    string          `json:"target"`
	Live      *importer.Stats `json:"live,omitempty"`
}

func (a *App) loadUploads(ctx context.Context, where string, args ...any) ([]uploadRow, error) {
	rows, err := a.DB.R.QueryContext(ctx, `SELECT id, ts, filename, bytes, rows, added, updated, unchanged, skipped, skip_reasons,
		status, error, notified, by, finished, target FROM uploads `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uploadRow{}
	for rows.Next() {
		var u uploadRow
		var reasons string
		if err := rows.Scan(&u.ID, &u.TS, &u.Filename, &u.Bytes, &u.Rows, &u.Added, &u.Updated, &u.Unchanged, &u.Skipped, &reasons,
			&u.Status, &u.Error, &u.Notified, &u.By, &u.Finished, &u.Target); err != nil {
			return nil, err
		}
		u.Reasons = json.RawMessage(orDefault(reasons, "{}"))
		if u.Status == "running" {
			if st, ok := a.Imports.Progress(u.ID); ok && st != nil {
				u.Live = st
			}
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (a *App) adminImports(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	pg := pageOf(r, 25, scalarQ(r.Context(), a.DB.R, `SELECT COUNT(*) FROM uploads`))
	rows, err := a.loadUploads(r.Context(), `ORDER BY id DESC LIMIT ? OFFSET ?`, pg.Per, pg.offset())
	if err != nil {
		a.queryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imports": rows, "running": a.Imports.Running(), "paging": pg})
}

// paging is a page of an admin list (offset paging: admin lists are small).
type paging struct {
	Page  int   `json:"page"` // 0-based
	Pages int   `json:"pages"`
	Per   int   `json:"per"`
	Total int64 `json:"total"`
}

func (p paging) offset() int { return p.Page * p.Per }

func pageOf(r *http.Request, per int, total int64) paging {
	p := paging{Per: per, Total: total, Pages: int(max(1, (total+int64(per)-1)/int64(per)))}
	p.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	p.Page = max(0, min(p.Page, p.Pages-1))
	return p
}

func (a *App) adminImport(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, err := a.loadUploads(r.Context(), `WHERE id=?`, id)
	if err != nil || len(rows) == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "Import not found.")
		return
	}
	writeJSON(w, http.StatusOK, rows[0])
}

func (a *App) adminImportCancel(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ok := a.Imports.Cancel(id)
	if ok {
		a.audit(r.Context(), s.Username, clientIP(r), "import.cancel", fmt.Sprint(id))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": ok})
}

type notifyIn struct {
	Subject string `json:"subject"`
	Intro   string `json:"intro"`
}

func (a *App) adminNotify(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in notifyIn
	if !decodeJSON(w, r, &in, 16<<10) {
		return
	}
	if len(in.Subject) > 150 || len(in.Intro) > 1500 {
		writeErr(w, http.StatusBadRequest, "too_long", "Subject (150) or intro (1500) is too long.")
		return
	}
	cid, total, err := a.Outbox.Enqueue(r.Context(), id, strings.TrimSpace(in.Subject), strings.TrimSpace(in.Intro))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, mail.ErrAlreadyNotified) {
			code = http.StatusConflict
		}
		writeErr(w, code, "notify_failed", err.Error())
		return
	}
	st := a.Outbox.Status(r.Context())
	a.audit(r.Context(), s.Username, clientIP(r), "notify", fmt.Sprintf("upload #%d campaign #%d recipients=%d", id, cid, total))
	writeJSON(w, http.StatusOK, map[string]any{"campaign": cid, "recipients": total, "days_to_done": st.DaysToDone, "bulk_left_today": st.BulkLeft})
}

func (a *App) adminOutbox(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	writeJSON(w, http.StatusOK, a.Outbox.Status(r.Context()))
}

func (a *App) adminOutboxPause(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in struct {
		Paused bool `json:"paused"`
	}
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	a.Outbox.Pause(in.Paused)
	a.audit(r.Context(), s.Username, clientIP(r), "outbox.pause", strconv.FormatBool(in.Paused))
	writeJSON(w, http.StatusOK, a.Outbox.Status(r.Context()))
}

func (a *App) adminTestEmail(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in struct {
		To string `json:"to"`
	}
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	to, ok, _ := emailcheck.Syntax(in.To)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_email", "Enter a valid email address.")
		return
	}
	p, err := a.Mailer.Send(r.Context(), mail.Priority, mail.Message{To: to, Subject: "Free Direct Dials test email",
		Text: "If you can read this, outgoing email works.", HTML: "<p>If you can read this, outgoing email works.</p>"})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "send_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"provider": p})
}

// ---------- settings ----------

func (a *App) adminSettings(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	writeJSON(w, http.StatusOK, map[string]any{"settings": a.Settings.Get(), "public": json.RawMessage(a.Settings.PublicJSON())})
}

func (a *App) adminSettingsSave(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	raw := make([]byte, 0, 4096)
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	all, err := a.Settings.Save(r.Context(), r.PathValue("section"), raw, s.Username)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid", err.Error())
		return
	}
	a.pages.Purge()
	a.purgeCDN(r.Context(), "/", "/api/config")
	writeJSON(w, http.StatusOK, map[string]any{"settings": all, "public": json.RawMessage(a.Settings.PublicJSON())})
}

// ---------- blocklist ----------

func (a *App) adminBlocklist(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	rows, err := a.DB.R.QueryContext(r.Context(), `SELECT domain, source FROM blocklist WHERE domain LIKE ? ORDER BY domain LIMIT 500`, "%"+q+"%")
	if err != nil {
		a.queryError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var d, src string
		rows.Scan(&d, &src)
		out = append(out, map[string]string{"domain": d, "source": src})
	}
	b, ad := a.Disposable.Size()
	writeJSON(w, http.StatusOK, map[string]any{"domains": out, "builtin": b, "admin": ad})
}

func (a *App) adminBlocklistAdd(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in struct {
		Domains string `json:"domains"`
	}
	if !decodeJSON(w, r, &in, 2<<20) {
		return
	}
	added, bad := 0, []string{}
	for _, line := range strings.FieldsFunc(in.Domains, func(c rune) bool { return c == '\n' || c == ',' || c == ' ' || c == ';' || c == '\r' || c == '\t' }) {
		d, ok := emailcheck.NormalizeDomain(line)
		if !ok {
			if len(bad) < 20 {
				bad = append(bad, line)
			}
			continue
		}
		if res, err := a.DB.W.ExecContext(r.Context(), `INSERT OR IGNORE INTO blocklist(domain, source) VALUES(?, 'admin')`, d); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				added++
			}
		}
	}
	a.Disposable.Reload(r.Context(), a.DB.R)
	a.audit(r.Context(), s.Username, clientIP(r), "blocklist.add", fmt.Sprintf("%d added", added))
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "invalid": bad})
}

func (a *App) adminBlocklistDelete(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	d := strings.ToLower(r.PathValue("domain"))
	a.DB.W.ExecContext(r.Context(), `DELETE FROM blocklist WHERE domain=?`, d)
	a.Disposable.Reload(r.Context(), a.DB.R)
	a.audit(r.Context(), s.Username, clientIP(r), "blocklist.delete", d)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- removals & contacts ----------

func (a *App) adminRemovals(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	pg := pageOf(r, 50, scalarQ(r.Context(), a.DB.R, `SELECT COUNT(*) FROM removals WHERE status=?`, status))
	rows, err := a.DB.R.QueryContext(r.Context(), `SELECT id, kind, value, note, status, ts, done_ts FROM removals WHERE status=? ORDER BY id DESC LIMIT ? OFFSET ?`,
		status, pg.Per, pg.offset())
	if err != nil {
		a.queryError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, ts, done int64
		var kind, value, note, st string
		rows.Scan(&id, &kind, &value, &note, &st, &ts, &done)
		out = append(out, map[string]any{"id": id, "kind": kind, "value": value, "note": note, "status": st, "ts": ts, "done_ts": done,
			"due": ts + 30*86400})
	}
	writeJSON(w, http.StatusOK, map[string]any{"removals": out, "paging": pg})
}

type removalIn struct {
	Action string `json:"action"` // delete | reject
	Kind   string `json:"kind"`   // override for "other" requests: email | linkedin | phone
	Value  string `json:"value"`
}

func (a *App) adminRemovalProcess(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in removalIn
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	ctx := r.Context()
	var kind, value string
	if err := a.DB.R.QueryRowContext(ctx, `SELECT kind, value FROM removals WHERE id=? AND status='pending'`, id).Scan(&kind, &value); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "Request not found or already processed.")
		return
	}
	now := a.Clock.Now().Unix()
	if in.Action == "reject" {
		a.DB.W.ExecContext(ctx, `UPDATE removals SET status='rejected', done_ts=? WHERE id=?`, now, id)
		a.audit(ctx, s.Username, clientIP(r), "removal.reject", fmt.Sprint(id))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if in.Kind != "" {
		kind, value = in.Kind, in.Value
	}
	n, err := a.removeIdentity(ctx, kind, value, now)
	if errors.Is(err, store.ErrUnavailable) || (err != nil && n > 0) {
		writeErr(w, http.StatusServiceUnavailable, "store_down", fmt.Sprintf("%d record(s) deleted, but %v. The identity is blocked from uploads; process again when every database is online.", n, err))
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_identity", err.Error())
		return
	}
	a.DB.W.ExecContext(ctx, `UPDATE removals SET status='done', done_ts=?, note=note || ? WHERE id=?`, now, fmt.Sprintf("\nprocessed by %s: %d record(s) deleted", s.Username, n), id)
	a.audit(ctx, s.Username, clientIP(r), "removal.done", fmt.Sprintf("#%d %s → %d deleted", id, kind, n))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}

// removeIdentity blocks an identity from future uploads, then deletes every
// matching contact in every store.
func (a *App) removeIdentity(ctx context.Context, kind, value string, now int64) (int, error) {
	if err := importer.Suppress(ctx, a.DB, kind, value, now); err != nil {
		return 0, err
	}
	n, err := a.Store.Remove(ctx, kind, value, now)
	if n > 0 {
		a.purgeData(context.WithoutCancel(ctx))
	}
	return n, err
}

func (a *App) adminContactRemove(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	var in removalIn
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	n, err := a.removeIdentity(r.Context(), in.Kind, in.Value, a.Clock.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_identity", err.Error())
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "contact.remove", fmt.Sprintf("%s → %d deleted", in.Kind, n))
	writeJSON(w, http.StatusOK, map[string]int{"removed": n})
}

// ---------- users ----------

func (a *App) adminUsers(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	pg := pageOf(r, 50, scalarQ(r.Context(), a.DB.R, `SELECT COUNT(*) FROM users WHERE email LIKE ? ESCAPE '\'`, like))
	rows, err := a.DB.R.QueryContext(r.Context(), `SELECT u.id, u.email, u.created, u.last_seen, u.subscribed, u.blocked, u.bounced,
		(SELECT COALESCE(SUM(ads),0) FROM user_daily x WHERE x.user_id=u.id) FROM users u WHERE u.email LIKE ? ESCAPE '\'
		ORDER BY u.last_seen DESC LIMIT ? OFFSET ?`, like, pg.Per, pg.offset())
	if err != nil {
		a.queryError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created, seen, n int64
		var email string
		var sub, blk, bnc int
		rows.Scan(&id, &email, &created, &seen, &sub, &blk, &bnc, &n)
		out = append(out, map[string]any{"id": id, "email": email, "created": created, "last_seen": seen, "subscribed": sub == 1,
			"blocked": blk == 1, "bounced": bnc == 1, "breaks": n})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "paging": pg})
}

func (a *App) adminUserBlock(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Blocked bool `json:"blocked"`
	}
	if !decodeJSON(w, r, &in, 1<<10) {
		return
	}
	v := 0
	if in.Blocked {
		v = 1
		a.DB.W.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id=?`, id)
	}
	a.DB.W.ExecContext(r.Context(), `UPDATE users SET blocked=? WHERE id=?`, v, id)
	a.audit(r.Context(), s.Username, clientIP(r), "user.block", fmt.Sprintf("%d=%v", id, in.Blocked))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) adminUserDelete(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	// unlocks live in every store; drop them first so a reused id can never inherit them
	if err := a.Store.ForgetUser(r.Context(), id); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "store_down", "A database is unreachable, so this user's unlocks can't be deleted yet. Try again shortly.")
		return
	}
	if err := a.Users.Delete(r.Context(), id); err != nil {
		a.queryError(w, err)
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "user.delete", fmt.Sprint(id))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- audit, backups, jobs, cache ----------

func (a *App) adminAudit(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if before <= 0 {
		before = 1 << 62
	}
	rows, err := a.DB.R.QueryContext(r.Context(), `SELECT id, ts, actor, ip, action, detail FROM audit WHERE id<? ORDER BY id DESC LIMIT 100`, before)
	if err != nil {
		a.queryError(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, ts int64
		var actor, ip, action, detail string
		rows.Scan(&id, &ts, &actor, &ip, &action, &detail)
		out = append(out, map[string]any{"id": id, "ts": ts, "actor": actor, "ip": ip, "action": action, "detail": detail})
	}
	next := int64(0)
	if len(out) == 100 {
		next = out[99]["id"].(int64)
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": out, "next": next})
}

func (a *App) adminBackups(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	writeJSON(w, http.StatusOK, map[string]any{"backups": a.Backup.List()})
}

func (a *App) adminJobRun(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	name := r.PathValue("name")
	started, err := a.Sched.RunNow(context.WithoutCancel(r.Context()), name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	a.audit(r.Context(), s.Username, clientIP(r), "job.run", name)
	writeJSON(w, http.StatusOK, map[string]bool{"started": started})
}

func (a *App) adminCachePurge(w http.ResponseWriter, r *http.Request, s auth.AdminSession) {
	a.purgeData(r.Context())
	a.audit(r.Context(), s.Username, clientIP(r), "cache.purge", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
