// Package auth implements visitor sign-in (email OTP + sessions), superadmin
// authentication (argon2id + TOTP), Cloudflare Turnstile and rate limiting.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

// Sender delivers one-time codes (implemented by the mail package).
type Sender interface {
	SendOTP(ctx context.Context, to, code, purpose string) error
}

var (
	ErrTooManySends = errors.New("too many codes requested; try again later")
	ErrNoCode       = errors.New("no active code; request a new one")
	ErrExpired      = errors.New("code expired; request a new one")
	ErrLocked       = errors.New("too many wrong attempts; request a new code")
)

// InvalidCodeError carries the number of attempts left.
type InvalidCodeError struct{ Remaining int }

func (e *InvalidCodeError) Error() string {
	return fmt.Sprintf("wrong code (%d attempts left)", e.Remaining)
}

type OTP struct {
	DB     *db.DB
	Clock  platform.Clock
	Secret []byte
	Sender Sender

	TTL          time.Duration // 10 min
	MaxTries     int           // 5
	SendsPerHour int           // 3
}

func (o *OTP) defaults() {
	if o.TTL == 0 {
		o.TTL = 10 * time.Minute
	}
	if o.MaxTries == 0 {
		o.MaxTries = 5
	}
	if o.SendsPerHour == 0 {
		o.SendsPerHour = 3
	}
}

func (o *OTP) hash(purpose, email, code string) string {
	return platform.HMACHex(o.Secret, "otp", purpose, email, code)
}

// Start issues a fresh code (invalidating any previous one) and emails it.
func (o *OTP) Start(ctx context.Context, email, purpose string) error {
	o.defaults()
	now := o.Clock.Now().Unix()
	code := platform.Digits(6)
	err := o.DB.Tx(ctx, func(tx *sql.Tx) error {
		var sends int
		var window int64
		err := tx.QueryRowContext(ctx, `SELECT sends, window_start FROM otps WHERE email=? AND purpose=?`, email, purpose).Scan(&sends, &window)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if now-window >= 3600 {
			sends, window = 0, now
		}
		if sends >= o.SendsPerHour {
			return ErrTooManySends
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO otps(email, purpose, hash, exp, tries, sends, window_start) VALUES(?,?,?,?,0,?,?)
			ON CONFLICT(email, purpose) DO UPDATE SET hash=excluded.hash, exp=excluded.exp, tries=0, sends=excluded.sends, window_start=excluded.window_start`,
			email, purpose, o.hash(purpose, email, code), now+int64(o.TTL.Seconds()), sends+1, window)
		return err
	})
	if err != nil {
		return err
	}
	if err := o.Sender.SendOTP(ctx, email, code, purpose); err != nil {
		// a provider outage must not eat the visitor's hourly allowance
		o.DB.W.ExecContext(context.WithoutCancel(ctx), `UPDATE otps SET sends=MAX(sends-1,0), hash='' WHERE email=? AND purpose=?`, email, purpose)
		return err
	}
	return nil
}

// Verify checks a code. Five wrong attempts lock the code; the sixth attempt
// is refused even if correct.
func (o *OTP) Verify(ctx context.Context, email, purpose, code string) error {
	o.defaults()
	code = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, code)
	now := o.Clock.Now().Unix()
	var result error
	err := o.DB.Tx(ctx, func(tx *sql.Tx) error {
		var hash string
		var exp int64
		var tries int
		err := tx.QueryRowContext(ctx, `SELECT hash, exp, tries FROM otps WHERE email=? AND purpose=?`, email, purpose).Scan(&hash, &exp, &tries)
		if errors.Is(err, sql.ErrNoRows) || hash == "" {
			result = ErrNoCode
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case tries >= o.MaxTries:
			result = ErrLocked
			return nil
		case now > exp:
			result = ErrExpired
			return nil
		}
		if len(code) != 6 || !platform.ConstantTimeEqual(hash, o.hash(purpose, email, code)) {
			tries++
			if _, err := tx.ExecContext(ctx, `UPDATE otps SET tries=? WHERE email=? AND purpose=?`, tries, email, purpose); err != nil {
				return err
			}
			result = &InvalidCodeError{Remaining: o.MaxTries - tries}
			return nil
		}
		// success: burn the code but keep the send window (rate limit survives)
		_, err = tx.ExecContext(ctx, `UPDATE otps SET hash='', tries=0, exp=0 WHERE email=? AND purpose=?`, email, purpose)
		return err
	})
	if err != nil {
		return err
	}
	return result
}
