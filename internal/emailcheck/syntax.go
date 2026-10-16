// Package emailcheck decides whether an email address may sign up: syntax,
// disposable-domain blocklist, optional business-email rule, DNS (MX / null MX)
// and an optional external Reacher verifier. Network failures fail open.
package emailcheck

import (
	"strings"

	"golang.org/x/net/idna"
)

var idnaProfile = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.StrictDomainName(true))

const localAllowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+/=?^_`{|}~.-"

// Syntax validates and normalises an address. It accepts the practical subset
// of RFC 5321/5322 that real mailboxes use (no quoted local parts, no IP
// literals) and converts IDN domains to punycode. Returns the lower-cased
// address and a machine reason on failure.
func Syntax(raw string) (string, bool, string) {
	e := strings.TrimSpace(raw)
	if e == "" {
		return "", false, "empty"
	}
	if len(e) > 254 {
		return "", false, "too_long"
	}
	for _, r := range e {
		if r <= ' ' || r == 0x7f {
			return "", false, "invalid_chars"
		}
	}
	at := strings.LastIndexByte(e, '@')
	if at <= 0 || at == len(e)-1 || strings.Count(e, "@") != 1 {
		return "", false, "missing_at"
	}
	local, domain := e[:at], e[at+1:]
	if len(local) > 64 {
		return "", false, "local_too_long"
	}
	if local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return "", false, "bad_local_dots"
	}
	for i := 0; i < len(local); i++ {
		if !strings.ContainsRune(localAllowed, rune(local[i])) {
			return "", false, "bad_local_chars"
		}
	}
	if strings.HasPrefix(domain, "[") {
		return "", false, "ip_literal"
	}
	ascii, err := idnaProfile.ToASCII(strings.TrimSuffix(domain, "."))
	if err != nil {
		return "", false, "bad_domain"
	}
	ascii = strings.ToLower(ascii)
	if len(ascii) > 253 {
		return "", false, "bad_domain"
	}
	labels := strings.Split(ascii, ".")
	if len(labels) < 2 {
		return "", false, "bad_domain"
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", false, "bad_domain"
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", false, "bad_domain"
			}
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 || (!strings.HasPrefix(tld, "xn--") && strings.IndexFunc(tld, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0) {
		return "", false, "bad_tld"
	}
	return strings.ToLower(local) + "@" + ascii, true, ""
}

// Domain returns the part after '@' (already normalised input expected).
func Domain(email string) string {
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		return email[i+1:]
	}
	return ""
}
