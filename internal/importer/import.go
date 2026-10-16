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
	BatchSize int          // rows per transaction (default 2000)
	CanWrite  func() error // disk guard, checked per batch
	Progress  func(Stats)
	Clock     platform.Clock
}

type Importer struct {
	DB         *db.DB
	Disposable *emailcheck.Disposable
}

// record is one cleaned CSV row.
type record struct {
	name, title, company, website, linkedin, email, phone string
	dimRaw                                                [12]string
	dimN                                                  [12]string
	tech                                                  []string
	extra                                                 map[string]string
	emailKey, liKey, hKey                                 string
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
	run := &runState{im: im, opt: opt, idx: mapping.index(), headers: headers, stats: &st,
		dimCache: map[int]map[string]int64{}, touched: map[int64]struct{}{}}
	bulk, err := beginBulk(ctx, im.DB)
	if err != nil {
		return st, err
	}
	bulkDone := false
	defer func() {
		if !bulkDone {
			bulk.end(context.WithoutCancel(ctx), st.Added+st.Updated)
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
		if err := run.processBatch(ctx, batch); err != nil {
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
	bulkDone = true
	if err := bulk.end(ctx, st.Added+st.Updated); err != nil {
		return st, err
	}
	if err := run.finalize(ctx); err != nil {
		return st, err
	}
	return st, nil
}

type runState struct {
	im       *Importer
	opt      Options
	idx      map[string][]int
	headers  []string
	stats    *Stats
	dimCache map[int]map[string]int64 // dim index → value_n → id
	touched  map[int64]struct{}       // dim ids whose label may change
	spell    map[int64]map[string]int64
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
func (rs *runState) build(fields []string) (record, string) {
	var r record
	g := func(f string) string { return rs.get(fields, f, maxField) }
	r.name = g(FName)
	if r.name == "" {
		r.name = strings.TrimSpace(g(FFirst) + " " + g(FLast))
	}
	r.title, r.company, r.website = g(FTitle), g(FCompany), g(FWebsite)

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
			r.email, found = e, true
			break
		}
		if !found {
			rs.stats.Cleaned["email_invalid"]++
		}
	}
	for _, f := range phonePriority {
		for _, i := range rs.idx[f] {
			if i >= len(fields) || r.phone != "" {
				continue
			}
			v := normalize.Value(fields[i])
			if v == "" {
				continue
			}
			if normalize.ValidPhone(v) {
				r.phone = normalize.Truncate(v, 40)
			} else {
				rs.stats.Cleaned["phone_invalid"]++
			}
		}
	}
	if li := g(FLinkedIn); li != "" {
		if k := normalize.LinkedInKey(li); k != "" {
			r.linkedin, r.liKey = li, k
		} else {
			rs.stats.Cleaned["linkedin_invalid"]++
		}
	}
	for i, d := range filter.Dims {
		raw := rs.get(fields, d.Key, maxDimField)
		if n := normalize.Dim(d.Key, raw); n != "" {
			r.dimRaw[i], r.dimN[i] = raw, n
		}
	}
	var techNames []string
	if t := rs.get(fields, FTech, 16000); t != "" {
		seen := map[string]bool{}
		for _, p := range techItems(t) {
			if c := filter.CleanTech(p); c != "" && !seen[c] {
				seen[c] = true
				r.tech = append(r.tech, c)
				techNames = append(techNames, normalize.Truncate(normalize.Clean(p), 60))
				if len(r.tech) == maxTech {
					break
				}
			}
		}
		sort.Strings(r.tech)
	}
	r.extra = map[string]string{}
	if len(techNames) > 0 { // shown in CSV exports; search uses tech_fts
		r.extra["Technologies"] = normalize.Truncate(strings.Join(techNames, ", "), 2000)
	}
	for k, f := range map[string]string{"Skills": FSkills, "Products": FProducts, "Keywords": FKeywords} {
		if v := rs.get(fields, f, maxExtraVal); v != "" {
			r.extra[k] = v
		}
	}
	for _, i := range rs.idx[FExtra] {
		if i >= len(fields) || len(r.extra) >= maxExtraKeys {
			continue
		}
		if v := normalize.Value(fields[i]); v != "" {
			r.extra[rs.headers[i]] = normalize.Truncate(v, maxExtraVal)
		}
	}

	if r.name == "" && r.email == "" && r.company == "" && r.phone == "" && r.liKey == "" {
		return r, "empty_row"
	}
	if r.email != "" {
		r.emailKey = normalize.EmailKey(r.email)
	}
	r.hKey = normalize.HashKey(r.name, r.company, r.title, r.phone)
	if r.emailKey == "" && r.liKey == "" && r.hKey == "" {
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

type stmts struct {
	byEmail, byLi, byH, load, label                          *sql.Stmt
	insC, insI, updC, updI, ftsDel, ftsIns, techDel, techIns *sql.Stmt
	dimGet, dimIns, spell, supp                              *sql.Stmt
}

func prepare(ctx context.Context, tx *sql.Tx) (*stmts, error) {
	s := &stmts{}
	dimCols := make([]string, len(filter.Dims))
	for i, d := range filter.Dims {
		dimCols[i] = d.Key
	}
	cols := strings.Join(dimCols, ",")
	sets := make([]string, len(dimCols))
	for i, c := range dimCols {
		sets[i] = c + "=?"
	}
	defs := []struct {
		dst *(*sql.Stmt)
		q   string
	}{
		{&s.byEmail, `SELECT id FROM contact_info WHERE email_key=?`},
		{&s.byLi, `SELECT id FROM contact_info WHERE li_key=?`},
		{&s.byH, `SELECT id FROM contact_info WHERE h_key=?`},
		{&s.load, `SELECT ` + prefixed("c.", dimCols) + `, i.email_key, i.li_key, i.h_key, i.name, i.title, i.company,
			i.website, i.linkedin, i.email, i.phone, i.extra FROM contacts c JOIN contact_info i ON i.id=c.id WHERE c.id=?`},
		{&s.label, `SELECT value_n FROM dims WHERE id=?`},
		{&s.insC, `INSERT INTO contacts(` + cols + `, has_email, has_phone) VALUES(` + db.Placeholders(len(dimCols)+2) + `)`},
		{&s.insI, `INSERT INTO contact_info(id, email_key, li_key, h_key, name, title, company, website, linkedin, email, phone, extra, updated)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`},
		{&s.updC, `UPDATE contacts SET ` + strings.Join(sets, ",") + `, has_email=?, has_phone=? WHERE id=?`},
		{&s.updI, `UPDATE contact_info SET email_key=?, li_key=?, h_key=?, name=?, title=?, company=?, website=?, linkedin=?,
			email=?, phone=?, extra=?, updated=? WHERE id=?`},
		{&s.ftsDel, `DELETE FROM fts WHERE rowid=?`},
		{&s.ftsIns, `INSERT INTO fts(rowid, txt) VALUES(?,?)`},
		{&s.techDel, `DELETE FROM tech_fts WHERE rowid=?`},
		{&s.techIns, `INSERT INTO tech_fts(rowid, txt) VALUES(?,?)`},
		{&s.dimGet, `SELECT id FROM dims WHERE dim=? AND value_n=?`},
		{&s.dimIns, `INSERT INTO dims(dim, value_n, label) VALUES(?,?,?) RETURNING id`},
		{&s.spell, `INSERT INTO dim_spell(dim_id, raw, cnt) VALUES(?,?,?) ON CONFLICT(dim_id, raw) DO UPDATE SET cnt=cnt+excluded.cnt`},
		{&s.supp, `SELECT 1 FROM suppress WHERE k IN (?,?,?) LIMIT 1`},
	}
	for _, d := range defs {
		st, err := tx.PrepareContext(ctx, d.q)
		if err != nil {
			return nil, fmt.Errorf("prepare %q: %w", d.q[:min(40, len(d.q))], err)
		}
		*d.dst = st
	}
	return s, nil
}

func prefixed(p string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = p + c
	}
	return strings.Join(out, ",")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (rs *runState) processBatch(ctx context.Context, batch [][]string) error {
	now := rs.opt.Clock.Now().Unix()
	rs.spell = map[int64]map[string]int64{}
	return rs.im.DB.Tx(ctx, func(tx *sql.Tx) error {
		s, err := prepare(ctx, tx)
		if err != nil {
			return err
		}
		for _, fields := range batch {
			rec, reason := rs.build(fields)
			if reason != "" {
				rs.stats.Skipped++
				rs.stats.Reasons[reason]++
				continue
			}
			keys := SuppressKeys(rec.emailKey, rec.liKey, rec.hKey)
			for len(keys) < 3 {
				keys = append(keys, "")
			}
			var one int
			if err := s.supp.QueryRowContext(ctx, keys[0], keys[1], keys[2]).Scan(&one); err == nil {
				rs.stats.Skipped++
				rs.stats.Reasons["suppressed"]++
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := rs.upsert(ctx, s, &rec, now); err != nil {
				return err
			}
		}
		for id, m := range rs.spell {
			for raw, n := range m {
				if _, err := s.spell.ExecContext(ctx, id, raw, n); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (rs *runState) dimID(ctx context.Context, s *stmts, i int, valueN, raw string) (int64, error) {
	if valueN == "" {
		return int64(i + 1), nil // blank ids 1..12
	}
	m := rs.dimCache[i]
	if m == nil {
		m = map[string]int64{}
		rs.dimCache[i] = m
	}
	id, ok := m[valueN]
	if !ok {
		err := s.dimGet.QueryRowContext(ctx, i+1, valueN).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = s.dimIns.QueryRowContext(ctx, i+1, valueN, raw).Scan(&id)
		}
		if err != nil {
			return 0, err
		}
		if len(m) < 200_000 {
			m[valueN] = id
		}
	}
	rs.touched[id] = struct{}{}
	sp := rs.spell[id]
	if sp == nil {
		sp = map[string]int64{}
		rs.spell[id] = sp
	}
	sp[raw]++
	return id, nil
}

func (rs *runState) find(ctx context.Context, s *stmts, r *record) (int64, error) {
	try := func(st *sql.Stmt, k string) (int64, error) {
		if k == "" {
			return 0, nil
		}
		var id int64
		err := st.QueryRowContext(ctx, k).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return id, err
	}
	for _, p := range []struct {
		st *sql.Stmt
		k  string
	}{{s.byEmail, r.emailKey}, {s.byLi, r.liKey}, {s.byH, r.hKey}} {
		id, err := try(p.st, p.k)
		if err != nil || id != 0 {
			return id, err
		}
	}
	return 0, nil
}

// keyFree reports whether key is unused or already belongs to self.
func keyFree(ctx context.Context, st *sql.Stmt, key string, self int64) (bool, error) {
	var id int64
	err := st.QueryRowContext(ctx, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return id == self, err
}

func (rs *runState) upsert(ctx context.Context, s *stmts, r *record, now int64) error {
	var dims [12]int64
	for i := range filter.Dims {
		id, err := rs.dimID(ctx, s, i, r.dimN[i], r.dimRaw[i])
		if err != nil {
			return err
		}
		dims[i] = id
	}
	id, err := rs.find(ctx, s, r)
	if err != nil {
		return err
	}
	if id == 0 {
		args := make([]any, 0, 14)
		for _, d := range dims {
			args = append(args, d)
		}
		args = append(args, b2i(r.email != ""), b2i(r.phone != ""))
		res, err := s.insC.ExecContext(ctx, args...)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := s.insI.ExecContext(ctx, id, nullable(r.emailKey), nullable(r.liKey), nullable(r.hKey), r.name, r.title,
			r.company, r.website, r.linkedin, r.email, r.phone, EncodeExtra(r.extra), now); err != nil {
			return err
		}
		if err := rs.writeSearch(ctx, s, id, r, r.dimN[3], r.dimN[4], false); err != nil {
			return err
		}
		if err := writeTech(ctx, s, id, r.tech, false); err != nil {
			return err
		}
		rs.stats.Added++
		return nil
	}
	return rs.merge(ctx, s, id, r, dims, now)
}

func (rs *runState) merge(ctx context.Context, s *stmts, id int64, r *record, dims [12]int64, now int64) error {
	var old [12]int64
	var ek, lk, hk sql.NullString
	var name, title, company, website, linkedin, email, phone string
	var extraBlob []byte
	dst := make([]any, 0, 24)
	for i := range old {
		dst = append(dst, &old[i])
	}
	dst = append(dst, &ek, &lk, &hk, &name, &title, &company, &website, &linkedin, &email, &phone, &extraBlob)
	if err := s.load.QueryRowContext(ctx, id).Scan(dst...); err != nil {
		return err
	}
	changed := false
	set := func(p *string, v string) {
		if v != "" && *p != v {
			*p, changed = v, true
		}
	}
	set(&name, r.name)
	set(&title, r.title)
	set(&company, r.company)
	set(&website, r.website)
	set(&phone, r.phone)
	newDims := old
	for i := range filter.Dims {
		if r.dimN[i] != "" && dims[i] != old[i] {
			newDims[i], changed = dims[i], true
		}
	}
	emailKey, liKey, hKey := ek.String, lk.String, hk.String
	if r.email != "" && r.emailKey != emailKey {
		if ok, err := keyFree(ctx, s.byEmail, r.emailKey, id); err != nil {
			return err
		} else if ok {
			email, emailKey, changed = r.email, r.emailKey, true
		}
	}
	if r.liKey != "" && r.liKey != liKey {
		if ok, err := keyFree(ctx, s.byLi, r.liKey, id); err != nil {
			return err
		} else if ok {
			linkedin, liKey, changed = r.linkedin, r.liKey, true
		}
	}
	if hKey == "" && r.hKey != "" {
		if ok, err := keyFree(ctx, s.byH, r.hKey, id); err != nil {
			return err
		} else if ok {
			hKey, changed = r.hKey, true
		}
	}
	extra := DecodeExtra(extraBlob)
	techChanged := len(r.tech) > 0 && extra["Technologies"] != r.extra["Technologies"]
	for k, v := range r.extra {
		if extra[k] != v {
			if len(extra) >= maxExtraKeys && extra[k] == "" {
				continue
			}
			extra[k], changed = v, true
		}
	}
	if !changed {
		rs.stats.Unchanged++
		return nil
	}
	args := make([]any, 0, 15)
	for _, d := range newDims {
		args = append(args, d)
	}
	args = append(args, b2i(email != ""), b2i(phone != ""), id)
	if _, err := s.updC.ExecContext(ctx, args...); err != nil {
		return err
	}
	if _, err := s.updI.ExecContext(ctx, nullable(emailKey), nullable(liKey), nullable(hKey), name, title, company, website,
		linkedin, email, phone, EncodeExtra(extra), now, id); err != nil {
		return err
	}
	merged := &record{name: name, title: title, company: company, extra: extra}
	ind, err := valueN(ctx, s, newDims[3])
	if err != nil {
		return err
	}
	sec, err := valueN(ctx, s, newDims[4])
	if err != nil {
		return err
	}
	if err := rs.writeSearch(ctx, s, id, merged, ind, sec, true); err != nil {
		return err
	}
	if techChanged {
		if err := writeTech(ctx, s, id, r.tech, true); err != nil {
			return err
		}
	}
	rs.stats.Updated++
	return nil
}

func valueN(ctx context.Context, s *stmts, id int64) (string, error) {
	if id <= 12 {
		return "", nil
	}
	var v string
	err := s.label.QueryRowContext(ctx, id).Scan(&v)
	return v, err
}

// ftsText is what keyword search matches: name, title, company, industry,
// sector, skills, products and keywords.
func ftsText(r *record, industry, sector string) string {
	parts := []string{r.name, r.title, r.company, industry, sector, r.extra["Skills"], r.extra["Products"], r.extra["Keywords"]}
	return strings.Join(parts, " \n ")
}

func (rs *runState) writeSearch(ctx context.Context, s *stmts, id int64, r *record, industry, sector string, replace bool) error {
	if replace {
		if _, err := s.ftsDel.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	_, err := s.ftsIns.ExecContext(ctx, id, ftsText(r, industry, sector))
	return err
}

func writeTech(ctx context.Context, s *stmts, id int64, tech []string, replace bool) error {
	if replace {
		if _, err := s.techDel.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	if len(tech) == 0 {
		return nil
	}
	_, err := s.techIns.ExecContext(ctx, id, strings.Join(tech, " "))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (rs *runState) finalize(ctx context.Context) error {
	ids := make([]int64, 0, len(rs.touched))
	for id := range rs.touched {
		ids = append(ids, id)
	}
	for len(ids) > 0 {
		n := min(500, len(ids))
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}
		if _, err := rs.im.DB.W.ExecContext(ctx, `UPDATE dims SET label = COALESCE((SELECT raw FROM dim_spell s
			WHERE s.dim_id = dims.id ORDER BY s.cnt DESC, s.raw ASC LIMIT 1), label) WHERE id IN (`+db.Placeholders(n)+`)`, args...); err != nil {
			return err
		}
	}
	return Refresh(ctx, rs.im.DB)
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
