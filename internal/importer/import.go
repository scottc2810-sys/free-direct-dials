package importer

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"markemind/dials/internal/db"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/normalize"
	"markemind/dials/internal/platform"
)

const (
	maxField     = 500
	maxDimField  = 120
	maxExtraVal  = 1000
	maxExtraKeys = 60
	maxTech      = 50
)

// Stats is reported live (progress) and stored in uploads.
type Stats struct {
	Rows      int64            `json:"rows"`
	Added     int64            `json:"added"`
	Updated   int64            `json:"updated"`
	Unchanged int64            `json:"unchanged"`
	Skipped   int64            `json:"skipped"`
	Reasons   map[string]int64 `json:"reasons"` // why rows were skipped
	Cleaned   map[string]int64 `json:"cleaned"` // values dropped from kept rows
	Sniffed   Sniffed          `json:"sniffed"`
}

func newStats() Stats {
	return Stats{Reasons: map[string]int64{}, Cleaned: map[string]int64{}}
}

func (s Stats) clone() Stats {
	c := s
	c.Reasons = make(map[string]int64, len(s.Reasons))
	for k, v := range s.Reasons {
		c.Reasons[k] = v
	}
	c.Cleaned = make(map[string]int64, len(s.Cleaned))
	for k, v := range s.Cleaned {
		c.Cleaned[k] = v
	}
	return c
}

type Options struct {
	Mapping   Mapping      // nil → AutoMap(headers)
	BatchSize int          // rows per transaction (default 5000)
	CanWrite  func() error // disk guard, checked per batch
	Progress  func(Stats)
	Clock     platform.Clock
	Target    Target // where records go; nil → the importer's own SQLite database
}

// Importer cleans rows and hands them to a Target. DB is the control database:
// it holds the suppression list (removed people) and, by default, the contacts.
type Importer struct {
	DB         *db.DB
	Disposable *emailcheck.Disposable
}

// Record is one cleaned input row, ready to be stored.
type Record struct {
	Name, Title, Company, Website, LinkedIn, Email, Phone string
	DimRaw                                                [12]string // as written in the file (label candidates)
	DimN                                                  [12]string // normalised (filter values)
	Tech                                                  []string   // folded technology names, sorted
	Extra                                                 map[string]string
	EmailKey, LiKey, HKey                                 string // identity keys, in match priority order
}

// Counts are the outcome of writing one batch.
type Counts struct{ Added, Updated, Unchanged int64 }

// Target stores records: one database, or a router over several (store.Federation).
type Target interface {
	Begin(ctx context.Context) error
	// Write upserts recs: a record whose identity (email, then LinkedIn, then
	// name+company+title+phone) is already stored merges into that contact.
	Write(ctx context.Context, recs []*Record, now int64) (Counts, error)
	// End settles labels, per-value counts and planner statistics.
	End(ctx context.Context, changed int64) error
	// Abort is End after a failure: committed batches stay and become consistent.
	Abort(ctx context.Context, changed int64)
}

// Stored is a contact as a store holds it; Merge works on it for every store kind.
type Stored struct {
	Dims                                                  [12]int64
	EmailKey, LiKey, HKey                                 string
	Name, Title, Company, Website, LinkedIn, Email, Phone string
	Extra                                                 map[string]string
}

// KeyFree reports whether identity key (kind 'e', 'l' or 'h') is unused or
// already belongs to the contact being merged.
type KeyFree func(kind byte, key string) (bool, error)

// Merge folds r into cur. Non-empty incoming values win; identity keys move
// only when no other contact owns them; extra columns are merged by name.
func Merge(cur *Stored, r *Record, dims [12]int64, keyFree KeyFree) (changed, techChanged bool, err error) {
	set := func(p *string, v string) {
		if v != "" && *p != v {
			*p, changed = v, true
		}
	}
	set(&cur.Name, r.Name)
	set(&cur.Title, r.Title)
	set(&cur.Company, r.Company)
	set(&cur.Website, r.Website)
	set(&cur.Phone, r.Phone)
	for i := range filter.Dims {
		if r.DimN[i] != "" && dims[i] != cur.Dims[i] {
			cur.Dims[i], changed = dims[i], true
		}
	}
	if r.Email != "" && r.EmailKey != cur.EmailKey {
		ok, err := keyFree('e', r.EmailKey)
		if err != nil {
			return false, false, err
		}
		if ok {
			cur.Email, cur.EmailKey, changed = r.Email, r.EmailKey, true
		}
	}
	if r.LiKey != "" && r.LiKey != cur.LiKey {
		ok, err := keyFree('l', r.LiKey)
		if err != nil {
			return false, false, err
		}
		if ok {
			cur.LinkedIn, cur.LiKey, changed = r.LinkedIn, r.LiKey, true
		}
	}
	if cur.HKey == "" && r.HKey != "" {
		ok, err := keyFree('h', r.HKey)
		if err != nil {
			return false, false, err
		}
		if ok {
			cur.HKey, changed = r.HKey, true
		}
	}
	if cur.Extra == nil {
		cur.Extra = map[string]string{}
	}
	techChanged = len(r.Tech) > 0 && cur.Extra["Technologies"] != r.Extra["Technologies"]
	for k, v := range r.Extra {
		if cur.Extra[k] != v {
			if len(cur.Extra) >= maxExtraKeys && cur.Extra[k] == "" {
				continue
			}
			cur.Extra[k], changed = v, true
		}
	}
	return changed, techChanged, nil
}

// FTSText is what keyword search matches: name, title, company, industry,
// sector, skills, products and keywords.
func FTSText(name, title, company, industry, sector string, extra map[string]string) string {
	parts := []string{name, title, company, industry, sector, extra["Skills"], extra["Products"], extra["Keywords"]}
	return filter.CollapseDots(strings.Join(parts, " \n "))
}

// Preview returns sniffing info, headers, the automatic mapping and up to n rows.
type PreviewResult struct {
	Sniffed Sniffed    `json:"sniffed"`
	Headers []string   `json:"headers"`
	Mapping Mapping    `json:"mapping"`
	Rows    [][]string `json:"rows"`
}

func Preview(r io.Reader, n int) (PreviewResult, error) {
	cr, sn, err := OpenCSV(r)
	if err != nil {
		return PreviewResult{}, err
	}
	head, err := cr.Read()
	if err != nil {
		return PreviewResult{}, fmt.Errorf("reading header: %w", err)
	}
	hs := CleanHeaders(append([]string(nil), head...))
	res := PreviewResult{Sniffed: sn, Headers: hs, Mapping: AutoMap(hs)}
	for len(res.Rows) < n {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				continue
			}
			return res, err
		}
		row := make([]string, len(rec))
		for i, v := range rec {
			row[i] = normalize.Truncate(normalize.Clean(v), 200)
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// Run imports the whole stream. On error the batches already committed stay
// committed (idempotent: re-running the same file merges, never duplicates).
func (im *Importer) Run(ctx context.Context, r io.Reader, opt Options) (Stats, error) {
	st := newStats()
	if opt.BatchSize <= 0 {
		opt.BatchSize = 5000
	}
	if opt.Clock == nil {
		opt.Clock = platform.SystemClock{}
	}
	cr, sn, err := OpenCSV(r)
	if err != nil {
		return st, err
	}
	st.Sniffed = sn
	head, err := cr.Read()
	if err != nil {
		return st, fmt.Errorf("reading header: %w", err)
	}
	headers := CleanHeaders(append([]string(nil), head...))
	mapping := opt.Mapping
	if mapping == nil {
		mapping = AutoMap(headers)
	}
	if len(mapping) != len(headers) {
		return st, fmt.Errorf("mapping has %d columns, file has %d", len(mapping), len(headers))
	}
	if err := mapping.Validate(); err != nil {
		return st, err
	}
	run := &runState{im: im, opt: opt, idx: mapping.index(), headers: headers, stats: &st}
	tgt := opt.Target
	if tgt == nil {
		tgt = &SQLiteTarget{DB: im.DB}
	}
	if err := tgt.Begin(ctx); err != nil {
		return st, err
	}
	ended := false
	defer func() {
		if !ended {
			tgt.Abort(context.WithoutCancel(ctx), st.Added+st.Updated)
		}
	}()

	batch := make([][]string, 0, opt.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if opt.CanWrite != nil {
			if err := opt.CanWrite(); err != nil {
				return err
			}
		}
		if err := run.processBatch(ctx, tgt, batch); err != nil {
			return err
		}
		batch = batch[:0]
		if opt.Progress != nil {
			opt.Progress(st.clone())
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				st.Rows++
				st.Skipped++
				st.Reasons["malformed"]++
				continue
			}
			return st, err
		}
		st.Rows++
		batch = append(batch, append([]string(nil), rec...))
		if len(batch) >= opt.BatchSize {
			if err := flush(); err != nil {
				return st, err
			}
		}
	}
	if err := flush(); err != nil {
		return st, err
	}
	ended = true
	if err := tgt.End(ctx, st.Added+st.Updated); err != nil {
		return st, err
	}
	return st, nil
}

// processBatch cleans one batch, drops suppressed (removed) people and writes the rest.
func (rs *runState) processBatch(ctx context.Context, tgt Target, batch [][]string) error {
	recs := make([]*Record, 0, len(batch))
	for _, fields := range batch {
		rec, reason := rs.build(fields)
		if reason != "" {
			rs.stats.Skipped++
			rs.stats.Reasons[reason]++
			continue
		}
		recs = append(recs, &rec)
	}
	recs, err := rs.im.dropSuppressed(ctx, recs, rs.stats)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	c, err := tgt.Write(ctx, recs, rs.opt.Clock.Now().Unix())
	if err != nil {
		return err
	}
	rs.stats.Added += c.Added
	rs.stats.Updated += c.Updated
	rs.stats.Unchanged += c.Unchanged
	return nil
}

// dropSuppressed removes records whose identity is on the suppression list.
func (im *Importer) dropSuppressed(ctx context.Context, recs []*Record, st *Stats) ([]*Record, error) {
	if im.DB == nil || len(recs) == 0 {
		return recs, nil
	}
	var keys []any
	for _, r := range recs {
		for _, k := range SuppressKeys(r.EmailKey, r.LiKey, r.HKey) {
			keys = append(keys, k)
		}
	}
	hit := map[string]bool{}
	for len(keys) > 0 {
		n := min(500, len(keys))
		rows, err := im.DB.R.QueryContext(ctx, `SELECT k FROM suppress WHERE k IN (`+db.Placeholders(n)+`)`, keys[:n]...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			hit[k] = true
		}
		rows.Close()
		keys = keys[n:]
	}
	if len(hit) == 0 {
		return recs, nil
	}
	out := recs[:0]
	for _, r := range recs {
		drop := false
		for _, k := range SuppressKeys(r.EmailKey, r.LiKey, r.HKey) {
			drop = drop || hit[k]
		}
		if drop {
			st.Skipped++
			st.Reasons["suppressed"]++
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

type runState struct {
	im      *Importer
	opt     Options
	idx     map[string][]int
	headers []string
	stats   *Stats
}

func (rs *runState) get(fields []string, field string, max int) string {
	for _, i := range rs.idx[field] {
		if i < len(fields) {
			if v := normalize.Value(fields[i]); v != "" {
				return normalize.Truncate(v, max)
			}
		}
	}
	return ""
}

// build cleans one row; reason != "" means skip.
func (rs *runState) build(fields []string) (Record, string) {
	var r Record
	g := func(f string) string { return rs.get(fields, f, maxField) }
	r.Name = g(FName)
	if r.Name == "" {
		r.Name = strings.TrimSpace(g(FFirst) + " " + g(FLast))
	}
	r.Title, r.Company, r.Website = g(FTitle), g(FCompany), g(FWebsite)

	if raw := g(FEmail); raw != "" {
		found := false
		for _, cand := range strings.FieldsFunc(raw, func(c rune) bool { return c == ';' || c == ',' || c == ' ' || c == '/' }) {
			e, ok, _ := emailcheck.Syntax(cand)
			if !ok {
				continue
			}
			if rs.im.Disposable != nil && rs.im.Disposable.IsDisposable(emailcheck.Domain(e)) {
				rs.stats.Cleaned["email_disposable"]++
				found = true
				break
			}
			r.Email, found = e, true
			break
		}
		if !found {
			rs.stats.Cleaned["email_invalid"]++
		}
	}
	for _, f := range phonePriority {
		for _, i := range rs.idx[f] {
			if i >= len(fields) || r.Phone != "" {
				continue
			}
			v := normalize.Value(fields[i])
			if v == "" {
				continue
			}
			if normalize.ValidPhone(v) {
				r.Phone = normalize.Truncate(v, 40)
			} else {
				rs.stats.Cleaned["phone_invalid"]++
			}
		}
	}
	if li := g(FLinkedIn); li != "" {
		if k := normalize.LinkedInKey(li); k != "" {
			r.LinkedIn, r.LiKey = li, k
		} else {
			rs.stats.Cleaned["linkedin_invalid"]++
		}
	}
	for i, d := range filter.Dims {
		raw := rs.get(fields, d.Key, maxDimField)
		if n := normalize.Dim(d.Key, raw); n != "" {
			r.DimRaw[i], r.DimN[i] = raw, n
		}
	}
	var techNames []string
	if t := rs.get(fields, FTech, 16000); t != "" {
		seen := map[string]bool{}
		for _, p := range techItems(t) {
			if c := filter.CleanTech(p); c != "" && !seen[c] {
				seen[c] = true
				r.Tech = append(r.Tech, c)
				techNames = append(techNames, normalize.Truncate(normalize.Clean(p), 60))
				if len(r.Tech) == maxTech {
					break
				}
			}
		}
		sort.Strings(r.Tech)
	}
	r.Extra = map[string]string{}
	if len(techNames) > 0 { // shown in CSV exports; search uses tech_fts
		r.Extra["Technologies"] = normalize.Truncate(strings.Join(techNames, ", "), 2000)
	}
	for k, f := range map[string]string{"Skills": FSkills, "Products": FProducts, "Keywords": FKeywords} {
		if v := rs.get(fields, f, maxExtraVal); v != "" {
			r.Extra[k] = v
		}
	}
	for _, i := range rs.idx[FExtra] {
		if i >= len(fields) || len(r.Extra) >= maxExtraKeys {
			continue
		}
		if v := normalize.Value(fields[i]); v != "" {
			r.Extra[rs.headers[i]] = normalize.Truncate(v, maxExtraVal)
		}
	}

	if r.Name == "" && r.Email == "" && r.Company == "" && r.Phone == "" && r.LiKey == "" {
		return r, "empty_row"
	}
	if r.Email != "" {
		r.EmailKey = normalize.EmailKey(r.Email)
	}
	r.HKey = normalize.HashKey(r.Name, r.Company, r.Title, r.Phone)
	if r.EmailKey == "" && r.LiKey == "" && r.HKey == "" {
		return r, "no_identity"
	}
	return r, ""
}

// techItems splits a technology cell. "Name|Category,Name|Category" (Slintel,
// BuiltWith exports) keeps only the names; otherwise , ; and | all separate.
func techItems(t string) []string {
	if strings.Contains(t, ",") && strings.Contains(t, "|") {
		var out []string
		for _, item := range strings.Split(t, ",") {
			name, _, _ := strings.Cut(item, "|")
			out = append(out, name)
		}
		return out
	}
	return strings.FieldsFunc(t, func(c rune) bool { return c == ',' || c == ';' || c == '|' })
}

// SuppressKeys returns the hashed suppression keys for a contact identity.
func SuppressKeys(emailKey, liKey, hKey string) []string {
	var ks []string
	if emailKey != "" {
		ks = append(ks, platform.SHA256Hex("e:"+emailKey))
	}
	if liKey != "" {
		ks = append(ks, platform.SHA256Hex("l:"+liKey))
	}
	if hKey != "" {
		ks = append(ks, platform.SHA256Hex("h:"+hKey))
	}
	return ks
}

// Refresh recomputes per-value counts and headline stats, bumps the dataset
// version (invalidating caches) and refreshes planner statistics.
func Refresh(ctx context.Context, d *db.DB) error {
	for _, dm := range filter.Dims {
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE dims SET cnt=0 WHERE dim=? AND cnt<>0`, dm.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE dims SET cnt = g.n FROM (SELECT `+dm.Key+` AS id, COUNT(*) AS n
				FROM contacts GROUP BY `+dm.Key+`) AS g WHERE dims.id = g.id`)
			return err
		})
		if err != nil {
			return fmt.Errorf("refresh %s: %w", dm.Key, err)
		}
	}
	if err := RefreshStats(ctx, d); err != nil {
		return err
	}
	if _, err := d.BumpDataset(ctx); err != nil {
		return err
	}
	// fresh, sampled planner statistics: without them SQLite picked a single-column
	// index plus row lookups over a covering composite (see docs/PERFORMANCE.md)
	if _, err := d.W.ExecContext(ctx, `PRAGMA analysis_limit=2000`); err != nil {
		return err
	}
	if _, err := d.W.ExecContext(ctx, `ANALYZE`); err != nil {
		return err
	}
	_, err := d.W.ExecContext(ctx, `PRAGMA optimize`)
	return err
}

// HeadlineStats are shown on the landing page and in JSON-LD.
type HeadlineStats struct {
	Contacts  int64 `json:"contacts"`
	WithEmail int64 `json:"with_email"`
	WithPhone int64 `json:"with_phone"`
	Countries int64 `json:"countries"`
	Updated   int64 `json:"updated"`
}

func RefreshStats(ctx context.Context, d *db.DB) error {
	var h HeadlineStats
	if err := d.W.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(has_email),0), COALESCE(SUM(has_phone),0) FROM contacts`).
		Scan(&h.Contacts, &h.WithEmail, &h.WithPhone); err != nil {
		return err
	}
	d.W.QueryRowContext(ctx, `SELECT COUNT(*) FROM dims WHERE dim=1 AND id>12 AND cnt>0`).Scan(&h.Countries)
	d.W.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated),0) FROM contact_info`).Scan(&h.Updated)
	b, _ := json.Marshal(h)
	return db.SetMeta(ctx, d.W, "stats", string(b))
}

func LoadStats(ctx context.Context, q db.Querier) HeadlineStats {
	var h HeadlineStats
	json.Unmarshal([]byte(db.GetMeta(ctx, q, "stats")), &h)
	return h
}

var (
	zOnce sync.Once
	zEnc  *zstd.Encoder
	zDec  *zstd.Decoder
)

func zinit() {
	zOnce.Do(func() {
		zEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(true), zstd.WithEncoderLevel(zstd.SpeedDefault))
		zDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(4<<20))
	})
}

// EncodeExtra stores extra columns as JSON, zstd-compressed when worthwhile.
// Byte 0 is the format: 0 = raw JSON, 1 = zstd(JSON).
func EncodeExtra(m map[string]string) []byte {
	if len(m) == 0 {
		return nil
	}
	js, _ := json.Marshal(m)
	if len(js) < 96 {
		return append([]byte{0}, js...)
	}
	zinit()
	return zEnc.EncodeAll(js, []byte{1})
}

func DecodeExtra(b []byte) map[string]string {
	m := map[string]string{}
	if len(b) < 2 {
		return m
	}
	js := b[1:]
	if b[0] == 1 {
		zinit()
		var err error
		if js, err = zDec.DecodeAll(b[1:], nil); err != nil {
			return m
		}
	}
	json.Unmarshal(js, &m)
	return m
}
