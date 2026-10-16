// Package unlock implements the monetisation core: ad tokens, claiming the
// next N never-before-unlocked contacts, the personal (paged) view and CSV export.
//
// Invariants (acceptance #6, #7, #9):
//   - a contact is handed to a user at most once (PRIMARY KEY in each store + per-user lock)
//   - a token is single-use, owned by one user, and only valid after the break's minimum time
//   - a claim that finds nothing new does not consume the token or the daily quota
//
// Tokens, quotas and batch numbers live in the control database; the unlocks
// themselves live next to the contacts, in whichever store holds them.
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
	"markemind/dials/internal/store"
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

// gptFloor is the minimum token age in Google rewarded mode: Google grants the
// reward only after the ad completes, so the server floor just stops scripts.
const gptFloor = 5

type Service struct {
	DB       *db.DB            // control database: tokens, quotas, batch numbers
	Fed      *store.Federation // contact stores
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

// minSeconds is how long a token of mode must age before it can be claimed.
func minSeconds(mode string, cfg settings.All) int {
	if mode == "gpt_rewarded" {
		return min(cfg.Ads.MinSeconds, gptFloor)
	}
	return cfg.Ads.MinSeconds
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
	t := Token{Mode: cfg.Ads.Mode, MaxAds: cfg.Limits.MaxAdsPerDay}
	t.MinSeconds = minSeconds(t.Mode, cfg)
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
	unlockFn := s.lock(uid)
	defer unlockFn()

	now := s.Clock.Now()
	day := platform.Day(now)
	if ip != "" && s.ipCount(ip, day, 0) >= cfg.Limits.MaxAdsPerDayIP {
		return res, ErrIPLimit
	}
	var owner, created int64
	var used int
	var mode string
	err := s.DB.W.QueryRowContext(ctx, `SELECT user_id, created, used, mode FROM ad_tokens WHERE id=?`, tokenID).Scan(&owner, &created, &used, &mode)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != uid) {
		return res, ErrTokenInvalid
	}
	if err != nil {
		return res, err
	}
	if used != 0 {
		return res, ErrTokenUsed
	}
	need := time.Duration(minSeconds(mode, cfg)-1) * time.Second
	if age := now.Sub(time.UnixMilli(created)); age < need {
		return res, &TooSoonError{Wait: need - age}
	}
	ads := s.adsToday(ctx, s.DB.W, uid, day)
	res.AdsToday = ads
	if ads >= cfg.Limits.MaxAdsPerDay {
		return res, ErrDailyLimit
	}
	var last int64
	if err := s.DB.W.QueryRowContext(ctx, `SELECT last_batch FROM users WHERE id=?`, uid).Scan(&last); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	res.Batch = last + 1
	rows, err := s.Fed.Claim(ctx, uid, res.Batch, f, cfg.Limits.ContactsPerAd, now.Unix())
	if err != nil {
		return res, err
	}
	if len(rows) == 0 {
		return res, ErrNothingLeft
	}
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE ad_tokens SET used=1 WHERE id=?`, tokenID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET last_batch=? WHERE id=? AND last_batch<?`, res.Batch, uid, res.Batch); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO user_daily(user_id, day, ads, last_claim) VALUES(?,?,1,?)
			ON CONFLICT(user_id, day) DO UPDATE SET ads=ads+1, last_claim=excluded.last_claim`, uid, day, now.Unix())
		return err
	})
	if err != nil {
		return res, err
	}
	res.AdsToday = ads + 1
	if ip != "" {
		s.ipCount(ip, day, 1)
	}
	res.Rows = rows
	res.Total, res.Unlocked, err = s.counts(ctx, uid, f)
	res.Remaining = max(0, res.Total-res.Unlocked)
	return res, err
}

func (s *Service) counts(ctx context.Context, uid int64, f filter.Filter) (total, unlocked int64, err error) {
	if total, _, err = s.Fed.Count(ctx, f); err != nil {
		return
	}
	unlocked, err = s.Fed.Unlocked(ctx, uid, f)
	return
}

type View struct {
	Rows        []search.Row `json:"rows"`
	Total       int64        `json:"total"`
	Unlocked    int64        `json:"unlocked"`
	Remaining   int64        `json:"remaining"`
	LatestBatch int64        `json:"latest_batch"`
	AdsToday    int          `json:"ads_today"`
	MaxAds      int          `json:"max_ads"`
	PageSize    int          `json:"page_size"`
	Next        string       `json:"next"`              // cursor of the next page ("" = last page)
	Partial     bool         `json:"partial,omitempty"` // a database is down: its contacts are missing
	Pending     bool         `json:"pending,omitempty"` // more databases are still being searched
}

// View lists one page of the user's unlocked contacts matching f, newest batch
// first. cursor is "" for the first page or a previous View.Next.
func (s *Service) View(ctx context.Context, uid int64, f filter.Filter, newestOnly bool, cursor string) (View, error) {
	cfg := s.Settings.Get()
	v := View{Rows: []search.Row{}, MaxAds: cfg.Limits.MaxAdsPerDay, PageSize: cfg.Limits.PageSize}
	v.AdsToday = s.adsToday(ctx, s.DB.R, uid, platform.Day(s.Clock.Now()))
	total, st, err := s.Fed.Count(ctx, f)
	if err != nil {
		return v, err
	}
	v.Total, v.Partial, v.Pending = total, len(st.Partial) > 0, st.Pending
	if v.Unlocked, err = s.Fed.Unlocked(ctx, uid, f); err != nil {
		return v, err
	}
	v.Remaining = max(0, v.Total-v.Unlocked)
	s.DB.R.QueryRowContext(ctx, `SELECT last_batch FROM users WHERE id=?`, uid).Scan(&v.LatestBatch)
	if v.Unlocked == 0 {
		return v, nil
	}
	var only int64
	if newestOnly {
		only = v.LatestBatch
	}
	rows, next, err := s.Fed.ViewPage(ctx, uid, f, store.ParseCursor(cursor), only, v.PageSize, false)
	if err != nil {
		return v, err
	}
	for i := range rows {
		rows[i].New = rows[i].Batch == v.LatestBatch
	}
	v.Rows, v.Next = rows, next.String()
	return v, nil
}

// Summary is the signed-in header: today's breaks and lifetime unlocks.
type Summary struct {
	AdsToday int
	MaxAds   int
	Unlocked int64
}

func (s *Service) Summary(ctx context.Context, uid int64) (Summary, error) {
	cfg := s.Settings.Get()
	sm := Summary{MaxAds: cfg.Limits.MaxAdsPerDay, AdsToday: s.adsToday(ctx, s.DB.R, uid, platform.Day(s.Clock.Now()))}
	var err error
	sm.Unlocked, err = s.Fed.Unlocked(ctx, uid, filter.Filter{})
	return sm, err
}

// SafeCell neutralises spreadsheet formula injection.
func SafeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

const (
	exportCap   = 100_000
	exportChunk = 1000
)

// Export streams the user's unlocked contacts matching f as CSV.
func (s *Service) Export(ctx context.Context, uid int64, f filter.Filter, w io.Writer) (int, error) {
	// walk pages in view order; two passes keep memory flat: first collect extra column names
	walk := func(fn func(search.Row) error) error {
		var cur store.Cursor
		seen := 0
		for seen < exportCap {
			rows, next, err := s.Fed.ViewPage(ctx, uid, f, cur, 0, min(exportChunk, exportCap-seen), true)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if err := fn(r); err != nil {
					return err
				}
			}
			seen += len(rows)
			if next.IsZero() || len(rows) == 0 {
				return nil
			}
			cur = next
		}
		return nil
	}
	extraKeys := map[string]bool{}
	if err := walk(func(r search.Row) error {
		for k := range r.Extra {
			if len(extraKeys) < 40 {
				extraKeys[k] = true
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	extras := make([]string, 0, len(extraKeys))
	for k := range extraKeys {
		extras = append(extras, k)
	}
	sort.Strings(extras)
	cw := csv.NewWriter(w)
	base := []string{"Name", "Title", "Company", "Email", "Phone", "LinkedIn", "Website", "City", "State", "Country", "Industry",
		"Sector", "Company Type", "Company Size", "Revenue", "Seniority", "Department", "Decision Power", "Email Confidence", "Unlock Batch"}
	header := append(append([]string(nil), base...), extras...)
	for i := range header {
		header[i] = SafeCell(header[i])
	}
	if err := cw.Write(header); err != nil {
		return 0, err
	}
	n := 0
	err := walk(func(x search.Row) error {
		rec := []string{x.Name, x.Title, x.Company, x.Email, x.Phone, x.LinkedIn, x.Website, x.City, x.State, x.Country, x.Industry,
			x.Sector, x.CType, x.CSize, x.Revenue, x.Seniority, x.Division, x.Decision, x.EConf, strconv.FormatInt(x.Batch, 10)}
		for _, k := range extras {
			rec = append(rec, x.Extra[k])
		}
		for j := range rec {
			rec[j] = SafeCell(rec[j])
		}
		n++
		return cw.Write(rec)
	})
	if err != nil {
		return n, err
	}
	cw.Flush()
	return n, cw.Error()
}
