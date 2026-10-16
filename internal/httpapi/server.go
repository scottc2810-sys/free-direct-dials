// Package httpapi exposes the public site, visitor API, superadmin API and
// operational endpoints over net/http.
package httpapi

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"markemind/dials/internal/auth"
	"markemind/dials/internal/config"
	"markemind/dials/internal/db"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/jobs"
	"markemind/dials/internal/mail"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/search"
	"markemind/dials/internal/settings"
	"markemind/dials/internal/store"
	"markemind/dials/internal/unlock"
	"markemind/dials/web"
)

// App wires every dependency the handlers need.
type App struct {
	Cfg        *config.Config
	DB         *db.DB
	Log        *slog.Logger
	Clock      platform.Clock
	Store      *store.Federation // every contact database (built-in + Supabase/Postgres)
	Unlock     *unlock.Service
	Settings   *settings.Store
	Users      *auth.Users
	Sessions   *auth.Sessions
	OTP        *auth.OTP
	Admins     *auth.Admins
	Turnstile  *auth.Turnstile
	Checker    *emailcheck.Checker
	Disposable *emailcheck.Disposable
	Mailer     *mail.Mailer
	Outbox     *mail.Outbox
	Importer   *importer.Importer
	Guard      *jobs.Guard
	Sched      *jobs.Scheduler
	Backup     *jobs.Backup
	Alerter    *jobs.Alerter
	Assets     *web.Assets
	Tpl        *web.Templates
	Started    time.Time

	Imports *ImportManager
	Uploads *UploadStore

	pages     *search.LRU // rendered HTML
	limits    limits
	purgeMu   sync.Mutex
	exportSem chan struct{} // at most 2 concurrent CSV exports (memory/IO on a small box)
}

type limits struct {
	api, authStart, authVerify, adminLogin, adminDenied, removal, export *auth.Limiter
}

// Init finishes construction (call once before Handler).
func (a *App) Init() error {
	now := a.Clock.Now
	a.limits = limits{
		api:         auth.NewLimiter(240, time.Minute, 100_000, now),
		authStart:   auth.NewLimiter(10, time.Hour, 100_000, now),
		authVerify:  auth.NewLimiter(30, time.Hour, 100_000, now),
		adminLogin:  auth.NewLimiter(10, 15*time.Minute, 10_000, now),
		adminDenied: auth.NewLimiter(3, time.Hour, 10_000, now),
		removal:     auth.NewLimiter(5, time.Hour, 50_000, now),
		export:      auth.NewLimiter(10, time.Hour, 50_000, now),
	}
	a.pages = search.NewLRU(8<<20, now)
	a.exportSem = make(chan struct{}, 2)
	if a.Started.IsZero() {
		a.Started = now()
	}
	var err error
	if a.Assets == nil {
		if a.Assets, err = web.LoadAssets(a.Cfg.BasePath); err != nil {
			return err
		}
	}
	if a.Tpl == nil {
		if a.Tpl, err = web.LoadTemplates(template.FuncMap{
			"asset": a.Assets.URL,
			"base":  func() string { return a.Cfg.BasePath },
			"abs":   a.Cfg.URL,
		}); err != nil {
			return err
		}
	}
	if a.Uploads == nil {
		a.Uploads = &UploadStore{Dir: a.Cfg.TmpDir, Clock: a.Clock}
	}
	if a.Imports == nil {
		a.Imports = &ImportManager{app: a}
	}
	return a.Imports.recoverInterrupted(context.Background())
}

// Handler returns the root handler with all middleware applied.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	a.routes(mux)
	return a.middleware(mux)
}

func (a *App) routes(m *http.ServeMux) {
	b := a.Cfg.BasePath
	h := func(pattern string, fn http.HandlerFunc) {
		method, p, _ := strings.Cut(pattern, " ")
		m.HandleFunc(method+" "+b+p, fn)
	}
	// pages
	if b != "" {
		m.HandleFunc("GET "+b, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, b+"/", http.StatusMovedPermanently) })
		if b == "/free-direct-dials" { // the original MVP spec's typo
			m.HandleFunc("GET /free-direct-dails/", a.redirectTypo)
			m.HandleFunc("GET /free-direct-dails", a.redirectTypo)
		}
	}
	h("GET /{$}", a.pageIndex)
	h("GET /country/{c}/{$}", a.pageSEO)
	h("GET /country/{c}/{i}/{$}", a.pageSEO)
	h("GET /industry/{i}/{$}", a.pageSEO)
	h("GET /country/{c}", a.slashRedirect)
	h("GET /country/{c}/{i}", a.slashRedirect)
	h("GET /industry/{i}", a.slashRedirect)
	h("GET /privacy", a.pageStatic("privacy"))
	h("GET /terms", a.pageStatic("terms"))
	h("GET /remove", a.pageStatic("remove"))
	h("GET /unsubscribe", a.pageUnsubscribe)
	h("GET /sitemap.xml", a.sitemap)
	h("GET /llms.txt", a.llms)
	if b == "" {
		m.HandleFunc("GET /robots.txt", a.robots)
	}
	h("GET /favicon.svg", a.favicon)
	h("GET /assets/{file}", func(w http.ResponseWriter, r *http.Request) {
		f := r.PathValue("file")
		if strings.HasPrefix(f, "admin.") && !a.adminAllowed(r) { // the panel's code is IP-restricted too
			http.NotFound(w, r)
			return
		}
		a.Assets.Serve(w, r, f)
	})
	h("GET /ad-frame", a.adFrame)
	h("GET /ads.txt", a.adsTxt)
	if b != "" { // ad networks only read ads.txt at the domain root
		m.HandleFunc("GET /ads.txt", a.adsTxt)
	}
	h("GET /healthz", a.healthz)
	h("GET /readyz", a.readyz)

	// public, cacheable API
	h("GET /api/config", a.apiConfig)
	h("GET /api/stats", a.apiStats)
	h("GET /api/facets", a.apiFacets)
	h("GET /api/preview", a.apiPreview)

	// visitor API (personal, never cached)
	h("POST /api/auth/start", a.apiAuthStart)
	h("POST /api/auth/verify", a.apiAuthVerify)
	h("POST /api/auth/logout", a.apiLogout)
	h("GET /api/me", a.apiMe)
	h("GET /api/me/view", a.apiView)
	h("GET /api/me/export", a.apiExport)
	h("POST /api/ad/start", a.apiAdStart)
	h("POST /api/ad/claim", a.apiAdClaim)
	h("POST /api/unsub", a.apiUnsub)
	h("POST /api/remove/start", a.apiRemoveStart)
	h("POST /api/remove/verify", a.apiRemoveVerify)
	h("POST /api/remove/request", a.apiRemoveRequest)
	h("POST /api/hooks/mail/{provider}", a.apiMailHook)

	a.adminRoutes(m)
}

func (a *App) redirectTypo(w http.ResponseWriter, r *http.Request) {
	target := "/free-direct-dials" + strings.TrimPrefix(r.URL.Path, "/free-direct-dails")
	if target == "/free-direct-dials" {
		target += "/"
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func (a *App) slashRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
}
