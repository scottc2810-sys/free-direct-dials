package emailcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

// Verdict is the outcome of a sign-up check. Reason is a stable machine code
// that the UI maps to a human message.
type Verdict struct {
	OK     bool
	Reason string
}

// Resolver is the subset of *net.Resolver we use (fakeable in tests).
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

type Checker struct {
	DB         *db.DB
	Disposable *Disposable
	Resolver   Resolver
	Reacher    *Reacher // nil when not configured
	Clock      platform.Clock
	Log        *slog.Logger

	RequireBusiness func() bool // settings-driven
	UseReacher      func() bool // settings-driven
}

const (
	verdictTTL = 30 * 24 * time.Hour
	mxTTLok    = 30 * 24 * time.Hour
	mxTTLbad   = 24 * time.Hour
	dnsTimeout = 3 * time.Second
)

// Check runs every rule in order and returns the normalised address.
func (c *Checker) Check(ctx context.Context, raw string) (string, Verdict) {
	email, ok, why := Syntax(raw)
	if !ok {
		return "", Verdict{false, "invalid_syntax:" + why}
	}
	domain := Domain(email)
	if c.Disposable.IsDisposable(domain) {
		return email, Verdict{false, "disposable"}
	}
	if c.RequireBusiness != nil && c.RequireBusiness() && IsFreeProvider(domain) {
		return email, Verdict{false, "free_provider"}
	}
	now := c.Clock.Now()

	var okInt int
	var reason string
	var ts int64
	err := c.DB.R.QueryRowContext(ctx, `SELECT ok, reason, ts FROM verdicts WHERE email=?`, email).Scan(&okInt, &reason, &ts)
	if err == nil && now.Sub(time.Unix(ts, 0)) < verdictTTL {
		return email, Verdict{okInt == 1, reason}
	}

	v, cacheable := c.checkMX(ctx, domain, now)
	if !v.OK {
		return email, v
	}
	if c.Reacher != nil && (c.UseReacher == nil || c.UseReacher()) {
		rv, rerr := c.Reacher.Check(ctx, email)
		if rerr != nil {
			c.Log.Warn("reacher unavailable, allowing", "err", rerr)
			cacheable = false
		} else {
			v = rv
		}
	}
	if cacheable {
		c.DB.W.ExecContext(ctx, `INSERT INTO verdicts(email, ok, reason, ts) VALUES(?,?,?,?)
			ON CONFLICT(email) DO UPDATE SET ok=excluded.ok, reason=excluded.reason, ts=excluded.ts`,
			email, b2i(v.OK), v.Reason, now.Unix())
	}
	return email, v
}

// checkMX returns the domain verdict and whether it is safe to cache.
func (c *Checker) checkMX(ctx context.Context, domain string, now time.Time) (Verdict, bool) {
	var okInt int
	var reason string
	var ts int64
	if err := c.DB.R.QueryRowContext(ctx, `SELECT ok, reason, ts FROM mx_cache WHERE domain=?`, domain).Scan(&okInt, &reason, &ts); err == nil {
		ttl := mxTTLok
		if okInt == 0 {
			ttl = mxTTLbad
		}
		if now.Sub(time.Unix(ts, 0)) < ttl {
			return Verdict{okInt == 1, reason}, true
		}
	}
	if c.Resolver == nil {
		return Verdict{true, "no_resolver"}, false
	}
	lctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	v, definitive := c.lookup(lctx, domain)
	if !definitive {
		c.Log.Warn("dns lookup inconclusive, allowing", "domain", domain, "reason", v.Reason)
		return v, false
	}
	c.DB.W.ExecContext(ctx, `INSERT INTO mx_cache(domain, ok, reason, ts) VALUES(?,?,?,?)
		ON CONFLICT(domain) DO UPDATE SET ok=excluded.ok, reason=excluded.reason, ts=excluded.ts`,
		domain, b2i(v.OK), v.Reason, now.Unix())
	return v, true
}

func (c *Checker) lookup(ctx context.Context, domain string) (Verdict, bool) {
	mxs, err := c.Resolver.LookupMX(ctx, domain)
	if err == nil && len(mxs) > 0 {
		for _, mx := range mxs {
			if h := strings.TrimSuffix(mx.Host, "."); h != "" {
				return Verdict{true, "mx"}, true
			}
		}
		return Verdict{false, "null_mx"}, true // RFC 7505: domain accepts no mail
	}
	if err != nil && !isNotFound(err) {
		return Verdict{true, "dns_unavailable"}, false
	}
	// No MX: RFC 5321 implicit MX via A/AAAA.
	addrs, herr := c.Resolver.LookupHost(ctx, domain)
	if herr == nil && len(addrs) > 0 {
		return Verdict{true, "implicit_mx"}, true
	}
	if herr != nil && !isNotFound(herr) {
		return Verdict{true, "dns_unavailable"}, false
	}
	return Verdict{false, "no_mx"}, true
}

func isNotFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Reacher calls a self-hosted check-if-email-exists (Reacher) instance.
type Reacher struct {
	URL    string
	Secret string
	Client *http.Client
	sem    chan struct{}
}

func NewReacher(url, secret string) *Reacher {
	if url == "" {
		return nil
	}
	return &Reacher{URL: url, Secret: secret, Client: &http.Client{Timeout: 20 * time.Second}, sem: make(chan struct{}, 3)}
}

type reacherResp struct {
	IsReachable string `json:"is_reachable"`
	Misc        struct {
		IsDisposable bool `json:"is_disposable"`
	} `json:"misc"`
	MX struct {
		AcceptsMail bool `json:"accepts_mail"`
	} `json:"mx"`
	Syntax struct {
		IsValidSyntax bool `json:"is_valid_syntax"`
	} `json:"syntax"`
}

// Check blocks disposable / invalid syntax / no MX / invalid; allows safe,
// risky and unknown. Any transport error is returned (callers fail open).
func (r *Reacher) Check(ctx context.Context, email string) (Verdict, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return Verdict{}, ctx.Err()
	case <-time.After(5 * time.Second):
		return Verdict{}, errors.New("reacher busy")
	}
	body, _ := json.Marshal(map[string]string{"to_email": email})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/v0/check_email", bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Secret != "" {
		req.Header.Set("x-reacher-secret", r.Secret)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return Verdict{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("reacher status %d", resp.StatusCode)
	}
	var rr reacherResp
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&rr); err != nil {
		return Verdict{}, err
	}
	switch {
	case rr.Misc.IsDisposable:
		return Verdict{false, "disposable"}, nil
	case !rr.Syntax.IsValidSyntax:
		return Verdict{false, "invalid_syntax:reacher"}, nil
	case !rr.MX.AcceptsMail:
		return Verdict{false, "no_mx"}, nil
	case rr.IsReachable == "invalid":
		return Verdict{false, "undeliverable"}, nil
	}
	return Verdict{true, "reacher_" + rr.IsReachable}, nil
}
