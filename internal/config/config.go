// Package config loads process configuration from the environment.
// Secrets live only here (from /etc/dials/dials.env), never in the database.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type MailProvider struct {
	Name string // brevo | resend | smtp | log
	Cap  int    // daily send cap for this provider
}

type Config struct {
	Env          string // dev | prod
	ListenAddr   string
	BasePath     string // "/free-direct-dials" or "" for subdomain mode
	PublicOrigin string // "https://markemindai.com"
	DataDir      string
	BackupDir    string
	TmpDir       string
	LogLevel     string
	SecretKey    []byte

	AdminPath   string
	AdminCIDRs  []netip.Prefix
	AdminEmail  string
	TrustHeader string // header carrying the client IP set by Caddy

	TurnstileSiteKey string
	TurnstileSecret  string

	MailProviders    []MailProvider
	MailFrom         string
	MailFromName     string
	MailReservePct   int
	MailWebhookToken string
	BrevoKey         string
	ResendKey        string
	SMTPHost         string
	SMTPPort         int
	SMTPUser         string
	SMTPPass         string

	ReacherURL   string
	ReacherToken string

	CFAPIToken string
	CFZoneID   string

	BackupKey string // hex/base64 32-byte key; backups are AES-256-GCM encrypted when set
}

func (c *Config) Prod() bool { return c.Env == "prod" }

// DBPath is the main database file.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "dials.db") }

// URL builds an absolute public URL for a path under the base path.
func (c *Config) URL(p string) string { return c.PublicOrigin + c.BasePath + p }

var adminPathRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// Load reads configuration from the environment (and optionally a dotenv file).
func Load(getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := &Config{
		Env:              get("APP_ENV", "dev"),
		ListenAddr:       get("LISTEN_ADDR", "127.0.0.1:8081"),
		BasePath:         strings.TrimRight(get("BASE_PATH", "/free-direct-dials"), "/"),
		PublicOrigin:     strings.TrimRight(get("PUBLIC_ORIGIN", "http://127.0.0.1:8081"), "/"),
		DataDir:          get("DATA_DIR", "./data"),
		LogLevel:         get("LOG_LEVEL", "warn"),
		AdminPath:        get("ADMIN_PATH", ""),
		AdminEmail:       get("ADMIN_EMAIL", ""),
		TrustHeader:      get("TRUST_PROXY_HEADER", "X-Real-IP"),
		TurnstileSiteKey: get("TURNSTILE_SITE_KEY", ""),
		TurnstileSecret:  get("TURNSTILE_SECRET", ""),
		MailFrom:         get("MAIL_FROM", "no-reply@markemindai.com"),
		MailFromName:     get("MAIL_FROM_NAME", "Free Direct Dials"),
		MailWebhookToken: get("MAIL_WEBHOOK_TOKEN", ""),
		BrevoKey:         get("BREVO_API_KEY", ""),
		ResendKey:        get("RESEND_API_KEY", ""),
		SMTPHost:         get("SMTP_HOST", ""),
		SMTPUser:         get("SMTP_USER", ""),
		SMTPPass:         get("SMTP_PASS", ""),
		ReacherURL:       strings.TrimRight(get("REACHER_URL", ""), "/"),
		ReacherToken:     get("REACHER_TOKEN", ""),
		CFAPIToken:       get("CF_API_TOKEN", ""),
		CFZoneID:         get("CF_ZONE_ID", ""),
		BackupKey:        get("BACKUP_KEY", ""),
	}
	c.BackupDir = get("BACKUP_DIR", filepath.Join(c.DataDir, "backups"))
	c.TmpDir = get("TMP_DIR", filepath.Join(c.DataDir, "tmp"))

	var errs []error
	var err error
	if c.SMTPPort, err = strconv.Atoi(get("SMTP_PORT", "587")); err != nil {
		errs = append(errs, fmt.Errorf("SMTP_PORT: %w", err))
	}
	if c.MailReservePct, err = strconv.Atoi(get("MAIL_OTP_RESERVE_PCT", "30")); err != nil || c.MailReservePct < 0 || c.MailReservePct > 90 {
		errs = append(errs, errors.New("MAIL_OTP_RESERVE_PCT must be 0–90"))
	}
	c.MailProviders, err = parseProviders(get("MAIL_PROVIDERS", "log:1000"))
	if err != nil {
		errs = append(errs, err)
	}
	for _, s := range splitList(get("ADMIN_ALLOW_CIDRS", "")) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, err2 := netip.ParseAddr(s)
			if err2 != nil {
				errs = append(errs, fmt.Errorf("ADMIN_ALLOW_CIDRS: bad entry %q", s))
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.AdminCIDRs = append(c.AdminCIDRs, p)
	}

	if len(c.AdminCIDRs) == 0 {
		if c.Prod() {
			errs = append(errs, errors.New("ADMIN_ALLOW_CIDRS is required in prod: list the IPs/CIDRs allowed to open the superadmin panel (e.g. 203.0.113.7,198.51.100.0/24)"))
		} else {
			// dev: the panel is still IP-restricted, to this machine only
			c.AdminCIDRs = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}
		}
	}
	for _, p := range c.AdminCIDRs {
		if p.Bits() == 0 {
			errs = append(errs, fmt.Errorf("ADMIN_ALLOW_CIDRS: %s would allow every address", p))
		}
	}

	secret := get("SECRET_KEY", "")
	if secret == "" && !c.Prod() {
		secret = "dev-only-insecure-secret-key-change-me-0123456789"
	}
	c.SecretKey = []byte(secret)

	if c.BasePath != "" && !strings.HasPrefix(c.BasePath, "/") {
		errs = append(errs, errors.New("BASE_PATH must start with /"))
	}
	if u, err := url.Parse(c.PublicOrigin); err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
		errs = append(errs, errors.New("PUBLIC_ORIGIN must be scheme://host with no path"))
	}
	if c.AdminPath == "" && !c.Prod() {
		c.AdminPath = "dev-admin-0123456789"
	}
	if !adminPathRe.MatchString(c.AdminPath) {
		errs = append(errs, errors.New("ADMIN_PATH must be 16–64 chars of [A-Za-z0-9_-]"))
	}

	if c.Prod() {
		if len(c.SecretKey) < 32 {
			errs = append(errs, errors.New("SECRET_KEY must be at least 32 bytes in prod"))
		}
		if !strings.HasPrefix(c.PublicOrigin, "https://") {
			errs = append(errs, errors.New("PUBLIC_ORIGIN must be https in prod"))
		}
		if c.TurnstileSecret == "" || c.TurnstileSiteKey == "" {
			errs = append(errs, errors.New("TURNSTILE_SITE_KEY and TURNSTILE_SECRET are required in prod"))
		}
		for _, p := range c.MailProviders {
			if p.Name == "log" {
				errs = append(errs, errors.New("MAIL_PROVIDERS may not use 'log' in prod"))
			}
		}
		if c.AdminEmail == "" {
			errs = append(errs, errors.New("ADMIN_EMAIL (alerts) is required in prod"))
		}
	}
	for _, p := range c.MailProviders {
		switch p.Name {
		case "brevo":
			if c.BrevoKey == "" {
				errs = append(errs, errors.New("BREVO_API_KEY missing for provider brevo"))
			}
		case "resend":
			if c.ResendKey == "" {
				errs = append(errs, errors.New("RESEND_API_KEY missing for provider resend"))
			}
		case "smtp":
			if c.SMTPHost == "" {
				errs = append(errs, errors.New("SMTP_HOST missing for provider smtp"))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func parseProviders(s string) ([]MailProvider, error) {
	var out []MailProvider
	for _, item := range splitList(s) {
		name, capStr, _ := strings.Cut(item, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		switch name {
		case "brevo", "resend", "smtp", "log":
		default:
			return nil, fmt.Errorf("MAIL_PROVIDERS: unknown provider %q", name)
		}
		capN := 300
		if capStr != "" {
			n, err := strconv.Atoi(strings.TrimSpace(capStr))
			if err != nil || n < 1 {
				return nil, fmt.Errorf("MAIL_PROVIDERS: bad cap for %s", name)
			}
			capN = n
		}
		out = append(out, MailProvider{Name: name, Cap: capN})
	}
	if len(out) == 0 {
		return nil, errors.New("MAIL_PROVIDERS is empty")
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadDotenv sets variables from a simple KEY=VALUE file without overriding
// variables already present in the environment. Missing file is not an error.
func LoadDotenv(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return nil
}
