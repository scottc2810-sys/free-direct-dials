package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/jobs"
	"markemind/dials/internal/normalize"
	"markemind/dials/internal/search"
	"markemind/dials/internal/seo"
	"markemind/dials/internal/settings"
	"markemind/dials/internal/store"
)

// ---------- page rendering ----------

type FAQ struct{ Q, A string }

type Crumb struct{ Name, URL string }

type Related struct {
	Label string
	URL   string
	Count int64
}

type pageData struct {
	Page        string
	Title       string
	Description string
	Canonical   string
	Base        string
	NoIndex     bool
	Stats       importer.HeadlineStats
	FAQs        []FAQ
	JSONLD      []any
	Site        settings.Site
	ContactsAd  int
	MaxAds      int

	// SEO landing pages
	H1       string
	Lead     string
	Count    int64
	Rows     []search.Row
	ToolURL  string
	Related  []Related
	Crumbs   []Crumb
	Filter   string
	Status   int
	Token    string // unsubscribe page
	Done     bool
	Response string
}

func (a *App) baseData(page string) pageData {
	cfg := a.Settings.Get()
	return pageData{Page: page, Base: a.Cfg.BasePath, Stats: a.Store.Stats(context.Background()), Site: cfg.Site,
		ContactsAd: cfg.Limits.ContactsPerAd, MaxAds: cfg.Limits.MaxAdsPerDay}
}

// publicCSP is strict unless the admin enabled a "direct" ad snippet or Google
// rewarded ads: both must run third-party (and inline) script in the page.
func (a *App) publicCSP() string {
	cfg := a.Settings.Get()
	direct := cfg.Ads.Mode == "gpt_rewarded"
	for _, s := range cfg.Ads.Slots {
		if s.Enabled && s.Isolation == "direct" {
			direct = true
		}
	}
	script := "'self' https://challenges.cloudflare.com"
	if direct {
		script = "'self' 'unsafe-inline' 'unsafe-eval' https: http:"
	}
	return "default-src 'self'; script-src " + script + "; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; " +
		"font-src 'self'; connect-src 'self' https:; frame-src 'self' https: ; media-src https:; object-src 'none'; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; upgrade-insecure-requests"
}

// render writes an HTML page, memoised per dataset + settings version.
func (a *App) render(w http.ResponseWriter, r *http.Request, name, key string, sMaxAge int, build func() (pageData, error)) {
	ck := fmt.Sprintf("%s|%d|%d|%s", name, a.Store.Version(), a.Settings.Version(), key)
	body, ok := a.pages.Get(ck)
	status := http.StatusOK
	if ok && len(body) > 3 && body[0] == '#' { // status prefix "#410\n"
		status, _ = strconv.Atoi(string(body[1:4]))
		body = body[5:]
	}
	if !ok {
		d, err := build()
		if err != nil {
			a.Log.Error("page build", "page", name, "err", err)
			if errors.Is(err, search.ErrBusy) {
				w.Header().Set("Retry-After", "5")
				http.Error(w, "Busy, please retry in a few seconds.", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "Something went wrong. Please retry.", http.StatusInternalServerError)
			return
		}
		var buf bytes.Buffer
		if err := a.Tpl.Render(&buf, d.Page, d); err != nil {
			a.Log.Error("template", "page", d.Page, "err", err)
			http.Error(w, "Something went wrong. Please retry.", http.StatusInternalServerError)
			return
		}
		body = buf.Bytes()
		if d.Status != 0 {
			status = d.Status
		}
		stored := body
		if status != http.StatusOK {
			stored = append([]byte(fmt.Sprintf("#%d\n", status)), body...)
		}
		a.pages.Set(ck, stored, 10*time.Minute)
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", a.publicCSP())
	h.Set("X-Frame-Options", "DENY")
	if sMaxAge > 0 {
		cachePublic(w, sMaxAge)
	} else {
		noStore(w)
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

func (a *App) faqs() []FAQ {
	cfg := a.Settings.Get()
	n, m := cfg.Limits.ContactsPerAd, cfg.Limits.MaxAdsPerDay
	return []FAQ{
		{"Is it really free?", "Yes. Browsing, filtering and counts never need an account. To see full emails and direct dials you confirm a real email address once, then each short sponsor break unlocks " + strconv.Itoa(n) + " new contacts. The sponsors pay for the data."},
		{"How many contacts can I unlock?", strconv.Itoa(n) + " per sponsor break, up to " + strconv.Itoa(m) + " breaks a day. You never receive the same contact twice, even when you change filters."},
		{"How is the data cleaned?", "On every import, emails are syntax-checked and screened against disposable-mailbox domains, phone numbers need at least six digits, blank markers like N/A are removed and duplicates are merged."},
		{"Can I get a bigger or custom list?", "Yes. Message us on WhatsApp with the countries, industries, job titles and volume you need and we will quote a custom or bulk export."},
		{"How do I remove my details?", "Open the removal page, confirm your email with a one-time code and your record is deleted immediately and blocked from future uploads."},
		{"What do you do with my email?", "We use it to sign you in and, if you agree, to tell you when new contacts are added. Every email has a one-click unsubscribe link. We never sell it."},
	}
}

func (a *App) jsonLDFAQ(fs []FAQ) map[string]any {
	var qs []map[string]any
	for _, f := range fs {
		qs = append(qs, map[string]any{"@type": "Question", "name": f.Q, "acceptedAnswer": map[string]any{"@type": "Answer", "text": f.A}})
	}
	return map[string]any{"@context": "https://schema.org", "@type": "FAQPage", "mainEntity": qs}
}

func (a *App) pageIndex(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "index", "", 600, func() (pageData, error) {
		d := a.baseData("index")
		d.Title = "Free Direct Dials: free B2B contacts with direct phone numbers and emails"
		d.Description = fmt.Sprintf("Search %s B2B decision-makers by country, industry, seniority and technology. Direct dials and verified emails, free.",
			approxOr(d.Stats.Contacts, "thousands of"))
		d.Canonical = a.Cfg.URL("/")
		d.FAQs = a.faqs()
		org := map[string]any{"@type": "Organization", "name": orDefault(d.Site.LegalName, "MarkeMind AI"), "url": a.Cfg.PublicOrigin}
		d.JSONLD = []any{
			map[string]any{"@context": "https://schema.org", "@type": "WebApplication", "name": "Free Direct Dials", "url": d.Canonical,
				"applicationCategory": "BusinessApplication", "operatingSystem": "Any", "isAccessibleForFree": true,
				"offers": map[string]any{"@type": "Offer", "price": "0", "priceCurrency": "USD"}, "publisher": org},
			map[string]any{"@context": "https://schema.org", "@type": "Dataset", "name": "Free Direct Dials B2B contact directory",
				"description": fmt.Sprintf("A free directory of %d B2B contacts (%d with verified-format emails, %d with direct phone numbers) across %d countries. Filterable by country, state, city, industry, sector, company type and size, revenue, seniority, department, decision power and technology.",
					d.Stats.Contacts, d.Stats.WithEmail, d.Stats.WithPhone, d.Stats.Countries),
				"url": d.Canonical, "isAccessibleForFree": true, "creator": org,
				"keywords":         []string{"B2B contacts", "direct dials", "business emails", "sales leads", "decision makers"},
				"variableMeasured": []string{"name", "job title", "company", "email", "phone", "country", "industry", "seniority"},
				"dateModified":     time.Unix(max(d.Stats.Updated, 1), 0).UTC().Format("2006-01-02")},
			a.jsonLDFAQ(d.FAQs),
		}
		return d, nil
	})
}

func approxOr(n int64, fallback string) string {
	if n < 100 {
		return fallback
	}
	s := strings.Split(webApprox(n), " ")
	return strings.Join(s, " ")
}

func webApprox(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f million", float64(n/100_000)/10)
	case n >= 10_000:
		return strconv.FormatInt(n/1000, 10) + ",000"
	}
	return strconv.FormatInt(n, 10)
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func (a *App) pageStatic(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.render(w, r, name, "", 3600, func() (pageData, error) {
			d := a.baseData(name)
			switch name {
			case "privacy":
				d.Title, d.Description = "Privacy Policy | Free Direct Dials", "How Free Direct Dials collects, uses and protects personal data."
			case "terms":
				d.Title, d.Description = "Terms of Use | Free Direct Dials", "The rules for using the Free Direct Dials directory."
			case "remove":
				d.Title, d.Description = "Remove your details | Free Direct Dials", "Delete your contact record from Free Direct Dials and block it from future uploads."
			}
			d.Canonical = a.Cfg.URL("/" + name)
			return d, nil
		})
	}
}

func (a *App) pageUnsubscribe(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("t")
	if len(tok) > 100 {
		tok = ""
	}
	d := a.baseData("unsubscribe")
	d.Title, d.NoIndex, d.Token = "Unsubscribe | Free Direct Dials", true, tok
	var buf bytes.Buffer
	if err := a.Tpl.Render(&buf, "unsubscribe", d); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", a.publicCSP())
	noStore(w)
	w.Write(buf.Bytes())
}

// ---------- SEO landing pages ----------

func (a *App) pageSEO(w http.ResponseWriter, r *http.Request) {
	parts := []string{}
	if strings.Contains(r.URL.Path, "/country/") {
		parts = append(parts, "country", r.PathValue("c"))
		if i := r.PathValue("i"); i != "" {
			parts = append(parts, i)
		}
	} else {
		parts = append(parts, "industry", r.PathValue("i"))
	}
	slug := strings.Join(parts, "/")
	if len(slug) > 200 {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "seo", slug, 3600, func() (pageData, error) {
		d := a.baseData("seo")
		p, ok := seo.Lookup(r.Context(), a.DB.R, slug)
		if !ok || p.Gone {
			d.Page, d.NoIndex = "error", true
			d.Status = http.StatusNotFound
			d.Title, d.H1, d.Lead = "Page not found | Free Direct Dials", "This page isn't here", "The list you were looking for doesn't exist. Search the full directory instead."
			if ok && p.Gone {
				d.Status = http.StatusGone
				d.H1, d.Lead = "This list is no longer published", "It fell below our minimum size. The full directory still has thousands of contacts to filter."
			}
			return d, nil
		}
		ctx := r.Context()
		q := url.Values{}
		if p.Country != "" {
			q.Set("country", p.Country)
		}
		if p.Industry != "" {
			q.Set("industry", p.Industry)
		}
		f, _ := filter.Parse(q)
		pv, err := a.Store.Preview(store.WithWait(ctx), f, 0, search.PreviewLimit, 1) // cached as HTML: must be complete
		if err != nil {
			return d, err
		}
		total, rows := pv.Total, pv.Rows
		cn := a.label(ctx, "country", p.Country)
		in := a.label(ctx, "industry", p.Industry)
		switch p.Kind {
		case "country":
			d.H1 = "Free B2B contacts in " + cn
		case "industry":
			d.H1 = "Free " + in + " contacts"
		default:
			d.H1 = "Free " + in + " contacts in " + cn
		}
		d.Count, d.Rows, d.Filter = total, rows, f.Canonical()
		d.Lead = fmt.Sprintf("%s decision-makers with direct dials and business emails. Filter further by seniority, department, company size and technology, then unlock %d contacts per sponsor break.",
			webThousands(total), d.ContactsAd)
		d.Title = fmt.Sprintf("%s (%s) | Free Direct Dials", d.H1, webThousands(total))
		d.Description = fmt.Sprintf("Browse %s free %s with direct phone numbers and verified emails. No signup needed to search.", webThousands(total), strings.TrimPrefix(d.H1, "Free "))
		d.Canonical = a.Cfg.URL("/" + slug + "/")
		d.ToolURL = a.Cfg.BasePath + "/#" + d.Filter
		d.Crumbs = []Crumb{{"Free Direct Dials", a.Cfg.URL("/")}}
		if p.Kind == "combo" {
			if cp := a.findPage(ctx, "country", p.Country, ""); cp != "" {
				d.Crumbs = append(d.Crumbs, Crumb{cn, a.Cfg.URL("/" + cp + "/")})
			}
		}
		d.Crumbs = append(d.Crumbs, Crumb{d.H1, d.Canonical})
		var rel []seo.Page
		switch p.Kind {
		case "country":
			rel, _ = seo.List(ctx, a.DB.R, "combo", p.Country, "", 16)
		case "industry":
			rel, _ = seo.List(ctx, a.DB.R, "combo", "", p.Industry, 16)
		default:
			rel, _ = seo.List(ctx, a.DB.R, "combo", p.Country, "", 13)
		}
		var cs, is []string
		for _, x := range rel {
			cs, is = append(cs, x.Country), append(is, x.Industry)
		}
		cl, il := a.Store.Labels(ctx, "country", cs), a.Store.Labels(ctx, "industry", is)
		for _, x := range rel {
			if x.Slug == p.Slug {
				continue
			}
			label := orDefault(il[x.Industry], pretty(x.Industry)) + " in " + orDefault(cl[x.Country], pretty(x.Country))
			d.Related = append(d.Related, Related{Label: label, URL: a.Cfg.BasePath + "/" + x.Slug + "/", Count: x.Count})
		}
		var items []map[string]any
		for i, c := range d.Crumbs {
			items = append(items, map[string]any{"@type": "ListItem", "position": i + 1, "name": c.Name, "item": c.URL})
		}
		d.JSONLD = []any{
			map[string]any{"@context": "https://schema.org", "@type": "BreadcrumbList", "itemListElement": items},
			map[string]any{"@context": "https://schema.org", "@type": "Dataset", "name": d.H1, "description": d.Lead, "url": d.Canonical,
				"isAccessibleForFree": true, "creator": map[string]any{"@type": "Organization", "name": orDefault(d.Site.LegalName, "MarkeMind AI")}},
		}
		return d, nil
	})
}

func webThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func (a *App) findPage(ctx context.Context, kind, country, industry string) string {
	ps, _ := seo.List(ctx, a.DB.R, kind, country, industry, 1)
	if len(ps) == 0 {
		return ""
	}
	return ps[0].Slug
}

// label is the display spelling of a normalised value ("" stays "").
func (a *App) label(ctx context.Context, dim, value string) string {
	if value == "" {
		return ""
	}
	return orDefault(a.Store.Labels(ctx, dim, []string{value})[value], pretty(value))
}

// pretty title-cases a normalised value when no store knows its label.
func pretty(v string) string {
	b := []rune(v)
	up := true
	for i, r := range b {
		if up && r >= 'a' && r <= 'z' {
			b[i] = r - 32
		}
		up = r == ' ' || r == '-'
	}
	return string(b)
}

// ---------- sitemap / llms / robots / favicon / ad frame ----------

type urlset struct {
	XMLName xml.Name `xml:"urlset"`
	NS      string   `xml:"xmlns,attr"`
	URLs    []smURL  `xml:"url"`
}

type smURL struct {
	Loc      string `xml:"loc"`
	LastMod  string `xml:"lastmod,omitempty"`
	Priority string `xml:"priority,omitempty"`
}

func (a *App) sitemap(w http.ResponseWriter, r *http.Request) {
	st := a.Store.Stats(r.Context())
	day := time.Unix(max(st.Updated, 1), 0).UTC().Format("2006-01-02")
	us := urlset{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	us.URLs = append(us.URLs, smURL{a.Cfg.URL("/"), day, "1.0"})
	pages, _ := seo.List(r.Context(), a.DB.R, "", "", "", 2000)
	for _, p := range pages {
		us.URLs = append(us.URLs, smURL{a.Cfg.URL("/" + p.Slug + "/"), time.Unix(p.Updated, 0).UTC().Format("2006-01-02"), "0.7"})
	}
	us.URLs = append(us.URLs, smURL{Loc: a.Cfg.URL("/remove"), Priority: "0.3"}, smURL{Loc: a.Cfg.URL("/privacy"), Priority: "0.2"},
		smURL{Loc: a.Cfg.URL("/terms"), Priority: "0.2"})
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	cachePublic(w, 3600)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(us)
}

func (a *App) llms(w http.ResponseWriter, r *http.Request) {
	st := a.Store.Stats(r.Context())
	cfg := a.Settings.Get()
	var b strings.Builder
	fmt.Fprintf(&b, "# Free Direct Dials\n\n> A free B2B contact directory by %s: %s contacts with direct phone numbers and business emails across %d countries.\n\n",
		orDefault(cfg.Site.LegalName, "MarkeMind AI"), webThousands(st.Contacts), st.Countries)
	b.WriteString("Anyone can search and filter without an account. Full emails and phone numbers are unlocked " +
		strconv.Itoa(cfg.Limits.ContactsPerAd) + " at a time after confirming an email address and a short sponsor break. It is free; it is funded by sponsors and donations.\n\n")
	b.WriteString("## Filters\n\nCountry, state/region, city, industry, sector, company type, company size, revenue, seniority, department, decision power, email confidence, technology (prefix match), keyword (name, title, company, skills, products), has email, has phone.\n\n")
	b.WriteString("## URL patterns\n\n")
	fmt.Fprintf(&b, "- Directory: %s\n- Pre-filtered tool: %s#country=india&industry=software\n- Country lists: %s\n- Industry lists: %s\n- Country × industry: %s\n- Remove a record: %s\n- Sitemap: %s\n\n",
		a.Cfg.URL("/"), a.Cfg.URL("/"), a.Cfg.URL("/country/{country}/"), a.Cfg.URL("/industry/{industry}/"),
		a.Cfg.URL("/country/{country}/{industry}/"), a.Cfg.URL("/remove"), a.Cfg.URL("/sitemap.xml"))
	b.WriteString("## Popular lists\n\n")
	pages, _ := seo.List(r.Context(), a.DB.R, "", "", "", 25)
	for _, p := range pages {
		fmt.Fprintf(&b, "- %s (%s contacts)\n", a.Cfg.URL("/"+p.Slug+"/"), webThousands(p.Count))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	cachePublic(w, 3600)
	w.Write([]byte(b.String()))
}

func (a *App) robots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	cachePublic(w, 86400)
	fmt.Fprintf(w, "User-agent: *\nAllow: /\nDisallow: /api/\nSitemap: %s\n", a.Cfg.URL("/sitemap.xml"))
}

func (a *App) favicon(w http.ResponseWriter, r *http.Request) {
	b, ct, ok := a.Assets.Raw("favicon.svg")
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

// adFrame serves one ad slot as its own document. The page embeds it in a
// sandboxed iframe without allow-same-origin, so third-party ad code runs in
// an opaque origin and cannot read cookies or touch the page.
func (a *App) adFrame(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("slot")
	slot, ok := a.Settings.Get().Ads.Slots[name]
	if !ok || name == "global" || !slot.Enabled {
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	html := slot.HTML
	if r.URL.Query().Get("m") == "1" && strings.TrimSpace(slot.HTMLMobile) != "" {
		html = slot.HTMLMobile
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src * data: blob: 'unsafe-inline' 'unsafe-eval'; frame-ancestors 'self'")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	cachePublic(w, 60)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><style>html,body{margin:0;padding:0;background:transparent;overflow:hidden}body{display:flex;align-items:center;justify-content:center;min-height:100vh}</style></head><body>%s</body></html>`, html)
}

// adsTxt serves the Authorized Digital Sellers file set in Settings → Ads.
// Empty means 404, which ad networks read as "no restriction".
func (a *App) adsTxt(w http.ResponseWriter, r *http.Request) {
	body := a.Settings.Get().Ads.AdsTxt
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	cachePublic(w, 3600)
	if strings.TrimSpace(body) == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Write([]byte(body + "\n"))
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Write([]byte("ok"))
}

func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var one int
	status := http.StatusOK
	checks := map[string]any{}
	if err := a.DB.R.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		status, checks["reader"] = http.StatusServiceUnavailable, err.Error()
	}
	if err := a.DB.W.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		status, checks["writer"] = http.StatusServiceUnavailable, err.Error()
	}
	lv, pct, _ := a.Guard.Check()
	checks["disk_pct"], checks["disk_level"] = int(pct), lv
	if lv >= jobs.LevelReadOnly {
		status = http.StatusServiceUnavailable
	}
	checks["dataset_version"], checks["schema"] = a.Store.Version(), a.DB.SchemaVersion(ctx)
	down := 0
	for _, s := range a.Store.Statuses(ctx) {
		if s.State == "down" {
			down++
		}
	}
	checks["stores_down"] = down // partial results, not an outage: readiness stays green
	checks["ok"] = status == http.StatusOK
	writeJSON(w, status, checks)
}

// ---------- public JSON API (cacheable; never reads cookies) ----------

func (a *App) apiConfig(w http.ResponseWriter, r *http.Request) {
	cachePublic(w, 60)
	var b bytes.Buffer
	b.WriteString(`{"base":`)
	jb, _ := json.Marshal(a.Cfg.BasePath)
	b.Write(jb)
	b.WriteString(`,"turnstile":`)
	jt, _ := json.Marshal(a.Cfg.TurnstileSiteKey)
	b.Write(jt)
	fmt.Fprintf(&b, `,"dataset":%d,"settings":`, a.Store.Version())
	b.Write(a.Settings.PublicJSON())
	b.WriteString(`}`)
	writeRaw(w, http.StatusOK, b.Bytes())
}

func (a *App) apiStats(w http.ResponseWriter, r *http.Request) {
	cachePublic(w, 300)
	writeJSON(w, http.StatusOK, a.Store.Stats(r.Context()))
}

func (a *App) parseFilter(w http.ResponseWriter, q url.Values) (filter.Filter, bool) {
	f, err := filter.Parse(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_filter", err.Error())
		return f, false
	}
	return f, true
}

// completeness of a federated answer, which decides how long it may be cached.
type completeness byte

const (
	complete completeness = 'F'
	partial  completeness = 'Q' // a database is down: retry within 20 s
	pending  completeness = 'P' // more databases still being searched: the page polls
)

func stateOf(isPartial, isPending bool) completeness {
	switch {
	case isPending:
		return pending
	case isPartial:
		return partial
	}
	return complete
}

// cached serves a JSON body from the LRU or computes it once (singleflight).
// Incomplete answers are kept briefly (here and at the CDN) so the next poll
// sees the background search's results.
func (a *App) cached(w http.ResponseWriter, key string, sMaxAge int, compute func() (any, completeness, error)) {
	serve := func(b []byte, hit string) {
		state, body := completeness(b[0]), b[1:]
		switch state {
		case pending:
			w.Header().Set("Cache-Control", "public, max-age=0, s-maxage=1")
		case partial:
			cachePublic(w, min(sMaxAge, 20))
		default:
			cachePublic(w, sMaxAge)
		}
		w.Header().Set("X-Cache", hit)
		writeRaw(w, http.StatusOK, body)
	}
	if b, ok := a.Store.Cache.Get(key); ok && len(b) > 0 {
		serve(b, "HIT")
		return
	}
	b, err := a.Store.SF.Do(key, func() ([]byte, error) {
		v, state, err := compute()
		if err != nil {
			return nil, err
		}
		js, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		b := append([]byte{byte(state)}, js...)
		ttl := 5 * time.Minute
		switch state {
		case pending:
			ttl = 1500 * time.Millisecond
		case partial:
			ttl = 20 * time.Second
		}
		a.Store.Cache.Set(key, b, ttl)
		return b, nil
	})
	if err != nil {
		a.queryError(w, err)
		return
	}
	serve(b, "MISS")
}

func (a *App) queryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, search.ErrBusy):
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusServiceUnavailable, "busy", "We're handling a lot of searches right now. Retrying shortly…")
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusServiceUnavailable, "slow_query", "That combination took too long. Add a country or industry to narrow it down.")
	case errors.Is(err, context.Canceled):
		writeErr(w, 499, "cancelled", "Request cancelled.")
	default:
		a.Log.Error("query failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side. Please retry.")
	}
}

func (a *App) apiFacets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dim := q.Get("dim")
	if _, ok := filter.DimByKey(dim); !ok {
		writeErr(w, http.StatusBadRequest, "bad_dim", "Unknown filter.")
		return
	}
	vq := normalize.Truncate(strings.TrimSpace(q.Get("q")), 60)
	f, ok := a.parseFilter(w, q)
	if !ok {
		return
	}
	key := fmt.Sprintf("facets|%d|%s|%s|%s", a.Store.Version(), dim, normalize.Fold(vq), f.Canonical())
	// shared computation must outlive the first caller (singleflight): detach cancellation
	bg := context.WithoutCancel(r.Context())
	a.cached(w, key, 60, func() (any, completeness, error) {
		res, err := a.Store.Facets(bg, f, dim, vq)
		return res, stateOf(res.Partial, res.Pending), err
	})
}

// apiPreview returns one page of masked rows. Anyone may browse the first
// Limits.PreviewPages pages (10 rows each); the page asks for a sponsor break
// before each page after the first. Real details always need an unlock.
func (a *App) apiPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, ok := a.parseFilter(w, q)
	if !ok {
		return
	}
	pages := a.Settings.Get().Limits.PreviewPages
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(0, min(page, pages-1))
	key := fmt.Sprintf("preview|%d|%d|%d|%s", a.Store.Version(), page, pages, f.Canonical())
	bg := context.WithoutCancel(r.Context())
	a.cached(w, key, 60, func() (any, completeness, error) {
		pv, err := a.Store.Preview(bg, f, page, search.PreviewLimit, pages)
		return pv, stateOf(pv.Partial, pv.Pending), err
	})
}

// purgeData drops every cache after a data change (import, removal).
func (a *App) purgeData(ctx context.Context) {
	a.Store.Cache.Purge()
	a.pages.Purge()
	a.purgeCDN(ctx, "/", "/api/config", "/api/stats", "/sitemap.xml", "/llms.txt")
}
