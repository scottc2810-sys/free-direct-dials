package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func prodEnv() map[string]string {
	return map[string]string{
		"APP_ENV": "prod", "PUBLIC_ORIGIN": "https://markemindai.com", "SECRET_KEY": strings.Repeat("k", 40),
		"ADMIN_PATH": "a-very-secret-admin-path-123", "ADMIN_ALLOW_CIDRS": "203.0.113.7, 198.51.100.0/24, 2001:db8::/32",
		"TURNSTILE_SITE_KEY": "x", "TURNSTILE_SECRET": "y", "MAIL_PROVIDERS": "brevo:300", "BREVO_API_KEY": "k", "ADMIN_EMAIL": "ops@markemindai.com",
	}
}

func TestProdValid(t *testing.T) {
	c, err := Load(env(prodEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AdminCIDRs) != 3 || c.AdminCIDRs[0].String() != "203.0.113.7/32" {
		t.Fatalf("cidrs %v", c.AdminCIDRs)
	}
}

func TestProdRequiresAdminAllowList(t *testing.T) {
	e := prodEnv()
	delete(e, "ADMIN_ALLOW_CIDRS")
	if _, err := Load(env(e)); err == nil || !strings.Contains(err.Error(), "ADMIN_ALLOW_CIDRS is required") {
		t.Fatalf("want allow-list error, got %v", err)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "not-an-ip"} {
		e["ADMIN_ALLOW_CIDRS"] = bad
		if _, err := Load(env(e)); err == nil {
			t.Errorf("ADMIN_ALLOW_CIDRS=%q accepted", bad)
		}
	}
}

func TestDevDefaultsToLocalhostOnly(t *testing.T) {
	c, err := Load(env(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AdminCIDRs) != 2 || c.AdminCIDRs[0].String() != "127.0.0.1/32" {
		t.Fatalf("dev admin allow-list = %v", c.AdminCIDRs)
	}
}

func TestProdRejectsUnsafe(t *testing.T) {
	for k, v := range map[string]string{"SECRET_KEY": "short", "PUBLIC_ORIGIN": "http://markemindai.com", "MAIL_PROVIDERS": "log", "ADMIN_PATH": "admin", "TURNSTILE_SECRET": ""} {
		e := prodEnv()
		e[k] = v
		if _, err := Load(env(e)); err == nil {
			t.Errorf("%s=%q accepted in prod", k, v)
		}
	}
}
