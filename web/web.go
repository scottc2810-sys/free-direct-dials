// Package web embeds the UI: static assets (served under content-hashed
// names, cached forever) and html/template pages.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static templates
var files embed.FS

type asset struct {
	name, url, ctype, etag string
	body                   []byte
}

type Assets struct {
	byName map[string]*asset
	byFile map[string]*asset // hashed file name → asset
}

// LoadAssets fingerprints every static file: app.js → app.3f9a1c2b.js.
func LoadAssets(base string) (*Assets, error) {
	a := &Assets{byName: map[string]*asset{}, byFile: map[string]*asset{}}
	err := fs.WalkDir(files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])[:10]
		ext := path.Ext(name)
		hashed := strings.TrimSuffix(name, ext) + "." + h + ext
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			ct = "application/octet-stream"
		}
		if strings.HasPrefix(ct, "text/") || ext == ".js" || ext == ".svg" || ext == ".json" {
			if !strings.Contains(ct, "charset") {
				ct += "; charset=utf-8"
			}
		}
		as := &asset{name: name, url: base + "/assets/" + hashed, ctype: ct, etag: `"` + h + `"`, body: b}
		a.byName[name] = as
		a.byFile[hashed] = as
		return nil
	})
	return a, err
}

// URL returns the fingerprinted URL for a logical asset name.
func (a *Assets) URL(name string) string {
	if as, ok := a.byName[name]; ok {
		return as.url
	}
	return ""
}

// Serve writes a fingerprinted asset; unknown names 404.
func (a *Assets) Serve(w http.ResponseWriter, r *http.Request, file string) {
	as, ok := a.byFile[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", as.ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", as.etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(as.body))
}

// Raw returns an asset's bytes by logical name (e.g. for favicon redirects).
func (a *Assets) Raw(name string) ([]byte, string, bool) {
	as, ok := a.byName[name]
	if !ok {
		return nil, "", false
	}
	return as.body, as.ctype, true
}

type Templates struct {
	pages      map[string]*template.Template
	standalone map[string]bool // pages with their own document shell (admin)
}

// LoadTemplates parses every page with the shared layout. funcs extends the
// default function map (asset URLs, formatting).
func LoadTemplates(funcs template.FuncMap) (*Templates, error) {
	base := template.FuncMap{
		"json": func(v any) (template.JS, error) {
			b, err := json.Marshal(v)
			return template.JS(b), err
		},
		"num":    func(n int64) string { return Thousands(n) },
		"approx": approx,
		"year":   func() int { return time.Now().Year() },
		"inc":    func(i int) int { return i + 1 },
	}
	for k, v := range funcs {
		base[k] = v
	}
	t := &Templates{pages: map[string]*template.Template{}, standalone: map[string]bool{}}
	entries, err := fs.ReadDir(files, "templates")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if n == "layout.html" || !strings.HasSuffix(n, ".html") {
			continue
		}
		if strings.HasPrefix(n, "admin") {
			tpl, err := template.New(n).Funcs(base).ParseFS(files, "templates/"+n)
			if err != nil {
				return nil, fmt.Errorf("template %s: %w", n, err)
			}
			name := strings.TrimSuffix(n, ".html")
			t.pages[name], t.standalone[name] = tpl, true
			continue
		}
		tpl, err := template.New("layout.html").Funcs(base).ParseFS(files, "templates/layout.html", "templates/"+n)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", n, err)
		}
		t.pages[strings.TrimSuffix(n, ".html")] = tpl
	}
	return t, nil
}

func (t *Templates) Render(w io.Writer, page string, data any) error {
	tpl, ok := t.pages[page]
	if !ok {
		return fmt.Errorf("unknown page %q", page)
	}
	if t.standalone[page] {
		return tpl.ExecuteTemplate(w, page+".html", data)
	}
	return tpl.ExecuteTemplate(w, "layout.html", data)
}

// Thousands formats 1234567 as "1,234,567".
func Thousands(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + Thousands(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// approx rounds down for headline copy: 1284113 → "1.2 million", 48210 → "48,000".
func approx(n int64) string {
	switch {
	case n >= 1_000_000:
		v := float64(n/100_000) / 10
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d million", int64(v))
		}
		return fmt.Sprintf("%.1f million", v)
	case n >= 10_000:
		return Thousands(n / 1000 * 1000)
	default:
		return Thousands(n)
	}
}
