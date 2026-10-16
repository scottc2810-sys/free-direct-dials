package mail

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	texttemplate "text/template"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

var ErrQuota = errors.New("daily email quota reached")

// Class decides which share of the daily quota a message may use.
type Class int

const (
	Priority Class = iota // OTP codes, alerts: may use 100 % of the cap
	Bulk                  // campaigns: stop at cap × (100 − reserve) %
)

type Route struct {
	P   Provider
	Cap int // messages per UTC day
}

// Footer carries the legal/compliance block (CAN-SPAM) from settings.
type Footer struct {
	LegalName string
	Address   string
	SiteURL   string
	DonateURL string
}

type Mailer struct {
	DB         *db.DB
	Clock      platform.Clock
	Log        *slog.Logger
	From       Address
	Routes     []Route
	ReservePct int
	Footer     func() Footer
}

func (m *Mailer) reserve(ctx context.Context, provider string, limit int) (bool, error) {
	if limit <= 0 {
		return false, nil
	}
	day := platform.Day(m.Clock.Now())
	ok := false
	err := m.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mail_quota(day, provider, sent) VALUES(?,?,0)`, day, provider); err != nil {
			return err
		}
		var n int
		err := tx.QueryRowContext(ctx, `UPDATE mail_quota SET sent=sent+1 WHERE day=? AND provider=? AND sent<? RETURNING sent`,
			day, provider, limit).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		ok = err == nil
		return err
	})
	return ok, err
}

func (m *Mailer) release(ctx context.Context, provider string) {
	m.DB.W.ExecContext(ctx, `UPDATE mail_quota SET sent=sent-1 WHERE day=? AND provider=? AND sent>0`,
		platform.Day(m.Clock.Now()), provider)
}

// Send tries each provider in order, respecting quota; it fails over on
// quota exhaustion and transient/provider errors, but not on permanent ones.
func (m *Mailer) Send(ctx context.Context, class Class, msg Message) (string, error) {
	var lastErr error = ErrQuota
	for _, r := range m.Routes {
		limit := r.Cap
		if class == Bulk {
			limit = r.Cap * (100 - m.ReservePct) / 100
		}
		ok, err := m.reserve(ctx, r.P.Name(), limit)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		err = r.P.Send(ctx, m.From, msg)
		if err == nil {
			return r.P.Name(), nil
		}
		m.release(ctx, r.P.Name())
		if IsPermanent(err) {
			return r.P.Name(), err
		}
		m.Log.Warn("mail provider failed; trying next", "provider", r.P.Name(), "err", err)
		lastErr = err
	}
	return "", lastErr
}

type Usage struct {
	Provider string `json:"provider"`
	Sent     int    `json:"sent"`
	Cap      int    `json:"cap"`
	BulkCap  int    `json:"bulk_cap"`
}

func (m *Mailer) Usage(ctx context.Context) []Usage {
	day := platform.Day(m.Clock.Now())
	var out []Usage
	for _, r := range m.Routes {
		u := Usage{Provider: r.P.Name(), Cap: r.Cap, BulkCap: r.Cap * (100 - m.ReservePct) / 100}
		m.DB.R.QueryRowContext(ctx, `SELECT sent FROM mail_quota WHERE day=? AND provider=?`, day, r.P.Name()).Scan(&u.Sent)
		out = append(out, u)
	}
	return out
}

// BulkRemaining is how many campaign emails can still go out today.
func (m *Mailer) BulkRemaining(ctx context.Context) int {
	n := 0
	for _, u := range m.Usage(ctx) {
		n += max(0, u.BulkCap-u.Sent)
	}
	return n
}

func (m *Mailer) footer() Footer {
	if m.Footer == nil {
		return Footer{}
	}
	return m.Footer()
}

// SendOTP implements auth.Sender.
func (m *Mailer) SendOTP(ctx context.Context, to, code, purpose string) error {
	data := map[string]any{"Code": code, "Purpose": purpose, "F": m.footer()}
	subject := "Your Free Direct Dials code: " + code
	if purpose == "remove" {
		subject = "Confirm your data removal request: " + code
	}
	html, text := render(otpHTML, otpText, data)
	_, err := m.Send(ctx, Priority, Message{To: to, Subject: subject, HTML: html, Text: text})
	return err
}

// Alert emails the operator (rate limiting happens in the jobs package).
func (m *Mailer) Alert(ctx context.Context, to, subject, body string) error {
	if to == "" {
		return nil
	}
	html, text := render(alertHTML, alertText, map[string]any{"Subject": subject, "Body": body})
	_, err := m.Send(ctx, Priority, Message{To: to, Subject: "[Free Direct Dials] " + subject, HTML: html, Text: text})
	return err
}

func render(h *template.Template, t *texttemplate.Template, data any) (string, string) {
	var hb, tb bytes.Buffer
	if err := h.Execute(&hb, data); err != nil {
		hb.WriteString("template error")
	}
	if err := t.Execute(&tb, data); err != nil {
		tb.WriteString("template error")
	}
	return hb.String(), tb.String()
}

const layoutHead = `<!doctype html><html><body style="margin:0;background:#f4f2ec;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1c1b17">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center" style="padding:24px 12px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border:1px solid #e3dfd3;border-radius:8px">
<tr><td style="padding:24px 28px 8px;font-size:13px;letter-spacing:.06em;text-transform:uppercase;color:#0e6b52;font-weight:700">Free Direct Dials</td></tr>`

const layoutFoot = `<tr><td style="padding:16px 28px 24px;border-top:1px solid #eeeae0;font-size:12px;line-height:1.6;color:#6b675c">
{{with .F}}{{if .DonateURL}}This directory is free and funded by readers. <a href="{{.DonateURL}}" style="color:#0e6b52">Chip in via UPI, PayPal or crypto</a>.<br>{{end}}
{{if .LegalName}}{{.LegalName}}{{end}}{{if .Address}} &middot; {{.Address}}{{end}}{{end}}
{{if .Unsub}}<br><a href="{{.Unsub}}" style="color:#6b675c">Unsubscribe</a> from new-data emails.{{end}}
</td></tr></table></td></tr></table></body></html>`

var otpHTML = template.Must(template.New("otp").Parse(layoutHead + `
<tr><td style="padding:8px 28px 20px;font-size:16px;line-height:1.5">
{{if eq .Purpose "remove"}}Use this code to confirm that you want your contact details removed from the directory:{{else}}Use this code to sign in and unlock contacts:{{end}}
<div style="margin:18px 0;font:700 32px/1 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;letter-spacing:.3em;color:#1c1b17">{{.Code}}</div>
It expires in 10 minutes. If you didn't ask for it, you can ignore this email.</td></tr>` + layoutFoot))

var otpText = texttemplate.Must(texttemplate.New("otp").Parse(`Your code: {{.Code}}

{{if eq .Purpose "remove"}}Enter it to confirm your data removal request.{{else}}Enter it to sign in to Free Direct Dials.{{end}}
It expires in 10 minutes. If you didn't ask for it, ignore this email.
`))

var alertHTML = template.Must(template.New("alert").Parse(layoutHead + `
<tr><td style="padding:8px 28px 24px;font-size:15px;line-height:1.5"><strong>{{.Subject}}</strong><pre style="white-space:pre-wrap;font-size:13px">{{.Body}}</pre></td></tr>` +
	`</table></td></tr></table></body></html>`))

var alertText = texttemplate.Must(texttemplate.New("alert").Parse("{{.Subject}}\n\n{{.Body}}\n"))

var campaignHTML = template.Must(template.New("campaign").Parse(layoutHead + `
<tr><td style="padding:8px 28px 8px;font-size:22px;line-height:1.3;font-weight:700">{{.Headline}}</td></tr>
<tr><td style="padding:4px 28px 20px;font-size:16px;line-height:1.55;color:#3a372f">{{.Intro}}</td></tr>
<tr><td style="padding:0 28px 28px"><a href="{{.CTA}}" style="display:inline-block;background:#0e6b52;color:#ffffff;text-decoration:none;font-weight:700;padding:12px 20px;border-radius:6px">Browse the new contacts</a></td></tr>` + layoutFoot))

var campaignText = texttemplate.Must(texttemplate.New("campaign").Parse(`{{.Headline}}

{{.Intro}}

Browse the new contacts: {{.CTA}}
{{with .F}}{{if .DonateURL}}
This directory is free and funded by readers: {{.DonateURL}}{{end}}
{{.LegalName}} {{.Address}}{{end}}
Unsubscribe: {{.Unsub}}
`))

// CampaignMessage renders one subscriber's copy of a campaign.
func (m *Mailer) CampaignMessage(to, subject, headline, intro, cta, unsubPage, unsubAPI string) Message {
	data := map[string]any{"Headline": headline, "Intro": intro, "CTA": cta, "F": m.footer(), "Unsub": unsubPage}
	html, text := render(campaignHTML, campaignText, data)
	return Message{To: to, Subject: subject, HTML: html, Text: text, Headers: map[string]string{
		"List-Unsubscribe":      "<" + unsubAPI + ">",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}}
}

// Thousands formats 1513 as "1,513".
func Thousands(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
