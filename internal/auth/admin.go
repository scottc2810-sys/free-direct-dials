package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

// argon2id parameters: 32 MiB keeps a login well inside the 0.75 GB box;
// hashing is serialised so a login flood cannot multiply memory use.
const (
	argonTime    = 3
	argonMemKiB  = 32 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

var hashMu sync.Mutex

func HashPassword(pw string) (string, error) {
	if len(pw) < 12 {
		return "", errors.New("password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	hashMu.Lock()
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemKiB, argonThreads, argonKeyLen)
	hashMu.Unlock()
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m > 256*1024 || t > 10 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	hashMu.Lock()
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	hashMu.Unlock()
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash equalises timing when the username does not exist.
var dummyHash, _ = HashPassword("timing-equaliser-not-a-real-password")

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() string {
	b := make([]byte, 20)
	io.ReadFull(rand.Reader, b)
	return b32.EncodeToString(b)
}

// TOTPCode implements RFC 6238 (SHA-1, 30 s, 6 digits).
func TOTPCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(strings.ReplaceAll(secret, " ", ""), "=")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000), nil
}

// VerifyTOTP accepts the current step ±1 and refuses any step <= lastStep (replay).
func VerifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / 30
	for _, s := range []int64{cur - 1, cur, cur + 1} {
		if s <= lastStep {
			continue
		}
		want, err := TOTPCode(secret, s)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// OTPAuthURL is what authenticator apps scan.
func OTPAuthURL(issuer, user, secret string) string {
	label := url.PathEscape(issuer + ":" + user)
	return "otpauth://totp/" + label + "?secret=" + secret + "&issuer=" + url.QueryEscape(issuer) + "&algorithm=SHA1&digits=6&period=30"
}

var (
	ErrBadLogin    = errors.New("invalid username, password or code")
	ErrAdminLocked = errors.New("account temporarily locked")
)

type Admins struct {
	DB       *db.DB
	Clock    platform.Clock
	Idle     time.Duration // 30 min
	Absolute time.Duration // 12 h
}

type AdminSession struct {
	AdminID  int64
	Username string
	CSRF     string
}

// Create adds an admin and returns the TOTP secret to enrol.
func (a *Admins) Create(ctx context.Context, username, password string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 {
		return "", errors.New("username too short")
	}
	h, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	secret := NewTOTPSecret()
	_, err = a.DB.W.ExecContext(ctx, `INSERT INTO admins(username, pw_hash, totp_secret, created) VALUES(?,?,?,?)
		ON CONFLICT(username) DO UPDATE SET pw_hash=excluded.pw_hash, totp_secret=excluded.totp_secret, totp_last_step=0, failed=0, locked_until=0`,
		username, h, secret, a.Clock.Now().Unix())
	return secret, err
}

// Login verifies password + TOTP with exponential lockout per account.
func (a *Admins) Login(ctx context.Context, username, password, code, ip string) (token string, s AdminSession, err error) {
	username = strings.ToLower(strings.TrimSpace(username))
	now := a.Clock.Now()
	var id, lastStep, lockedUntil int64
	var failed int
	var pwHash, secret string
	err = a.DB.R.QueryRowContext(ctx, `SELECT id, pw_hash, totp_secret, totp_last_step, failed, locked_until FROM admins WHERE username=?`,
		username).Scan(&id, &pwHash, &secret, &lastStep, &failed, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		VerifyPassword(dummyHash, password)
		return "", s, ErrBadLogin
	}
	if err != nil {
		return "", s, err
	}
	if now.Unix() < lockedUntil {
		return "", s, ErrAdminLocked
	}
	step, totpOK := VerifyTOTP(secret, code, now, lastStep)
	if !VerifyPassword(pwHash, password) || !totpOK {
		failed++
		var until int64
		if failed >= 5 {
			back := time.Duration(15<<min(failed-5, 6)) * time.Minute
			until = now.Add(min(back, 24*time.Hour)).Unix()
		}
		a.DB.W.ExecContext(ctx, `UPDATE admins SET failed=?, locked_until=? WHERE id=?`, failed, until, id)
		return "", s, ErrBadLogin
	}
	token = platform.Token(32)
	s = AdminSession{AdminID: id, Username: username, CSRF: platform.Token(24)}
	err = a.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE admins SET failed=0, locked_until=0, totp_last_step=? WHERE id=?`, step, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO admin_sessions(id, admin_id, created, exp, idle_exp, csrf, ip) VALUES(?,?,?,?,?,?,?)`,
			platform.SHA256Hex(token), id, now.Unix(), now.Add(a.Absolute).Unix(), now.Add(a.Idle).Unix(), s.CSRF, ip)
		return err
	})
	return token, s, err
}

// Session validates an admin token and slides the idle timeout.
func (a *Admins) Session(ctx context.Context, token string) (AdminSession, error) {
	var s AdminSession
	if len(token) < 20 || len(token) > 100 {
		return s, sql.ErrNoRows
	}
	id := platform.SHA256Hex(token)
	now := a.Clock.Now().Unix()
	var exp, idle int64
	err := a.DB.R.QueryRowContext(ctx, `SELECT s.admin_id, a.username, s.csrf, s.exp, s.idle_exp FROM admin_sessions s
		JOIN admins a ON a.id=s.admin_id WHERE s.id=?`, id).Scan(&s.AdminID, &s.Username, &s.CSRF, &exp, &idle)
	if err != nil {
		return s, err
	}
	if now > exp || now > idle {
		a.DB.W.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id=?`, id)
		return s, sql.ErrNoRows
	}
	if idle-now < int64(a.Idle.Seconds())-60 { // refresh at most once a minute
		a.DB.W.ExecContext(ctx, `UPDATE admin_sessions SET idle_exp=? WHERE id=?`, now+int64(a.Idle.Seconds()), id)
	}
	return s, nil
}

func (a *Admins) Logout(ctx context.Context, token string) {
	a.DB.W.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id=?`, platform.SHA256Hex(token))
}

// Count reports how many admins exist (used for first-run guidance).
func (a *Admins) Count(ctx context.Context) int {
	var n int
	a.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	return n
}
