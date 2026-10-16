// Package unlock implements the monetisation core: ad tokens, claiming the
// next N never-before-unlocked contacts, the personal view and CSV export.
//
// Invariants (acceptance #6, #7, #9):
//   - a contact is handed to a user at most once (PRIMARY KEY + single writer + per-user lock)
//   - a token is single-use, owned by one user, and only valid after MinSeconds−1
//   - a claim that finds nothing new does not consume the token or the daily quota
package unlock

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/search"
	"markemind/dials/internal/settings"
)

var (
	ErrDailyLimit   = errors.New("daily unlock limit reached")
	ErrIPLimit      = errors.New("daily unlock limit reached for this network")
	ErrTokenInvalid = errors.New("ad session not found; start the ad again")
	ErrTokenUsed    = errors.New("this ad was already used")
	ErrNothingLeft  = errors.New("you have unlocked every contact for these filters")
)

// TooSoonError says how long the visitor still has to wait.
type TooSoonError struct{ Wait time.Duration }

func (e *TooSoonError) Error() string {
	return fmt.Sprintf("please wait %d more seconds", int(e.Wait.Seconds()+0.999))
}

const maxActiveTokens = 3

type Service struct {
	DB       *db.DB
	Search   *search.Service
	Settings *settings.Store
	Clock    platform.Clock

	locks [256]sync.Mutex

	ipMu  sync.Mutex
	ipDay int
	ipCnt map[string]int
}

func (s *Service) lock(uid int64) func() {
	m := &s.locks[uint64(uid)%uint64(len(s.locks))]
	m.Lock()
	return m.Unlock
}

type Token struct {
	ID         string `json:"token"`
	Mode       string `json:"mode"`
	MinSeconds int    `json:"min_seconds"`
	SmartLink  string `json:"smartlink,omitempty"`
	AdsToday   int    `json:"ads_today"`
	MaxAds     int    `json:"max_ads"`
}

func (s *Service) adsToday(ctx context.Context, q db.Querier, uid int64, day int) int {
	var n int
	q.QueryRowContext(ctx, `SELECT ads FROM user_daily WHERE user_id=? AND day=?`, uid, day).Scan(&n)
	return n
}

func (s *Service) ipCount(ip string, day int, add int) int {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	if s.ipDay != day || s.ipCnt == nil || len(s.ipCnt) > 200_000 {
		s.ipDay, s.ipCnt = day, map[string]int{}
	}
	s.ipCnt[ip] += add
	return s.ipCnt[ip]
}

// Start begins an ad view. Up to three tokens may be active at once (tabs).
func (s *Service) Start(ctx context.Context, uid int64, ip string) (Token, error) {
	cfg := s.Settings.Get()
	now := s.Clock.Now()
	day := platform.Day(now)
	t := Token{Mode: cfg.Ads.Mode, MinSeconds: cfg.Ads.MinSeconds, MaxAds: cfg.Limits.MaxAdsPerDay}
	if t.Mode == "smartlink" {
		t.SmartLink = cfg.Ads.SmartLink
	}
	t.AdsToday = s.adsToday(ctx, s.DB.R, uid, day)
	if t.AdsToday >= cfg.Limits.MaxAdsPerDay {
		return t, ErrDailyLimit
	}
	if ip != "" && s.ipCount(ip, day, 0) >= cfg.Limits.MaxAdsPerDayIP {
		return t, ErrIPLimit
	}
	t.ID = platform.Token(18)
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ad_tokens(id, user_id, created, mode) VALUES(?,?,?,?)`,
			t.ID, uid, now.UnixMilli(), t.Mode); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM ad_tokens WHERE user_id=? AND used=0 AND id NOT IN
			(SELECT id FROM ad_tokens WHERE user_id=? AND used=0 ORDER BY created DESC LIMIT ?)`, uid, uid, maxActiveTokens)
		return err
	})
	return t, err
}

type Result struct {
	Batch     int64        `json:"batch"`
	Rows      []search.Row `json:"rows"`
	Total     int64        `json:"total"`
	Unlocked  int64        `json:"unlocked"`
	Remaining int64        `json:"remaining"`
	AdsToday  int          `json:"ads_today"`
	MaxAds    int          `json:"max_ads"`
}

// Claim validates the token and hands out the next N unseen matching contacts.
func (s *Service) Claim(ctx context.Context, uid int64, tokenID string, f filter.Filter, ip string) (Result, error) {
	cfg := s.Settings.Get()
	var res Result
	res.MaxAds = cfg.Limits.MaxAdsPerDay
	if len(tokenID) < 10 || len(tokenID) > 64 {
		return res, ErrTokenInvalid
	}
	r, err := s.Search.Resolve(ctx, f)
	if err != nil {
		return res, err
	}
	unlockFn := s.lock(uid)
	defer unlockFn()

	now := s.Clock.Now()
	day := platform.Day(now)
	if ip != "" && s.ipCount(ip, day, 0) >= cfg.Limits.MaxAdsPerDayIP {
		return res, ErrIPLimit
	}
	var ids []int64
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		var owner, created int64
		var used int
		err := tx.QueryRowContext(ctx, `SELECT user_id, created, used FROM ad_tokens WHERE id=?`, tokenID).Scan(&owner, &created, &used)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != uid) {
			return ErrTokenInvalid
		}
		if err != nil {
			return err
		}
		if used != 0 {
			return ErrTokenUsed
		}
		need := time.Duration(cfg.Ads.MinSeconds-1) * time.Second
		if age := now.Sub(time.UnixMilli(created)); age < need {
			return &TooSoonError{Wait: need - age}
		}
		ads := s.adsToday(ctx, tx, uid, day)
		res.AdsToday = ads
		if ads >= cfg.Limits.MaxAdsPerDay {
			return ErrDailyLimit
		}
		if r.Impossible() {
			return ErrNothingLeft
		}
		where, args := r.Where("")
		args = append(args, uid, cfg.Limits.ContactsPerAd)
		ids, err = search.QueryIDs(ctx, tx, `SELECT c.id FROM contacts c WHERE `+where+`
			AND NOT EXISTS (SELECT 1 FROM unlocked u WHERE u.user_id=? AND u.contact_id=c.id) ORDER BY c.id LIMIT ?`, args...)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return ErrNothingLeft
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(batch),0)+1 FROM unlocked WHERE user_id=?`, uid).Scan(&res.Batch); err != nil {
			return err
		}
		vals := make([]string, len(ids))
		iargs := make([]any, 0, len(ids)*4)
		for i, id := range ids {
			vals[i] = "(?,?,?,?)"
			iargs = append(iargs, uid, id, res.Batch, now.Unix())
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO unlocked(user_id, contact_id, batch, ts) VALUES `+strings.Join(vals, ","), iargs...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ad_tokens SET used=1 WHERE id=?`, tokenID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO user_daily(user_id, day, ads, last_claim) VALUES(?,?,1,?)
			ON CONFLICT(user_id, day) DO UPDATE SET ads=ads+1, last_claim=excluded.last_claim`, uid, day, now.Unix())
		res.AdsToday = ads + 1
		return err
	})
	if err != nil {
		return res, err
	}
	if ip != "" {
		s.ipCount(ip, day, 1)
	}
	if res.Rows, err = s.Search.FetchRows(ctx, s.DB.R, ids, false); err != nil {
		return res, err
	}
	for i := range res.Rows {
		res.Rows[i].Batch, res.Rows[i].New = res.Batch, true
	}
	res.Total, res.Unlocked, err = s.counts(ctx, uid, r)
	res.Remaining = max(0, res.Total-res.Unlocked)
	return res, err
}

func (s *Service) counts(ctx context.Context, uid int64, r *search.Resolved) (total, unlocked int64, err error) {
	if total, err = s.Search.Count(ctx, r); err != nil {
		return
	}
	if r.Impossible() {
		return total, 0, nil
	}
	where, args := r.Where("")
	err = s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM unlocked u JOIN contacts c ON c.id=u.contact_id WHERE u.user_id=? AND `+where,
		append([]any{uid}, args...)...).Scan(&unlocked)
	return
}

type View struct {
	Rows        []search.Row `json:"rows"`
	Total       int64        `json:"total"`
	Unlocked    int64        `json:"unlocked"`
	Remaining   int64        `json:"remaining"`
	LatestBatch int64        `json:"latest_batch"`
	Capped      bool         `json:"capped"`
	AdsToday    int          `json:"ads_today"`
	MaxAds      int          `json:"max_ads"`
}

// View lists the user's unlocked contacts matching f, newest batch first.
func (s *Service) View(ctx context.Context, uid int64, f filter.Filter, newestOnly bool) (View, error) {
	cfg := s.Settings.Get()
	v := View{Rows: []search.Row{}, MaxAds: cfg.Limits.MaxAdsPerDay}
	v.AdsToday = s.adsToday(ctx, s.DB.R, uid, platform.Day(s.Clock.Now()))
	r, err := s.Search.Resolve(ctx, f)
	if err != nil {
		return v, err
	}
	if v.Total, v.Unlocked, err = s.counts(ctx, uid, r); err != nil {
		return v, err
	}
	v.Remaining = max(0, v.Total-v.Unlocked)
	s.DB.R.QueryRowContext(ctx, `SELECT COALESCE(MAX(batch),0) FROM unlocked WHERE user_id=?`, uid).Scan(&v.LatestBatch)
	if v.Unlocked == 0 {
		return v, nil
	}
	ids, batches, err := s.unlockedIDs(ctx, uid, r, newestOnly, v.LatestBatch, cfg.Limits.ViewCap)
	if err != nil {
		return v, err
	}
	v.Capped = int64(len(ids)) < v.Unlocked && !newestOnly
	if v.Rows, err = s.Search.FetchRows(ctx, s.DB.R, ids, false); err != nil {
		return v, err
	}
	for i := range v.Rows {
		b := batches[v.Rows[i].ID]
		v.Rows[i].Batch, v.Rows[i].New = b, b == v.LatestBatch
	}
	return v, nil
}

func (s *Service) unlockedIDs(ctx context.Context, uid int64, r *search.Resolved, newestOnly bool, latest int64, limit int) ([]int64, map[int64]int64, error) {
	where, args := r.Where("")
	q := `SELECT u.contact_id, u.batch FROM unlocked u JOIN contacts c ON c.id=u.contact_id WHERE u.user_id=? AND ` + where
	a := append([]any{uid}, args...)
	if newestOnly {
		q += ` AND u.batch=?`
		a = append(a, latest)
	}
	q += ` ORDER BY u.batch DESC, u.contact_id LIMIT ?`
	a = append(a, limit)
	rows, err := s.DB.R.QueryContext(ctx, q, a...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	batches := map[int64]int64{}
	for rows.Next() {
		var id, b int64
		if err := rows.Scan(&id, &b); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		batches[id] = b
	}
	return ids, batches, rows.Err()
}

// SafeCell neutralises spreadsheet formula injection.
func SafeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

const exportCap = 100_000

// Export streams the user's unlocked contacts matching f as CSV.
func (s *Service) Export(ctx context.Context, uid int64, f filter.Filter, w io.Writer) (int, error) {
	r, err := s.Search.Resolve(ctx, f)
	if err != nil {
		return 0, err
	}
	ids, batches, err := s.unlockedIDs(ctx, uid, r, false, 0, exportCap)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	base := []string{"Name", "Title", "Company", "Email", "Phone", "LinkedIn", "Website", "City", "State", "Country", "Industry",
		"Sector", "Company Type", "Company Size", "Revenue", "Seniority", "Department", "Decision Power", "Email Confidence", "Unlock Batch"}
	// two passes over chunks keep memory flat: first collect extra column names
	extraKeys := map[string]bool{}
	const chunk = 1000
	for i := 0; i < len(ids); i += chunk {
		rows, err := s.Search.FetchRows(ctx, s.DB.R, ids[i:min(i+chunk, len(ids))], true)
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			for k := range row.Extra {
				if len(extraKeys) < 40 {
					extraKeys[k] = true
				}
			}
		}
	}
	extras := make([]string, 0, len(extraKeys))
	for k := range extraKeys {
		extras = append(extras, k)
	}
	sort.Strings(extras)
	header := append(append([]string(nil), base...), extras...)
	for i := range header {
		header[i] = SafeCell(header[i])
	}
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	n := 0
	for i := 0; i < len(ids); i += chunk {
		rows, err := s.Search.FetchRows(ctx, s.DB.R, ids[i:min(i+chunk, len(ids))], true)
		if err != nil {
			return n, err
		}
		for _, x := range rows {
			rec := []string{x.Name, x.Title, x.Company, x.Email, x.Phone, x.LinkedIn, x.Website, x.City, x.State, x.Country, x.Industry,
				x.Sector, x.CType, x.CSize, x.Revenue, x.Seniority, x.Division, x.Decision, x.EConf, strconv.FormatInt(batches[x.ID], 10)}
			for _, k := range extras {
				rec = append(rec, x.Extra[k])
			}
			for j := range rec {
				rec[j] = SafeCell(rec[j])
			}
			if err := cw.Write(rec); err != nil {
				return n, err
			}
			n++
		}
	}
	cw.Flush()
	return n, cw.Error()
}
