// Package mail sends transactional (OTP, alerts) and campaign email through
// pluggable providers with failover, a daily quota that always reserves
// capacity for sign-in codes, and a persistent outbox for campaigns.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Address struct{ Name, Email string }

func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return mime.QEncoding.Encode("utf-8", a.Name) + " <" + a.Email + ">"
}

type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
	Headers map[string]string
}

type Provider interface {
	Name() string
	Send(ctx context.Context, from Address, m Message) error
}

// PermanentError: the recipient/message is bad; retrying or failing over is useless.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

func clean(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

var httpClient = &http.Client{Timeout: 15 * time.Second}

// classify maps an HTTP API response to our error model.
func classify(provider string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	err := fmt.Errorf("%s: HTTP %d: %s", provider, resp.StatusCode, strings.TrimSpace(string(body)))
	if resp.StatusCode == 400 || resp.StatusCode == 422 {
		return &PermanentError{err}
	}
	return err // 401/403 (config), 429 (rate), 5xx → try next provider / retry later
}

func postJSON(ctx context.Context, url string, headers map[string]string, payload any) (*http.Response, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return httpClient.Do(req)
}

// Brevo (ex-Sendinblue) transactional API.
type Brevo struct {
	Key      string
	Endpoint string
}

func (b *Brevo) Name() string { return "brevo" }

func (b *Brevo) Send(ctx context.Context, from Address, m Message) error {
	ep := b.Endpoint
	if ep == "" {
		ep = "https://api.brevo.com/v3/smtp/email"
	}
	payload := map[string]any{
		"sender":      map[string]string{"name": from.Name, "email": from.Email},
		"to":          []map[string]string{{"email": m.To}},
		"subject":     clean(m.Subject),
		"htmlContent": m.HTML,
		"textContent": m.Text,
	}
	if len(m.Headers) > 0 {
		payload["headers"] = m.Headers
	}
	resp, err := postJSON(ctx, ep, map[string]string{"api-key": b.Key}, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return classify("brevo", resp)
}

// Resend email API.
type Resend struct {
	Key      string
	Endpoint string
}

func (r *Resend) Name() string { return "resend" }

func (r *Resend) Send(ctx context.Context, from Address, m Message) error {
	ep := r.Endpoint
	if ep == "" {
		ep = "https://api.resend.com/emails"
	}
	payload := map[string]any{"from": from.String(), "to": []string{m.To}, "subject": clean(m.Subject), "html": m.HTML, "text": m.Text}
	if len(m.Headers) > 0 {
		payload["headers"] = m.Headers
	}
	resp, err := postJSON(ctx, ep, map[string]string{"Authorization": "Bearer " + r.Key}, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return classify("resend", resp)
}

// SMTP works with Amazon SES, Zoho, Postmark, Mailgun… (STARTTLS on 587, TLS on 465).
type SMTP struct {
	Host       string
	Port       int
	User, Pass string
}

func (s *SMTP) Name() string { return "smtp" }

func (s *SMTP) Send(ctx context.Context, from Address, m Message) error {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	if s.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(45 * time.Second))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Pass, s.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from.Email); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) && te.Code >= 500 {
			return &PermanentError{err} // 5xx: mailbox rejected
		}
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(BuildMIME(from, m, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// BuildMIME renders a multipart/alternative message.
func BuildMIME(from Address, m Message, now time.Time) []byte {
	var b bytes.Buffer
	rnd := make([]byte, 12)
	rand.Read(rnd)
	boundary := "b_" + hex.EncodeToString(rnd)
	domain := from.Email[strings.LastIndexByte(from.Email, '@')+1:]
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, clean(v)) }
	hdr("From", from.String())
	hdr("To", m.To)
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(rnd)+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	for k, v := range m.Headers {
		hdr(k, v)
	}
	hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	part := func(ct, body string) {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary, ct)
		qp := quotedprintable.NewWriter(&b)
		qp.Write([]byte(body))
		qp.Close()
		b.WriteString("\r\n")
	}
	part("text/plain", m.Text)
	part("text/html", m.HTML)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// LogProvider is for development: it logs mail instead of sending it and keeps
// the last messages for tests.
type LogProvider struct {
	Log  *slog.Logger
	mu   sync.Mutex
	last []Message
	Fail error // tests: force an error
}

func (l *LogProvider) Name() string { return "log" }

func (l *LogProvider) Send(_ context.Context, from Address, m Message) error {
	if l.Fail != nil {
		return l.Fail
	}
	l.mu.Lock()
	l.last = append(l.last, m)
	if len(l.last) > 200 {
		l.last = l.last[1:]
	}
	l.mu.Unlock()
	if l.Log != nil {
		l.Log.Warn("DEV MAIL (not sent)", "to", m.To, "subject", m.Subject)
	}
	return nil
}

func (l *LogProvider) Sent() []Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Message(nil), l.last...)
}
