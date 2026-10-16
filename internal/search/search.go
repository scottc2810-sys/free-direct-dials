// Package search answers counts, faceted dropdowns and masked previews over
// the narrow contacts table, with caching and load shedding.
package search

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/normalize"
)

var ErrBusy = errors.New("server busy")

const (
	FacetLimit   = 300
	PreviewLimit = 10
)

type Service struct {
	DB           *db.DB
	Cache        *LRU // response cache (JSON bytes)
	SF           Group
	QueryTimeout time.Duration
	WaitTimeout  time.Duration

	sem    chan struct{}
	values *LRU // dim value_n → id
	labels *LRU // dim id → label
}

func New(d *db.DB, cache *LRU, readers int) *Service {
	if readers <= 0 {
		readers = 4
	}
	return &Service{
		DB: d, Cache: cache, QueryTimeout: 3 * time.Second, WaitTimeout: 2 * time.Second,
		sem:    make(chan struct{}, readers*2),
		values: NewLRU(8<<20, nil),
		labels: NewLRU(8<<20, nil),
	}
}

// acquire bounds concurrent DB reads; callers past the queue get ErrBusy (503).
func (s *Service) acquire(ctx context.Context) (context.Context, func(), error) {
	t := time.NewTimer(s.WaitTimeout)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
	case <-t.C:
		return nil, nil, ErrBusy
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	qctx, cancel := context.WithTimeout(ctx, s.QueryTimeout)
	return qctx, func() { cancel(); <-s.sem }, nil
}

// Resolved is a filter with dimension values mapped to dictionary ids.
type Resolved struct {
	F     filter.Filter
	ids   map[string][]int64
	empty map[string]bool // dimension whose every value is unknown → matches nothing
	fts   string
}

// Resolve maps folded values to ids. Unknown values match nothing (never an error).
func (s *Service) Resolve(ctx context.Context, f filter.Filter) (*Resolved, error) {
	r := &Resolved{F: f, ids: map[string][]int64{}, empty: map[string]bool{}, fts: filter.FTSQuery(f.Keyword)}
	ver := s.DB.DatasetVersion()
	for _, d := range filter.Dims {
		vals := f.Values[d.Key]
		if len(vals) == 0 {
			continue
		}
		var ids []int64
		var missing []string
		for _, v := range vals {
			if v == filter.Blank {
				ids = append(ids, int64(d.ID))
				continue
			}
			if b, ok := s.values.Get(valKey(ver, d.ID, v)); ok {
				if id := int64(binary.LittleEndian.Uint64(b)); id > 0 {
					ids = append(ids, id)
				}
				continue
			}
			missing = append(missing, v)
		}
		if len(missing) > 0 {
			args := []any{d.ID}
			for _, v := range missing {
				args = append(args, v)
			}
			rows, err := s.DB.R.QueryContext(ctx, `SELECT value_n, id FROM dims WHERE dim=? AND value_n IN (`+db.Placeholders(len(missing))+`)`, args...)
			if err != nil {
				return nil, err
			}
			found := map[string]int64{}
			for rows.Next() {
				var v string
				var id int64
				if err := rows.Scan(&v, &id); err != nil {
					rows.Close()
					return nil, err
				}
				found[v] = id
			}
			rows.Close()
			for _, v := range missing {
				id := found[v]
				b := make([]byte, 8)
				binary.LittleEndian.PutUint64(b, uint64(id))
				s.values.Set(valKey(ver, d.ID, v), b, 10*time.Minute)
				if id > 0 {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) == 0 {
			r.empty[d.Key] = true
		} else {
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			r.ids[d.Key] = ids
		}
	}
	return r, nil
}

func valKey(ver int64, dim int, v string) string {
	return strconv.FormatInt(ver, 10) + "|" + strconv.Itoa(dim) + "|" + v
}

// Where renders the SQL predicate over alias "c" (the contacts table),
// optionally ignoring one dimension (facets ignore their own selection).
func (r *Resolved) Where(exclude string) (string, []any) {
	var conds []string
	var args []any
	for _, d := range filter.Dims {
		if d.Key == exclude {
			continue
		}
		if r.empty[d.Key] {
			return "0", nil
		}
		ids := r.ids[d.Key]
		if len(ids) == 0 {
			continue
		}
		if len(ids) == 1 {
			conds = append(conds, "c."+d.Key+"=?")
		} else {
			conds = append(conds, "c."+d.Key+" IN ("+db.Placeholders(len(ids))+")")
		}
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if r.fts != "" {
		conds = append(conds, "c.id IN (SELECT rowid FROM fts WHERE fts MATCH ?)")
		args = append(args, r.fts)
	}
	if q := filter.TechFTSQuery(r.F.Tech); q != "" {
		conds = append(conds, "c.id IN (SELECT rowid FROM tech_fts WHERE tech_fts MATCH ?)")
		args = append(args, q)
	}
	if r.F.HasEmail {
		conds = append(conds, "c.has_email=1")
	}
	if r.F.HasPhone {
		conds = append(conds, "c.has_phone=1")
	}
	if len(conds) == 0 {
		return "1", nil
	}
	return strings.Join(conds, " AND "), args
}

// Impossible reports a filter that can match nothing (unknown values).
func (r *Resolved) Impossible() bool { return len(r.empty) > 0 }

// Count returns the exact number of matching contacts.
func (s *Service) Count(ctx context.Context, r *Resolved) (int64, error) {
	if r.Impossible() {
		return 0, nil
	}
	if r.F.IsEmpty() {
		return importer.LoadStats(ctx, s.DB.R).Contacts, nil
	}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	if dim, ok := r.F.OnlyDim(); ok { // fast path: per-value counts are precomputed
		ids := r.ids[dim]
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		var n int64
		err := s.DB.R.QueryRowContext(ctx, `SELECT COALESCE(SUM(cnt),0) FROM dims WHERE id IN (`+db.Placeholders(len(ids))+`)`, args...).Scan(&n)
		return n, err
	}
	where, args := r.Where("")
	var n int64
	err = s.DB.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts c WHERE `+where, args...).Scan(&n)
	return n, err
}

type FacetValue struct {
	Value    string `json:"v"`
	Label    string `json:"l"`
	Count    int64  `json:"n"`
	Selected bool   `json:"s,omitempty"`
}

type FacetResult struct {
	Dim       string       `json:"dim"`
	Values    []FacetValue `json:"values"`
	Truncated bool         `json:"truncated"`
}

// Facets returns value counts for one dimension, ignoring that dimension's own
// selection but honouring every other filter. q narrows to matching labels.
func (s *Service) Facets(ctx context.Context, r *Resolved, dim, q string) (FacetResult, error) {
	d, ok := filter.DimByKey(dim)
	if !ok {
		return FacetResult{}, fmt.Errorf("unknown dimension %q", dim)
	}
	res := FacetResult{Dim: dim, Values: []FacetValue{}}
	selected := map[int64]bool{}
	for _, id := range r.ids[dim] {
		selected[id] = true
	}
	var like string
	if qn := normalize.Dim(dim, q); qn != "" {
		like = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(qn) + "%"
	}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return res, err
	}
	defer release()

	counts := map[int64]int64{}
	var order []int64
	rest := r.F.Without(dim)
	others := &Resolved{F: rest, ids: map[string][]int64{}, empty: map[string]bool{}, fts: r.fts}
	for k, v := range r.ids {
		if k != dim {
			others.ids[k] = v
		}
	}
	for k := range r.empty {
		if k != dim {
			others.empty[k] = true
		}
	}

	if others.Impossible() {
		// every value has count 0; still show the selection
	} else if rest.IsEmpty() {
		q := `SELECT id, cnt FROM dims WHERE dim=? AND cnt>0`
		args := []any{d.ID}
		if like != "" {
			q += ` AND value_n LIKE ? ESCAPE '\'`
			args = append(args, like)
		}
		q += ` ORDER BY cnt DESC, id LIMIT ?`
		args = append(args, FacetLimit+1)
		if err := collect(ctx, s.DB.R, q, args, counts, &order); err != nil {
			return res, err
		}
	} else {
		where, args := others.Where(dim)
		q := `SELECT c.` + d.Key + `, COUNT(*) FROM contacts c WHERE ` + where
		if like != "" {
			q += ` AND c.` + d.Key + ` IN (SELECT id FROM dims WHERE dim=? AND value_n LIKE ? ESCAPE '\')`
			args = append(args, d.ID, like)
		}
		q += ` GROUP BY 1 ORDER BY 2 DESC LIMIT ?`
		args = append(args, FacetLimit+1)
		if err := collect(ctx, s.DB.R, q, args, counts, &order); err != nil {
			return res, err
		}
	}
	if len(order) > FacetLimit {
		res.Truncated = true
		delete(counts, order[FacetLimit])
		order = order[:FacetLimit]
	}
	// selected values are always present, with their real count under the other filters
	var missing []int64
	for id := range selected {
		if _, ok := counts[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 && !others.Impossible() {
		sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
		margs := make([]any, len(missing))
		for i, id := range missing {
			margs[i] = id
		}
		var q string
		var args []any
		if rest.IsEmpty() {
			q, args = `SELECT id, cnt FROM dims WHERE id IN (`+db.Placeholders(len(missing))+`)`, margs
		} else {
			where, wargs := others.Where(dim)
			q = `SELECT c.` + d.Key + `, COUNT(*) FROM contacts c WHERE ` + where + ` AND c.` + d.Key + ` IN (` + db.Placeholders(len(missing)) + `) GROUP BY 1`
			args = append(wargs, margs...)
		}
		var extra []int64
		if err := collect(ctx, s.DB.R, q, args, counts, &extra); err != nil {
			return res, err
		}
	}
	for _, id := range missing {
		if _, ok := counts[id]; !ok {
			counts[id] = 0
		}
		order = append(order, id)
	}
	labels, values, err := s.dimLabels(ctx, order)
	if err != nil {
		return res, err
	}
	for _, id := range order {
		res.Values = append(res.Values, FacetValue{Value: values[id], Label: labels[id], Count: counts[id], Selected: selected[id]})
	}
	sort.SliceStable(res.Values, func(i, j int) bool {
		a, b := res.Values[i], res.Values[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Label < b.Label
	})
	return res, nil
}

func collect(ctx context.Context, q db.Querier, query string, args []any, into map[int64]int64, order *[]int64) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		into[id] = n
		*order = append(*order, id)
	}
	return rows.Err()
}

// dimLabels returns label and value_n for dictionary ids (cached per version).
func (s *Service) dimLabels(ctx context.Context, ids []int64) (map[int64]string, map[int64]string, error) {
	labels := make(map[int64]string, len(ids))
	values := make(map[int64]string, len(ids))
	ver := strconv.FormatInt(s.DB.DatasetVersion(), 10)
	var missing []any
	for _, id := range ids {
		if _, done := labels[id]; done {
			continue
		}
		if b, ok := s.labels.Get(ver + "|" + strconv.FormatInt(id, 10)); ok {
			v, l, _ := strings.Cut(string(b), "\x00")
			values[id], labels[id] = v, l
			continue
		}
		labels[id] = ""
		missing = append(missing, id)
	}
	for len(missing) > 0 {
		n := min(500, len(missing))
		chunk := missing[:n]
		missing = missing[n:]
		rows, err := s.DB.R.QueryContext(ctx, `SELECT id, value_n, label FROM dims WHERE id IN (`+db.Placeholders(n)+`)`, chunk...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id int64
			var v, l string
			if err := rows.Scan(&id, &v, &l); err != nil {
				rows.Close()
				return nil, nil, err
			}
			values[id], labels[id] = v, l
			s.labels.Set(ver+"|"+strconv.FormatInt(id, 10), []byte(v+"\x00"+l), 30*time.Minute)
		}
		rows.Close()
	}
	return labels, values, nil
}

// Row is one contact as shown in the UI and exported to CSV.
type Row struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	Company   string `json:"company"`
	Website   string `json:"website"`
	LinkedIn  string `json:"linkedin"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	City      string `json:"city"`
	State     string `json:"state"`
	Country   string `json:"country"`
	Industry  string `json:"industry"`
	Sector    string `json:"sector"`
	CType     string `json:"ctype"`
	Seniority string `json:"seniority"`
	Division  string `json:"division"`
	Decision  string `json:"decision"`
	CSize     string `json:"csize"`
	Revenue   string `json:"revenue"`
	EConf     string `json:"econf"`
	Batch     int64  `json:"batch,omitempty"`
	New       bool   `json:"new,omitempty"`
	Masked    bool   `json:"masked,omitempty"`

	Extra map[string]string `json:"-"`
}

// FetchRows loads display rows for ids, preserving the order of ids.
func (s *Service) FetchRows(ctx context.Context, q db.Querier, ids []int64, withExtra bool) ([]Row, error) {
	if len(ids) == 0 {
		return []Row{}, nil
	}
	order := append([]int64(nil), ids...)
	byID := make(map[int64]*Row, len(ids))
	var dimIDs []int64
	dimsOf := map[int64][12]int64{}
	for len(ids) > 0 {
		n := min(500, len(ids))
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, n)
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := q.QueryContext(ctx, `SELECT c.id, c.country, c.state, c.city, c.industry, c.sector, c.ctype, c.seniority,
			c.division, c.decision, c.csize, c.revenue, c.econf, i.name, i.title, i.company, i.website, i.linkedin, i.email, i.phone, i.extra
			FROM contacts c JOIN contact_info i ON i.id=c.id WHERE c.id IN (`+db.Placeholders(n)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r Row
			var dm [12]int64
			var extra []byte
			if err := rows.Scan(&r.ID, &dm[0], &dm[1], &dm[2], &dm[3], &dm[4], &dm[5], &dm[6], &dm[7], &dm[8], &dm[9], &dm[10], &dm[11],
				&r.Name, &r.Title, &r.Company, &r.Website, &r.LinkedIn, &r.Email, &r.Phone, &extra); err != nil {
				rows.Close()
				return nil, err
			}
			if withExtra {
				r.Extra = importer.DecodeExtra(extra)
			}
			byID[r.ID] = &r
			dimsOf[r.ID] = dm
			dimIDs = append(dimIDs, dm[:]...)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	labels, _, err := s.dimLabels(ctx, dimIDs)
	if err != nil {
		return nil, err
	}
	lab := func(id int64) string {
		if id <= 12 {
			return ""
		}
		return labels[id]
	}
	return orderRows(order, byID, dimsOf, lab), nil
}

func orderRows(order []int64, byID map[int64]*Row, dimsOf map[int64][12]int64, lab func(int64) string) []Row {
	out := make([]Row, 0, len(byID))
	for _, id := range order {
		r, ok := byID[id]
		if !ok {
			continue // deleted between id selection and fetch
		}
		dm := dimsOf[id]
		r.Country, r.State, r.City = lab(dm[0]), lab(dm[1]), lab(dm[2])
		r.Industry, r.Sector, r.CType = lab(dm[3]), lab(dm[4]), lab(dm[5])
		r.Seniority, r.Division, r.Decision = lab(dm[6]), lab(dm[7]), lab(dm[8])
		r.CSize, r.Revenue, r.EConf = lab(dm[9]), lab(dm[10]), lab(dm[11])
		out = append(out, *r)
	}
	return out
}

// Preview returns the total and the first PreviewLimit matching contacts, masked.
func (s *Service) Preview(ctx context.Context, r *Resolved) (int64, []Row, error) {
	total, err := s.Count(ctx, r)
	if err != nil || total == 0 {
		return total, []Row{}, err
	}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer release()
	where, args := r.Where("")
	ids, err := queryIDs(ctx, s.DB.R, `SELECT c.id FROM contacts c WHERE `+where+` ORDER BY c.id LIMIT ?`, append(args, PreviewLimit)...)
	if err != nil {
		return 0, nil, err
	}
	rows, err := s.FetchRows(ctx, s.DB.R, ids, false)
	if err != nil {
		return 0, nil, err
	}
	for i := range rows {
		Mask(&rows[i])
	}
	return total, rows, nil
}

func queryIDs(ctx context.Context, q db.Querier, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// QueryIDs is exported for the unlock service.
func QueryIDs(ctx context.Context, q db.Querier, query string, args ...any) ([]int64, error) {
	return queryIDs(ctx, q, query, args...)
}

const dot = "\xe2\x80\xa2" // •

// Mask hides contact details in previews. Masking happens server-side: real
// values never reach an anonymous browser.
func Mask(r *Row) {
	r.Masked = true
	r.Email = MaskEmail(r.Email)
	r.Phone = MaskPhone(r.Phone)
	if r.LinkedIn != "" {
		r.LinkedIn = "linkedin.com/in/" + strings.Repeat(dot, 6)
	}
}

func MaskEmail(e string) string {
	at := strings.LastIndexByte(e, '@')
	if at <= 0 {
		return ""
	}
	local, dom := e[:at], e[at+1:]
	tld := ""
	if i := strings.LastIndexByte(dom, '.'); i > 0 {
		tld = dom[i:]
	}
	return firstRune(local) + strings.Repeat(dot, 3) + "@" + firstRune(dom) + strings.Repeat(dot, 3) + tld
}

func MaskPhone(p string) string {
	d := normalize.PhoneDigits(p)
	if len(d) < 6 {
		return ""
	}
	prefix := ""
	if strings.HasPrefix(strings.TrimSpace(p), "+") {
		prefix = "+"
	}
	return prefix + strings.Repeat(dot, 3) + " " + strings.Repeat(dot, 3) + " " + strings.Repeat(dot, 2) + d[len(d)-2:]
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}
