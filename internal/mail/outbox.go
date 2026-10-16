package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

var ErrAlreadyNotified = errors.New("subscribers were already notified for this upload")

// Outbox stores one row per recipient per campaign and drains it within the
// daily bulk quota, resuming the next day automatically.
type Outbox struct {
	DB     *db.DB
	Mailer *Mailer
	Clock  platform.Clock
	Log    *slog.Logger
	URL    func(path string) string // absolute public URL for a path under the base path
	Pace   time.Duration            // pause between sends (default 250 ms)

	paused atomic.Bool
	// waitUntil is a unix time before which ticks do nothing (quota exhausted).
	waitUntil atomic.Int64
}

func (o *Outbox) Pause(p bool) { o.paused.Store(p) }
func (o *Outbox) Paused() bool { return o.paused.Load() }

// Enqueue creates a campaign for an upload and queues every active subscriber.
// An upload can be notified only once.
func (o *Outbox) Enqueue(ctx context.Context, uploadID int64, subject, intro string) (int64, int64, error) {
	now := o.Clock.Now().Unix()
	var cid, total int64
	err := o.DB.Tx(ctx, func(tx *sql.Tx) error {
		var added int64
		var notified int
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT added, notified, status FROM uploads WHERE id=?`, uploadID).Scan(&added, &notified, &status); err != nil {
			return err
		}
		if notified != 0 {
			return ErrAlreadyNotified
		}
		if status != "done" {
			return errors.New("upload has not finished importing")
		}
		if added <= 0 {
			return errors.New("this upload added no new contacts")
		}
		if subject == "" {
			subject = Thousands(added) + " new direct dials just added"
		}
		if intro == "" {
			intro = "We just added " + Thousands(added) + " new B2B contacts with direct dials and verified emails. Filter by country, industry, seniority and more. It's free."
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO campaigns(upload_id, subject, intro, added, created) VALUES(?,?,?,?,?) RETURNING id`,
			uploadID, subject, intro, added, now).Scan(&cid); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO outbox(campaign_id, user_id, status, next_at)
			SELECT ?, id, 'queued', ? FROM users WHERE subscribed=1 AND blocked=0 AND bounced=0 ORDER BY last_seen DESC`, cid, now)
		if err != nil {
			return err
		}
		total, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `UPDATE campaigns SET total=? WHERE id=?`, total, cid); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE uploads SET notified=? WHERE id=?`, cid, uploadID)
		return err
	})
	if err == nil {
		o.waitUntil.Store(0)
	}
	return cid, total, err
}

type campaign struct{ subject, intro string }

// Tick sends up to batch queued emails. It returns the number sent.
func (o *Outbox) Tick(ctx context.Context, batch int) (int, error) {
	if o.Paused() || o.Clock.Now().Unix() < o.waitUntil.Load() {
		return 0, nil
	}
	if o.Pace == 0 {
		o.Pace = 250 * time.Millisecond
	}
	now := o.Clock.Now().Unix()
	rows, err := o.DB.R.QueryContext(ctx, `SELECT o.id, o.campaign_id, o.attempts, u.email, u.unsub_token, u.subscribed, u.blocked, u.bounced
		FROM outbox o JOIN users u ON u.id=o.user_id WHERE o.status='queued' AND o.next_at<=? ORDER BY o.id LIMIT ?`, now, batch)
	if err != nil {
		return 0, err
	}
	type item struct {
		id, cid           int64
		attempts          int
		email, unsub      string
		sub, blk, bounced int
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.cid, &it.attempts, &it.email, &it.unsub, &it.sub, &it.blk, &it.bounced); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	camps := map[int64]campaign{}
	sent := 0
	for _, it := range items {
		if ctx.Err() != nil {
			return sent, ctx.Err()
		}
		if it.sub == 0 || it.blk == 1 || it.bounced == 1 {
			o.DB.W.ExecContext(ctx, `UPDATE outbox SET status='failed', err='recipient no longer active' WHERE id=?`, it.id)
			continue
		}
		c, ok := camps[it.cid]
		if !ok {
			if err := o.DB.R.QueryRowContext(ctx, `SELECT subject, intro FROM campaigns WHERE id=?`, it.cid).Scan(&c.subject, &c.intro); err != nil {
				return sent, err
			}
			camps[it.cid] = c
		}
		cta := o.URL(fmt.Sprintf("/?utm_source=email&utm_medium=notify&utm_campaign=c%d", it.cid))
		msg := o.Mailer.CampaignMessage(it.email, c.subject, c.subject, c.intro, cta,
			o.URL("/unsubscribe?t="+it.unsub), o.URL("/api/unsub?t="+it.unsub))
		_, err := o.Mailer.Send(ctx, Bulk, msg)
		switch {
		case err == nil:
			sent++
			o.DB.W.ExecContext(ctx, `UPDATE outbox SET status='sent', sent_at=?, attempts=attempts+1, err='' WHERE id=?`, o.Clock.Now().Unix(), it.id)
		case errors.Is(err, ErrQuota):
			// resume at the next UTC midnight (+ jitter-free 60 s margin)
			t := o.Clock.Now().UTC()
			next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 1, 0, 0, time.UTC)
			o.waitUntil.Store(next.Unix())
			return sent, nil
		case IsPermanent(err):
			o.DB.W.ExecContext(ctx, `UPDATE outbox SET status='failed', attempts=attempts+1, err=? WHERE id=?`, trim(err.Error()), it.id)
		default:
			att := it.attempts + 1
			if att >= 5 {
				o.DB.W.ExecContext(ctx, `UPDATE outbox SET status='failed', attempts=?, err=? WHERE id=?`, att, trim(err.Error()), it.id)
			} else {
				backoff := time.Duration(1<<att) * 5 * time.Minute
				o.DB.W.ExecContext(ctx, `UPDATE outbox SET attempts=?, next_at=?, err=? WHERE id=?`, att,
					o.Clock.Now().Add(backoff).Unix(), trim(err.Error()), it.id)
			}
			o.Log.Warn("campaign send failed", "err", err)
		}
		if o.Pace > 0 {
			select {
			case <-ctx.Done():
				return sent, ctx.Err()
			case <-time.After(o.Pace):
			}
		}
	}
	return sent, nil
}

func trim(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

type Status struct {
	Queued     int64   `json:"queued"`
	Sent       int64   `json:"sent"`
	Failed     int64   `json:"failed"`
	Paused     bool    `json:"paused"`
	ResumeAt   int64   `json:"resume_at"`
	Usage      []Usage `json:"usage"`
	BulkLeft   int     `json:"bulk_left_today"`
	DaysToDone float64 `json:"days_to_done"`
}

func (o *Outbox) Status(ctx context.Context) Status {
	var s Status
	o.DB.R.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='queued'),0), COALESCE(SUM(status='sent'),0), COALESCE(SUM(status='failed'),0) FROM outbox`).
		Scan(&s.Queued, &s.Sent, &s.Failed)
	s.Paused = o.Paused()
	s.ResumeAt = o.waitUntil.Load()
	s.Usage = o.Mailer.Usage(ctx)
	s.BulkLeft = o.Mailer.BulkRemaining(ctx)
	bulkPerDay := 0
	for _, u := range s.Usage {
		bulkPerDay += u.BulkCap
	}
	if bulkPerDay > 0 {
		s.DaysToDone = float64(s.Queued) / float64(bulkPerDay)
	}
	return s
}
