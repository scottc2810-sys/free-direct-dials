package emailcheck

import (
	"context"
	_ "embed"
	"strings"
	"sync"

	"markemind/dials/internal/db"
)

//go:embed disposable.txt
var builtinDisposable string

// Label fragments that mark a throwaway mailbox service. Matched against every
// domain label except the TLD (so "hotmail" is never caught by "tmail").
var disposablePatterns = []string{
	"tempmail", "temp-mail", "tmpmail", "trashmail", "trash-mail", "10minute", "20minute", "minutemail",
	"mailinator", "guerrilla", "yopmail", "throwaway", "fakeinbox", "fakemail", "fake-mail", "getnada",
	"sharklasers", "maildrop", "dispostable", "burnermail", "tempinbox", "spambox", "spamgourmet",
	"mailnesia", "emailondeck", "discardmail", "disposable", "wegwerf", "jetable", "mytemp", "tempemail",
}

var freeProviders = setOf(`gmail.com googlemail.com yahoo.com yahoo.co.in yahoo.co.uk yahoo.in ymail.com rocketmail.com
hotmail.com hotmail.co.uk hotmail.in outlook.com outlook.in live.com live.in msn.com aol.com icloud.com me.com mac.com
proton.me protonmail.com pm.me gmx.com gmx.de gmx.net mail.com yandex.com yandex.ru rediffmail.com zoho.com zohomail.in
qq.com 163.com 126.com sina.com naver.com daum.net web.de t-online.de orange.fr free.fr laposte.net libero.it
virgilio.it tutanota.com tuta.io fastmail.com hey.com`)

func setOf(s string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, f := range strings.Fields(s) {
		m[strings.ToLower(f)] = struct{}{}
	}
	return m
}

// Disposable holds the built-in list plus admin-managed entries (blocklist table).
type Disposable struct {
	mu      sync.RWMutex
	builtin map[string]struct{}
	admin   map[string]struct{}
}

func NewDisposable() *Disposable {
	d := &Disposable{builtin: map[string]struct{}{}, admin: map[string]struct{}{}}
	for _, line := range strings.Split(builtinDisposable, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "" && !strings.HasPrefix(line, "#") {
			d.builtin[line] = struct{}{}
		}
	}
	return d
}

// Reload replaces the admin-managed set from the database.
func (d *Disposable) Reload(ctx context.Context, q db.Querier) error {
	rows, err := q.QueryContext(ctx, `SELECT domain FROM blocklist`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]struct{}{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		m[s] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	d.admin = m
	d.mu.Unlock()
	return nil
}

// Size returns (built-in, admin) entry counts.
func (d *Disposable) Size() (int, int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.builtin), len(d.admin)
}

// IsDisposable checks the domain, every parent domain, and label patterns.
func (d *Disposable) IsDisposable(domain string) bool {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	labels := strings.Split(domain, ".")
	d.mu.RLock()
	defer d.mu.RUnlock()
	for i := 0; i < len(labels)-1; i++ {
		cand := strings.Join(labels[i:], ".")
		if _, ok := d.builtin[cand]; ok {
			return true
		}
		if _, ok := d.admin[cand]; ok {
			return true
		}
	}
	for _, l := range labels[:len(labels)-1] {
		for _, p := range disposablePatterns {
			if strings.Contains(l, p) {
				return true
			}
		}
	}
	return false
}

// IsFreeProvider reports consumer mailbox providers (used by the optional
// "business email only" rule).
func IsFreeProvider(domain string) bool {
	_, ok := freeProviders[strings.ToLower(domain)]
	return ok
}

// NormalizeDomain cleans an admin-entered blocklist domain ("@Foo.com " → "foo.com").
func NormalizeDomain(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "@")
	s = strings.TrimSuffix(s, ".")
	if _, ok, _ := Syntax("x@" + s); !ok {
		return "", false
	}
	return s, true
}
