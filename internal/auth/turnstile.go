package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrTurnstile = errors.New("bot check failed; please retry")

// Turnstile verifies Cloudflare Turnstile tokens. With no secret configured
// (dev only — config refuses prod) verification is skipped.
type Turnstile struct {
	Secret   string
	Hostname string // expected hostname, e.g. markemindai.com ("" = don't check)
	Endpoint string
	Client   *http.Client
	Log      *slog.Logger
}

func NewTurnstile(secret, hostname string, log *slog.Logger) *Turnstile {
	return &Turnstile{Secret: secret, Hostname: hostname, Log: log,
		Endpoint: "https://challenges.cloudflare.com/turnstile/v0/siteverify",
		Client:   &http.Client{Timeout: 5 * time.Second}}
}

func (t *Turnstile) Enabled() bool { return t != nil && t.Secret != "" }

// Verify fails closed on a negative answer and open on transport failure
// (an outage at Cloudflare must not stop sign-ups; OTP rate limits still apply).
func (t *Turnstile) Verify(ctx context.Context, token, ip, action string) error {
	if !t.Enabled() {
		return nil
	}
	if token == "" || len(token) > 2048 {
		return ErrTurnstile
	}
	form := url.Values{"secret": {t.Secret}, "response": {token}}
	if ip != "" {
		form.Set("remoteip", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.Client.Do(req)
	if err != nil {
		t.Log.Warn("turnstile unreachable, allowing", "err", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Log.Warn("turnstile 5xx, allowing", "status", resp.StatusCode)
		return nil
	}
	var r struct {
		Success  bool     `json:"success"`
		Hostname string   `json:"hostname"`
		Action   string   `json:"action"`
		Codes    []string `json:"error-codes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&r); err != nil {
		t.Log.Warn("turnstile bad response, allowing", "err", err)
		return nil
	}
	if !r.Success {
		return ErrTurnstile
	}
	if t.Hostname != "" && r.Hostname != "" && !strings.EqualFold(r.Hostname, t.Hostname) {
		return ErrTurnstile
	}
	if action != "" && r.Action != "" && r.Action != action {
		return ErrTurnstile
	}
	return nil
}
