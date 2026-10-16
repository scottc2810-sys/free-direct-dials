package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

type User struct {
	ID         int64  `json:"id"`
	Email      string `json:"email"`
	Created    int64  `json:"created"`
	LastSeen   int64  `json:"last_seen"`
	Subscribed bool   `json:"subscribed"`
	Blocked    bool   `json:"blocked"`
	Bounced    bool   `json:"bounced"`
	UnsubToken string `json:"-"`
}

var ErrBlocked = errors.New("account blocked")

type Users struct {
	DB    *db.DB
	Clock platform.Clock
}

const userCols = `id, email, created, last_seen, subscribed, blocked, bounced, unsub_token`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var sub, blk, bnc int
	err := row.Scan(&u.ID, &u.Email, &u.Created, &u.LastSeen, &sub, &blk, &bnc, &u.UnsubToken)
	u.Subscribed, u.Blocked, u.Bounced = sub == 1, blk == 1, bnc == 1
	return u, err
}

// Upsert creates the user on first sign-in (subscription preference applies
// only then) and refreshes last_seen on later sign-ins.
func (u *Users) Upsert(ctx context.Context, email string, subscribe bool) (User, error) {
	now := u.Clock.Now().Unix()
	sub := 0
	if subscribe {
		sub = 1
	}
	row := u.DB.W.QueryRowContext(ctx, `INSERT INTO users(email, created, last_seen, subscribed, unsub_token) VALUES(?,?,?,?,?)
		ON CONFLICT(email) DO UPDATE SET last_seen=excluded.last_seen RETURNING `+userCols,
		email, now, now, sub, platform.Token(24))
	usr, err := scanUser(row)
	if err != nil {
		return usr, err
	}
	if usr.Blocked {
		return usr, ErrBlocked
	}
	return usr, nil
}

func (u *Users) Get(ctx context.Context, id int64) (User, error) {
	return scanUser(u.DB.R.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (u *Users) ByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(u.DB.R.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email=?`, email))
}

// Unsubscribe by token; returns the email for confirmation copy.
func (u *Users) Unsubscribe(ctx context.Context, token string) (string, error) {
	if len(token) < 16 {
		return "", sql.ErrNoRows
	}
	var email string
	err := u.DB.W.QueryRowContext(ctx, `UPDATE users SET subscribed=0 WHERE unsub_token=? RETURNING email`, token).Scan(&email)
	return email, err
}

// Delete erases a user and everything tied to them (GDPR erasure).
func (u *Users) Delete(ctx context.Context, id int64) error {
	return u.DB.Tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM sessions WHERE user_id=?`, `DELETE FROM ad_tokens WHERE user_id=?`, `DELETE FROM unlocked WHERE user_id=?`,
			`DELETE FROM user_daily WHERE user_id=?`, `DELETE FROM outbox WHERE user_id=?`, `DELETE FROM users WHERE id=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

type Sessions struct {
	DB    *db.DB
	Clock platform.Clock
	TTL   time.Duration // 30 days, sliding
}

const maxSessionsPerUser = 20

// Create issues a session token (only its SHA-256 is stored).
func (s *Sessions) Create(ctx context.Context, userID int64) (string, time.Time, error) {
	tok := platform.Token(32)
	now := s.Clock.Now()
	exp := now.Add(s.TTL)
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, user_id, created, exp, last_refresh) VALUES(?,?,?,?,?)`,
			platform.SHA256Hex(tok), userID, now.Unix(), exp.Unix(), now.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND id NOT IN
			(SELECT id FROM sessions WHERE user_id=? ORDER BY created DESC LIMIT ?)`, userID, userID, maxSessionsPerUser)
		return err
	})
	return tok, exp, err
}

// Lookup validates a token. When the session was last extended over a day
// ago it slides the expiry and returns refreshed=true (caller re-sets cookie).
func (s *Sessions) Lookup(ctx context.Context, tok string) (userID int64, exp time.Time, refreshed bool, err error) {
	if len(tok) < 20 || len(tok) > 100 {
		return 0, time.Time{}, false, sql.ErrNoRows
	}
	id := platform.SHA256Hex(tok)
	var e, last int64
	if err = s.DB.R.QueryRowContext(ctx, `SELECT user_id, exp, last_refresh FROM sessions WHERE id=?`, id).Scan(&userID, &e, &last); err != nil {
		return 0, time.Time{}, false, err
	}
	now := s.Clock.Now()
	if now.Unix() > e {
		return 0, time.Time{}, false, sql.ErrNoRows
	}
	exp = time.Unix(e, 0)
	if now.Unix()-last > 86400 {
		exp = now.Add(s.TTL)
		if _, err := s.DB.W.ExecContext(ctx, `UPDATE sessions SET exp=?, last_refresh=? WHERE id=?`, exp.Unix(), now.Unix(), id); err == nil {
			refreshed = true
		}
	}
	return userID, exp, refreshed, nil
}

func (s *Sessions) Delete(ctx context.Context, tok string) error {
	_, err := s.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, platform.SHA256Hex(tok))
	return err
}
