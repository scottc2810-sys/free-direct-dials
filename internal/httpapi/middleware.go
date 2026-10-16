package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"markemind/dials/internal/platform"
)

type ctxKey int

const (
	keyIP ctxKey = iota
	keyReqID
)

func clientIP(r *http.Request) string {
	if v, ok := r.Context().Value(keyIP).(string); ok {
		return v
	}
	return ""
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// middleware: recover → request id → real IP → base headers → API rate limit → log.
func (a *App) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		id := r.Header.Get("X-Request-ID")
		if len(id) == 0 || len(id) > 64 {
			id = platform.Token(9)
		}
		ip := a.realIP(r)
		ctx := context.WithValue(context.WithValue(r.Context(), keyIP, ip), keyReqID, id)
		r = r.WithContext(ctx)
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				a.Log.Error("panic", "err", p, "path", r.URL.Path, "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeErr(sw, http.StatusInternalServerError, "server_error", "Something went wrong on our side. Please retry.")
				}
				a.Alerter.Alert(context.WithoutCancel(ctx), "panic", "Handler panic", fmt.Sprintf("%v at %s", p, r.URL.Path))
			}
			d := time.Since(start)
			if sw.status >= 500 {
				a.Log.Warn("request", "status", sw.status, "method", r.Method, "path", r.URL.Path, "ms", d.Milliseconds(), "id", id)
			} else if d > time.Second {
				a.Log.Info("slow request", "status", sw.status, "path", r.URL.Path, "ms", d.Milliseconds(), "id", id)
			}
		}()
		hd := sw.Header()
		hd.Set("X-Request-ID", id)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
		if strings.HasPrefix(r.URL.Path, a.Cfg.BasePath+"/api/") {
			if ok, wait := a.limits.api.Take(ip); !ok {
				hd.Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeErr(sw, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please slow down for a moment.")
				return
			}
		}
		next.ServeHTTP(sw, r)
	})
}

// realIP trusts the proxy header only when the TCP peer is loopback (Caddy).
func (a *App) realIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err == nil && peer.IsLoopback() {
		if v := strings.TrimSpace(r.Header.Get(a.Cfg.TrustHeader)); v != "" {
			if ip, err := netip.ParseAddr(v); err == nil {
				return ip.String()
			}
		}
	}
	return host
}

// ---- responses ----

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Extra   any    `json:"extra,omitempty"`
}

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

func cachePublic(w http.ResponseWriter, sMaxAge int) {
	w.Header().Set("Cache-Control", "public, max-age=0, s-maxage="+strconv.Itoa(sMaxAge)+", stale-while-revalidate=30")
	w.Header().Add("Vary", "Accept-Encoding")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		noStore(w)
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	enc.Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

func writeErrExtra(w http.ResponseWriter, status int, code, msg string, extra any) {
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, apiError{Error: code, Message: msg, Extra: extra})
}

// decodeJSON enforces content type, size and strictness for request bodies.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, max int64) bool {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "bad_request", "Expected a JSON body.")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "Request body is too large.")
		} else {
			writeErr(w, http.StatusBadRequest, "bad_request", "Malformed request.")
		}
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeErr(w, http.StatusBadRequest, "bad_request", "Malformed request.")
		return false
	}
	return true
}

// sameOrigin blocks cross-site state changes: browsers always send Origin on
// POST; our client also sends X-FDD, which a cross-site form cannot set.
func (a *App) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-FDD") != "1" {
		writeErr(w, http.StatusForbidden, "forbidden", "Request blocked.")
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && o != a.Cfg.PublicOrigin && !(!a.Cfg.Prod() && isLocalOrigin(o)) {
		writeErr(w, http.StatusForbidden, "forbidden", "Request blocked (origin).")
		return false
	}
	return true
}

func isLocalOrigin(o string) bool {
	return strings.HasPrefix(o, "http://127.0.0.1") || strings.HasPrefix(o, "http://localhost") || strings.HasPrefix(o, "http://[::1]")
}

// ---- cookies ----

const (
	sessionCookie = "fdd_s"
	hintCookie    = "fdd_in"
)

func (a *App) cookiePath() string {
	if a.Cfg.BasePath == "" {
		return "/"
	}
	return a.Cfg.BasePath + "/"
}

func (a *App) setSession(w http.ResponseWriter, tok string, exp time.Time) {
	secure := strings.HasPrefix(a.Cfg.PublicOrigin, "https://")
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: a.cookiePath(), Expires: exp,
		MaxAge: int(time.Until(exp).Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	// readable hint (no secret): lets the page skip the uncached /api/me call for anonymous visitors
	http.SetCookie(w, &http.Cookie{Name: hintCookie, Value: "1", Path: a.cookiePath(), Expires: exp,
		MaxAge: int(time.Until(exp).Seconds()), Secure: secure, SameSite: http.SameSiteLaxMode})
}

func (a *App) clearSession(w http.ResponseWriter) {
	secure := strings.HasPrefix(a.Cfg.PublicOrigin, "https://")
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: a.cookiePath(), MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: hintCookie, Value: "", Path: a.cookiePath(), MaxAge: -1, Secure: secure, SameSite: http.SameSiteLaxMode})
}

// user resolves the session cookie; it writes 401 and returns ok=false when absent.
func (a *App) user(w http.ResponseWriter, r *http.Request) (int64, bool) {
	noStore(w)
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "Please sign in.")
		return 0, false
	}
	uid, exp, refreshed, err := a.Sessions.Lookup(r.Context(), c.Value)
	if err != nil {
		a.clearSession(w)
		writeErr(w, http.StatusUnauthorized, "unauthorized", "Your session expired. Please sign in again.")
		return 0, false
	}
	if refreshed {
		a.setSession(w, c.Value, exp)
	}
	return uid, true
}
