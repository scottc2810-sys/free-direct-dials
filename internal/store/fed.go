package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/normalize"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/search"
)

// Member is one registered store and its live state.
type Member struct {
	ID       int
	Name     string
	Kind     string
	CapBytes int64 // 0 = no quota (local disk, guarded separately)
	Read     bool  // serves searches, previews and claims
	Write    bool  // receives new contacts from uploads
	B        Backend
	dsn      string // decrypted connection string (reconnects)

	mir atomic.Pointer[mirror] // local copy of a remote store's per-value counts

	mu      sync.Mutex
	fails   int
	until   time.Time // skipped until then after a failure (circuit breaker)
	lastErr string
	lastOK  time.Time
	bytes   atomic.Int64
	count   atomic.Int64
}

// Status is what the admin panel shows for a store.
type Status struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Host     string `json:"host,omitempty"`
	Read     bool   `json:"read"`
	Write    bool   `json:"write"`
	CapBytes int64  `json:"cap_bytes"`
	Bytes    int64  `json:"bytes"`
	Contacts int64  `json:"contacts"`
	State    string `json:"state"` // ok | down | read-only | detached
	LastErr  string `json:"last_err,omitempty"`
	LastOK   int64  `json:"last_ok"`
	Pct      int    `json:"pct"`
}

func (m *Member) usable(now time.Time) bool {
	if m.B == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !now.Before(m.until)
}

func (m *Member) failed(now time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails++
	back := 5 * time.Second << min(m.fails-1, 6) // 5 s … 5 min
	m.until = now.Add(min(back, 5*time.Minute))
	m.lastErr = err.Error()
}

func (m *Member) ok(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails, m.until, m.lastErr, m.lastOK = 0, time.Time{}, "", now
}

// full reports a store at or past 90 % of its quota (Supabase free projects
// turn read-only at 100 %, so new contacts go elsewhere well before that).
func (m *Member) full() bool {
	return m.CapBytes > 0 && m.bytes.Load()*10 >= m.CapBytes*9
}

func (m *Member) remote() bool { return m.Kind != KindLocal }

// ---------- mirror ----------

// mirror is a remote store's dictionary counts and headline stats, kept in the
// control database. It answers totals, single-filter counts, plain dropdowns,
// labels and stats without a network call, and tells which stores can't match.
type mirror struct {
	ver   int64
	stats importer.HeadlineStats
	cnt   [13]map[string]int64 // dimension id → value_n → contacts (blank included)
	label [13]map[string]string
}

func newMirror() *mirror {
	m := &mirror{}
	for i := range m.cnt {
		m.cnt[i], m.label[i] = map[string]int64{}, map[string]string{}
	}
	return m
}

// fresh returns the mirror if it reflects the store's current data.
func (m *Member) fresh() *mirror {
	mr := m.mir.Load()
	if mr == nil || m.B == nil || mr.ver != m.B.Version() {
		return nil
	}
	return mr
}

// canMatch is false when the mirror proves no contact in the store matches f.
func canMatch(mr *mirror, f filter.Filter) bool {
	if mr == nil {
		return true
	}
	if mr.stats.Contacts == 0 || (f.HasEmail && mr.stats.WithEmail == 0) || (f.HasPhone && mr.stats.WithPhone == 0) {
		return false
	}
	for _, d := range filter.Dims {
		vals := f.Values[d.Key]
		if len(vals) == 0 {
			continue
		}
		found := false
		for _, v := range vals {
			found = found || mr.cnt[d.ID][v] > 0
		}
		if !found {
			return false
		}
	}
	return true
}

// Federation answers every contact query over the readable stores. The
// built-in store is always asked first; a remote store is only contacted when
// nothing local (mirror, cache) can answer, one store at a time, in order.
// When the first store can't fill a page, the next one is searched in the
// background: a request waits Budget for it, then answers with what it has
// and marks the result Pending so the page asks again a moment later.
type Federation struct {
	Control *db.DB // built-in database: registry, users, suppression list
	Clock   platform.Clock
	Log     *slog.Logger
	Key     []byte // encrypts stored connection strings
	DataDir string // extra SQLite stores must live here

	Cache *search.LRU // merged response cache (JSON), keyed by Version()
	SF    search.Group

	// Budget is how long a request waits for a background search before
	// answering Pending (default 1.2 s; negative = always wait: tests, CLI).
	Budget time.Duration

	mu      sync.RWMutex
	members []*Member // sorted by ID; [0] is the built-in store
	gen     atomic.Int64

	once  sync.Once
	memo  *search.LRU // remote answers keyed by store version
	bgMu  sync.Mutex
	bgRun map[string]chan struct{}
	bgSem chan struct{}
}

func (f *Federation) init() {
	f.once.Do(func() {
		f.memo = search.NewLRU(16<<20, nil)
		f.bgRun = map[string]chan struct{}{}
		f.bgSem = make(chan struct{}, 2) // background searches at once, across all visitors
		if f.Cache == nil {
			f.Cache = search.NewLRU(8<<20, nil)
		}
	})
}

func (f *Federation) now() time.Time {
	if f.Clock == nil {
		return time.Now()
	}
	return f.Clock.Now()
}

func (f *Federation) log() *slog.Logger {
	if f.Log == nil {
		return slog.Default()
	}
	return f.Log
}

type waitKey struct{}

// WithWait makes federation calls on ctx wait for every needed store instead
// of answering Pending (server-rendered pages that are cached for minutes).
func WithWait(ctx context.Context) context.Context { return context.WithValue(ctx, waitKey{}, true) }

func (f *Federation) budget(ctx context.Context) time.Duration {
	if ctx.Value(waitKey{}) != nil || f.Budget < 0 {
		return -1
	}
	if f.Budget == 0 {
		return 1200 * time.Millisecond
	}
	return f.Budget
}

// background runs fn once per key (concurrent callers share it) on a detached
// context, at most two at a time. The channel closes when it is done.
func (f *Federation) background(key string, fn func(ctx context.Context)) <-chan struct{} {
	f.init()
	f.bgMu.Lock()
	if ch, ok := f.bgRun[key]; ok {
		f.bgMu.Unlock()
		return ch
	}
	ch := make(chan struct{})
	f.bgRun[key] = ch
	f.bgMu.Unlock()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				f.log().Error("background search panicked", "key", key, "panic", p)
			}
			f.bgMu.Lock()
			delete(f.bgRun, key)
			f.bgMu.Unlock()
			close(ch)
		}()
		f.bgSem <- struct{}{}
		defer func() { <-f.bgSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fn(ctx)
	}()
	return ch
}

// await waits for ch up to the request's budget; true if it finished.
func (f *Federation) await(ctx context.Context, ch <-chan struct{}) bool {
	b := f.budget(ctx)
	if b < 0 {
		select {
		case <-ch:
			return true
		case <-ctx.Done():
			return false
		}
	}
	t := time.NewTimer(b)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Members returns a snapshot of the registered stores in order.
func (f *Federation) Members() []*Member {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]*Member(nil), f.members...)
}

// Member finds a store by id.
func (f *Federation) Member(id int) *Member {
	for _, m := range f.Members() {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (f *Federation) readable() []*Member {
	var out []*Member
	for _, m := range f.Members() {
		if m.Read && m.B != nil {
			out = append(out, m)
		}
	}
	return out
}

// Version changes whenever any store's data or the set of stores changes;
// it is part of every cache key.
func (f *Federation) Version() int64 {
	v := f.Control.DatasetVersion()*1_000_003 + f.gen.Load()*7919
	for _, m := range f.Members() {
		if m.B != nil && m.remote() {
			v += m.B.Version() * int64(m.ID+1) * 31
		}
	}
	return v
}

// bump invalidates caches after a data change in any store.
func (f *Federation) bump(ctx context.Context) {
	if _, err := f.Control.BumpDataset(ctx); err != nil {
		f.gen.Add(1)
	}
}

// failure records a remote error and returns nil (the caller carries on
// without that store); local errors, cancellations and load shedding are
// returned to the caller instead.
func (f *Federation) failure(ctx context.Context, m *Member, err error) error {
	if !m.remote() || errors.Is(err, search.ErrBusy) || ctx.Err() != nil {
		return err
	}
	m.failed(f.now(), err)
	f.log().Warn("store query failed; results exclude it for now", "store", m.Name, "err", err)
	return nil
}

// ---------- memo (remote answers, keyed by store version) ----------

func memoKey(m *Member, kind, rest string) string {
	return kind + "|" + strconv.Itoa(m.ID) + "|" + strconv.FormatInt(m.B.Version(), 10) + "|" + rest
}

func (f *Federation) memoGet(key string, v any) bool {
	f.init()
	b, ok := f.memo.Get(key)
	return ok && json.Unmarshal(b, v) == nil
}

func (f *Federation) memoSet(key string, v any) {
	f.init()
	if b, err := json.Marshal(v); err == nil {
		f.memo.Set(key, b, 30*time.Minute)
	}
}

// quickCount answers a remote store's count without a network call when the
// mirror or the memo can; ok=false means the store has to be asked.
func (f *Federation) quickCount(m *Member, flt filter.Filter) (int64, bool) {
	if mr := m.fresh(); mr != nil {
		if !canMatch(mr, flt) {
			return 0, true
		}
		if flt.IsEmpty() {
			return mr.stats.Contacts, true
		}
		if dim, ok := flt.OnlyDim(); ok {
			d, _ := filter.DimByKey(dim)
			var n int64
			for _, v := range flt.Values[dim] {
				n += mr.cnt[d.ID][v]
			}
			return n, true
		}
	}
	var n int64
	if f.memoGet(memoKey(m, "n", flt.Canonical()), &n) {
		return n, true
	}
	return 0, false
}

// searchCounts asks the given stores, one after another, in the background.
func (f *Federation) searchCounts(ms []*Member, flt filter.Filter) <-chan struct{} {
	key := "count|" + flt.Canonical()
	for _, m := range ms {
		key += "|" + strconv.Itoa(m.ID) + "." + strconv.FormatInt(m.B.Version(), 10)
	}
	return f.background(key, func(ctx context.Context) {
		for _, m := range ms {
			if _, ok := f.quickCount(m, flt); ok || !m.usable(f.now()) {
				continue
			}
			n, err := m.B.Count(ctx, flt)
			if err != nil {
				f.failure(ctx, m, err)
				continue
			}
			f.memoSet(memoKey(m, "n", flt.Canonical()), n)
		}
	})
}

// ---------- counts ----------

// State tells how complete a federated answer is.
type State struct {
	Partial []string // stores that are down: their contacts are missing
	Pending bool     // a store is still being searched in the background
}

type seg struct {
	m     *Member
	n     int64
	known bool
}

// segments returns each readable store's count for flt in order: the built-in
// store directly, remote ones from mirror/memo (known=false if they must be asked).
func (f *Federation) segments(ctx context.Context, flt filter.Filter) ([]seg, State, error) {
	var st State
	var out []seg
	now := f.now()
	for _, m := range f.readable() {
		if !m.usable(now) {
			st.Partial = append(st.Partial, m.Name)
			continue
		}
		if !m.remote() {
			n, err := m.B.Count(ctx, flt)
			if err != nil {
				return nil, st, err
			}
			out = append(out, seg{m, n, true})
			continue
		}
		n, ok := f.quickCount(m, flt)
		out = append(out, seg{m, n, ok})
	}
	return out, st, nil
}

// resolveRest searches the stores whose counts are unknown (in the
// background) and waits up to the budget; whatever is still unknown makes
// the answer Pending.
func (f *Federation) resolveRest(ctx context.Context, flt filter.Filter, segs []seg, st *State) {
	var missing []*Member
	for _, s := range segs {
		if !s.known {
			missing = append(missing, s.m)
		}
	}
	if len(missing) == 0 {
		return
	}
	f.await(ctx, f.searchCounts(missing, flt))
	for i := range segs {
		if segs[i].known {
			continue
		}
		if n, ok := f.quickCount(segs[i].m, flt); ok {
			segs[i].n, segs[i].known = n, true
		} else if !segs[i].m.usable(f.now()) {
			st.Partial = append(st.Partial, segs[i].m.Name)
		} else {
			st.Pending = true
		}
	}
}

// Count returns the number of matching contacts across stores (a lower bound
// while State.Pending).
func (f *Federation) Count(ctx context.Context, flt filter.Filter) (int64, State, error) {
	segs, st, err := f.segments(ctx, flt)
	if err != nil {
		return 0, st, err
	}
	f.resolveRest(ctx, flt, segs, &st)
	var n int64
	for _, s := range segs {
		if s.known {
			n += s.n
		}
	}
	return n, st, nil
}

// ---------- facets ----------

// FacetResult is the merged dropdown for one dimension.
type FacetResult struct {
	Dim       string              `json:"dim"`
	Values    []search.FacetValue `json:"values"`
	Truncated bool                `json:"truncated"`
	Partial   bool                `json:"partial,omitempty"`
	Pending   bool                `json:"pending,omitempty"`
}

type facetPart struct {
	Vals  []FacetCount `json:"v"`
	Trunc bool         `json:"t"`
}

// mirrorFacet answers a dropdown with no other filters from the mirror.
func mirrorFacet(mr *mirror, flt filter.Filter, dim, q string) facetPart {
	d, _ := filter.DimByKey(dim)
	qn := normalize.Dim(dim, q)
	var vals []FacetCount
	for v, n := range mr.cnt[d.ID] {
		if n > 0 && (qn == "" || strings.Contains(v, qn)) {
			vals = append(vals, FacetCount{Value: v, Label: mr.label[d.ID][v], Count: n})
		}
	}
	sort.Slice(vals, func(i, j int) bool {
		if vals[i].Count != vals[j].Count {
			return vals[i].Count > vals[j].Count
		}
		return vals[i].Value < vals[j].Value
	})
	p := facetPart{Vals: vals}
	if len(vals) > search.FacetLimit {
		p.Vals, p.Trunc = vals[:search.FacetLimit:search.FacetLimit], true
		for _, v := range vals[search.FacetLimit:] {
			for _, s := range flt.Values[dim] {
				if v.Value == s {
					p.Vals = append(p.Vals, v)
				}
			}
		}
	}
	return p
}

// Facets merges per-store value counts by normalised value. The label shown
// is the spelling from the store holding most of that value's contacts.
func (f *Federation) Facets(ctx context.Context, flt filter.Filter, dim, q string) (FacetResult, error) {
	res := FacetResult{Dim: dim, Values: []search.FacetValue{}}
	if _, ok := filter.DimByKey(dim); !ok {
		return res, fmt.Errorf("unknown dimension %q", dim)
	}
	rest := flt.Without(dim)
	fkey := dim + "|" + normalize.Fold(q) + "|" + flt.Canonical()
	var parts []facetPart
	var missing []*Member
	now := f.now()
	for _, m := range f.readable() {
		if !m.usable(now) {
			res.Partial = true
			continue
		}
		if !m.remote() {
			v, t, err := m.B.Facet(ctx, flt, dim, q, search.FacetLimit)
			if err != nil {
				return res, err
			}
			parts = append(parts, facetPart{v, t})
			continue
		}
		if mr := m.fresh(); mr != nil {
			if !canMatch(mr, rest) {
				continue
			}
			if rest.IsEmpty() {
				parts = append(parts, mirrorFacet(mr, flt, dim, q))
				continue
			}
		}
		var p facetPart
		if f.memoGet(memoKey(m, "f", fkey), &p) {
			parts = append(parts, p)
			continue
		}
		missing = append(missing, m)
	}
	if len(missing) > 0 {
		key := "facet|" + fkey
		for _, m := range missing {
			key += "|" + strconv.Itoa(m.ID) + "." + strconv.FormatInt(m.B.Version(), 10)
		}
		f.await(ctx, f.background(key, func(ctx context.Context) {
			for _, m := range missing {
				if !m.usable(f.now()) {
					continue
				}
				v, t, err := m.B.Facet(ctx, flt, dim, q, search.FacetLimit)
				if err != nil {
					f.failure(ctx, m, err)
					continue
				}
				f.memoSet(memoKey(m, "f", fkey), facetPart{v, t})
			}
		}))
		for _, m := range missing {
			var p facetPart
			if f.memoGet(memoKey(m, "f", fkey), &p) {
				parts = append(parts, p)
			} else if !m.usable(f.now()) {
				res.Partial = true
			} else {
				res.Pending = true
			}
		}
	}
	type acc struct {
		label string
		best  int64
		n     int64
	}
	merged := map[string]*acc{}
	for _, p := range parts {
		res.Truncated = res.Truncated || p.Trunc
		for _, v := range p.Vals {
			a := merged[v.Value]
			if a == nil {
				a = &acc{label: v.Label, best: v.Count}
				merged[v.Value] = a
			} else if v.Count > a.best {
				a.label, a.best = v.Label, v.Count
			}
			a.n += v.Count
		}
	}
	selected := map[string]bool{}
	for _, v := range flt.Values[dim] {
		selected[v] = true
		if merged[v] == nil {
			label := v
			if v == filter.Blank {
				label = "(Blank)"
			}
			merged[v] = &acc{label: label}
		}
	}
	for v, a := range merged {
		res.Values = append(res.Values, search.FacetValue{Value: v, Label: a.label, Count: a.n, Selected: selected[v]})
	}
	sort.Slice(res.Values, func(i, j int) bool {
		a, b := res.Values[i], res.Values[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Label < b.Label
	})
	if len(res.Values) > search.FacetLimit {
		keep := res.Values[:search.FacetLimit:search.FacetLimit]
		for _, v := range res.Values[search.FacetLimit:] {
			if v.Selected {
				keep = append(keep, v)
			}
		}
		res.Values, res.Truncated = keep, true
	}
	return res, nil
}

// ---------- preview ----------

// PreviewPage is one page of masked rows.
type PreviewPage struct {
	Total   int64        `json:"total"` // a lower bound while Pending
	Rows    []search.Row `json:"rows"`
	Page    int          `json:"page"`
	Pages   int          `json:"pages"`
	Partial bool         `json:"partial,omitempty"`
	Pending bool         `json:"pending,omitempty"`
}

// rows returns unmasked rows from one store; remote pages are memoised.
func (f *Federation) rows(ctx context.Context, m *Member, flt filter.Filter, offset, limit int) ([]search.Row, error) {
	if !m.remote() {
		return m.B.Page(ctx, flt, offset, limit)
	}
	key := memoKey(m, "r", fmt.Sprintf("%d|%d|%s", offset, limit, flt.Canonical()))
	var rows []search.Row
	if f.memoGet(key, &rows) {
		return rows, nil
	}
	rows, err := m.B.Page(ctx, flt, offset, limit)
	if err == nil {
		f.memoSet(key, rows)
	}
	return rows, err
}

// Preview returns page p (0-based) of size n of matching contacts, masked,
// ordered by store then id; maxPages caps how deep anonymous browsing goes.
// Stores are visited in order and only as far as the page reaches. When the
// stores so far are used up (or hold nothing), the next one is searched in
// the background; the page after this one is prefetched the same way.
func (f *Federation) Preview(ctx context.Context, flt filter.Filter, p, n, maxPages int) (PreviewPage, error) {
	out := PreviewPage{Rows: []search.Row{}, Page: max(0, min(p, maxPages-1))}
	segs, st, err := f.segments(ctx, flt)
	if err != nil {
		return out, err
	}
	offset, need := int64(out.Page*n), n
	stopped := false
	for i := range segs {
		if need <= 0 {
			break
		}
		s := &segs[i]
		if !s.known {
			// the stores before this one can't fill the page: search it now, in the background
			f.await(ctx, f.searchCounts([]*Member{s.m}, flt))
			if n2, ok := f.quickCount(s.m, flt); ok {
				s.n, s.known = n2, true
			} else {
				stopped = true // can't place rows past a store we haven't heard from yet
				break
			}
		}
		if offset >= s.n {
			offset -= s.n
			continue
		}
		rows, err := f.rows(ctx, s.m, flt, int(offset), need)
		if err != nil {
			if err := f.failure(ctx, s.m, err); err != nil {
				return out, err
			}
			st.Partial = append(st.Partial, s.m.Name)
			continue
		}
		offset = 0
		need -= len(rows)
		for _, r := range rows {
			r.ID = GlobalID(s.m.ID, r.ID)
			search.Mask(&r)
			out.Rows = append(out.Rows, r)
		}
	}
	// the total still needs every later store: counted in the background, one by one
	var later []*Member
	for _, s := range segs {
		if !s.known && s.m.usable(f.now()) {
			later = append(later, s.m)
		}
	}
	if len(later) > 0 {
		f.searchCounts(later, flt)
	}
	for _, s := range segs {
		if s.known {
			out.Total += s.n
		} else if s.m.usable(f.now()) {
			st.Pending = true
		} else {
			st.Partial = append(st.Partial, s.m.Name)
		}
	}
	st.Pending = st.Pending || stopped
	out.Pages = int(min((out.Total+int64(n)-1)/int64(n), int64(maxPages)))
	if st.Pending && out.Pages < maxPages {
		out.Pages++ // more may be coming: keep "Next" available
	}
	out.Partial, out.Pending = len(st.Partial) > 0, st.Pending
	if !st.Pending && out.Page+1 < out.Pages {
		f.prefetch(flt, segs, (out.Page+1)*n, n)
	}
	return out, nil
}

// prefetch warms the remote rows of the window at offset (the next page).
func (f *Federation) prefetch(flt filter.Filter, segs []seg, offset, n int) {
	off := int64(offset)
	for _, s := range segs {
		if off >= s.n {
			off -= s.n
			continue
		}
		if !s.m.remote() {
			return // the built-in store is fast enough without
		}
		m, o := s.m, int(off)
		lim := int(min(int64(n), s.n-off))
		f.background(memoKey(m, "pre", fmt.Sprintf("%d|%d|%s", o, lim, flt.Canonical())), func(ctx context.Context) {
			if _, err := f.rows(ctx, m, flt, o, lim); err != nil {
				f.failure(ctx, m, err)
			}
		})
		return
	}
}

// ---------- stats, labels, SEO (local + mirrors: no network calls) ----------

// Stats sums the headline numbers of every readable store.
func (f *Federation) Stats(ctx context.Context) importer.HeadlineStats {
	var h importer.HeadlineStats
	countries := map[string]bool{}
	remote := false
	for _, m := range f.readable() {
		if !m.remote() {
			h, _ = m.B.Stats(ctx)
			vals, _ := m.B.Values(ctx, "country", 1, 100_000)
			for _, v := range vals {
				countries[v.Value] = true
			}
			continue
		}
		mr := m.mir.Load()
		if mr == nil {
			continue
		}
		remote = true
		h.Contacts += mr.stats.Contacts
		h.WithEmail += mr.stats.WithEmail
		h.WithPhone += mr.stats.WithPhone
		h.Updated = max(h.Updated, mr.stats.Updated)
		for v, n := range mr.cnt[1] {
			if n > 0 && v != filter.Blank {
				countries[v] = true
			}
		}
	}
	if remote {
		h.Countries = int64(len(countries))
	}
	return h
}

// Labels resolves display labels for normalised values (SEO pages).
func (f *Federation) Labels(ctx context.Context, dim string, values []string) map[string]string {
	out := map[string]string{}
	d, ok := filter.DimByKey(dim)
	if !ok || len(values) == 0 {
		return out
	}
	missing := values
	for _, m := range f.readable() {
		if len(missing) == 0 {
			break
		}
		var rest []string
		switch mr := m.mir.Load(); {
		case !m.remote():
			got, err := m.B.Labels(ctx, dim, missing)
			if err != nil {
				continue
			}
			for _, v := range missing {
				if l, ok := got[v]; ok {
					out[v] = l
				} else {
					rest = append(rest, v)
				}
			}
		case mr != nil:
			for _, v := range missing {
				if l, ok := mr.label[d.ID][v]; ok {
					out[v] = l
				} else {
					rest = append(rest, v)
				}
			}
		default:
			rest = missing
		}
		missing = rest
	}
	return out
}

// TopValues merges the most common values of a dimension (SEO page candidates).
func (f *Federation) TopValues(ctx context.Context, dim string, minCount, limit int) ([]FacetCount, error) {
	d, ok := filter.DimByKey(dim)
	if !ok {
		return nil, fmt.Errorf("unknown dimension %q", dim)
	}
	acc := map[string]*FacetCount{}
	best := map[string]int64{}
	add := func(v FacetCount) {
		a := acc[v.Value]
		if a == nil {
			a = &FacetCount{Value: v.Value}
			acc[v.Value] = a
		}
		a.Count += v.Count
		if v.Count > best[v.Value] {
			best[v.Value], a.Label = v.Count, v.Label
		}
	}
	for _, m := range f.readable() {
		if !m.remote() {
			vals, err := m.B.Values(ctx, dim, 1, 1_000_000)
			if err != nil {
				return nil, err
			}
			for _, v := range vals {
				add(v)
			}
		} else if mr := m.mir.Load(); mr != nil {
			for v, n := range mr.cnt[d.ID] {
				if n > 0 && v != filter.Blank {
					add(FacetCount{Value: v, Label: mr.label[d.ID][v], Count: n})
				}
			}
		}
	}
	var out []FacetCount
	for _, a := range acc {
		if a.Count >= int64(minCount) {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// TopCombos merges country × industry counts (daily SEO job: asks each store in turn).
func (f *Federation) TopCombos(ctx context.Context, minCount, limit int) ([]Combo, error) {
	ms := f.readable()
	per := max(1, minCount/max(len(ms), 1))
	acc := map[[2]string]int64{}
	for _, m := range ms {
		if !m.usable(f.now()) {
			continue
		}
		if mr := m.fresh(); mr != nil && mr.stats.Contacts == 0 {
			continue
		}
		cs, err := m.B.Combos(ctx, per, limit*2)
		if err != nil {
			if err := f.failure(ctx, m, err); err != nil {
				return nil, err
			}
			continue
		}
		for _, c := range cs {
			acc[[2]string{c.Country, c.Industry}] += c.Count
		}
	}
	var out []Combo
	for k, n := range acc {
		if n >= int64(minCount) {
			out = append(out, Combo{Country: k[0], Industry: k[1], Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Country+"|"+out[i].Industry < out[j].Country+"|"+out[j].Industry
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---------- unlocks ----------

type userStore struct {
	id         int
	n          int64
	minB, maxB int64
}

// userStores lists the stores holding uid's unlocks (control database).
func (f *Federation) userStores(ctx context.Context, uid int64) ([]userStore, error) {
	rows, err := f.Control.R.QueryContext(ctx, `SELECT store_id, n, min_batch, max_batch FROM user_stores WHERE user_id=? AND n>0 ORDER BY store_id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []userStore
	for rows.Next() {
		var u userStore
		if err := rows.Scan(&u.id, &u.n, &u.minB, &u.maxB); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func exhaustedKey(m *Member, uid int64, flt filter.Filter) string {
	return memoKey(m, "x", strconv.FormatInt(uid, 10)+"|"+flt.Canonical())
}

// Claim takes up to n never-unlocked matching contacts for uid, recording them
// as batch: from the first store that still has some, moving to the next only
// when it is used up for this visitor and filter (remembered until its data
// changes, and the next store is warmed in the background). The ad was
// already watched, so a claim waits for every store it needs.
func (f *Federation) Claim(ctx context.Context, uid, batch int64, flt filter.Filter, n int, now int64) ([]search.Row, error) {
	f.init()
	var out []search.Row
	var firstErr error
	ms := f.readable()
	for i, m := range ms {
		if len(out) >= n {
			break
		}
		if !m.usable(f.now()) || !canMatch(m.fresh(), flt) {
			continue
		}
		var gone bool
		if f.memoGet(exhaustedKey(m, uid, flt), &gone) && gone {
			continue
		}
		want := n - len(out)
		rows, err := m.B.Claim(ctx, uid, batch, flt, want, now)
		if err != nil {
			if err := f.failure(ctx, m, err); err != nil {
				return out, err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(rows) < want { // used up for this visitor: skip it next time, warm the next store
			f.memoSet(exhaustedKey(m, uid, flt), true)
			for _, next := range ms[i+1:] {
				if next.remote() && next.usable(f.now()) {
					if _, ok := f.quickCount(next, flt); !ok {
						f.searchCounts([]*Member{next}, flt)
					}
					break
				}
			}
		}
		if len(rows) == 0 {
			continue
		}
		if _, err := f.Control.W.ExecContext(ctx, `INSERT INTO user_stores(user_id, store_id, n, min_batch, max_batch) VALUES(?,?,?,?,?)
			ON CONFLICT(user_id, store_id) DO UPDATE SET n=n+excluded.n,
			min_batch=CASE WHEN min_batch=0 OR excluded.min_batch<min_batch THEN excluded.min_batch ELSE min_batch END,
			max_batch=max(max_batch, excluded.max_batch)`, uid, m.ID, len(rows), batch, batch); err != nil {
			return out, err
		}
		for _, r := range rows {
			r.ID = GlobalID(m.ID, r.ID)
			out = append(out, r)
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Unlocked counts uid's unlocked contacts matching flt (only stores holding some).
func (f *Federation) Unlocked(ctx context.Context, uid int64, flt filter.Filter) (int64, error) {
	us, err := f.userStores(ctx, uid)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, u := range us {
		m := f.Member(u.id)
		if m == nil || !m.Read || !m.usable(f.now()) || !canMatch(m.fresh(), flt) {
			continue
		}
		n, err := m.B.Unlocked(ctx, uid, flt)
		if err != nil {
			if err := f.failure(ctx, m, err); err != nil {
				return 0, err
			}
			continue
		}
		total += n
	}
	return total, nil
}

// ViewPage returns up to limit of uid's unlocked contacts after cursor, newest
// batch first, and the next cursor (zero at the end). Only stores holding the
// user's unlocks are asked, newest batches first; a store whose newest batch
// is older than the page already collected is left for the next page.
func (f *Federation) ViewPage(ctx context.Context, uid int64, flt filter.Filter, after Cursor, onlyBatch int64, limit int, withExtra bool) ([]search.Row, Cursor, error) {
	us, err := f.userStores(ctx, uid)
	if err != nil {
		return nil, Cursor{}, err
	}
	sort.Slice(us, func(i, j int) bool {
		if us[i].maxB != us[j].maxB {
			return us[i].maxB > us[j].maxB
		}
		return us[i].id < us[j].id
	})
	type item struct {
		r     search.Row
		store int
		local int64
	}
	var all []item
	less := func(a, b item) bool {
		if a.r.Batch != b.r.Batch {
			return a.r.Batch > b.r.Batch
		}
		if a.store != b.store {
			return a.store < b.store
		}
		return a.local < b.local
	}
	more := false
	for _, u := range us {
		m := f.Member(u.id)
		if m == nil || !m.Read || !m.usable(f.now()) || !canMatch(m.fresh(), flt) {
			continue
		}
		if onlyBatch > 0 && (onlyBatch < u.minB || onlyBatch > u.maxB) {
			continue
		}
		if !after.IsZero() && u.minB > after.Batch {
			continue // everything there was on earlier pages
		}
		if len(all) >= limit && u.maxB < all[limit-1].r.Batch {
			more = true // all its rows sort after this page: the next page asks it
			continue
		}
		rows, err := m.B.ViewPage(ctx, uid, flt, after, m.ID, onlyBatch, limit+1, withExtra)
		if err != nil {
			if err := f.failure(ctx, m, err); err != nil {
				return nil, Cursor{}, err
			}
			continue
		}
		for _, r := range rows {
			all = append(all, item{r, m.ID, r.ID})
		}
		sort.Slice(all, func(i, j int) bool { return less(all[i], all[j]) })
	}
	var next Cursor
	if len(all) > limit || (more && len(all) >= limit) {
		all = all[:limit]
		last := all[limit-1]
		next = Cursor{Batch: last.r.Batch, Store: last.store, ID: last.local}
	}
	out := make([]search.Row, len(all))
	for i, it := range all {
		it.r.ID = GlobalID(it.store, it.local)
		out[i] = it.r
	}
	return out, next, nil
}

// ForgetUser removes a deleted user's unlocks from the stores holding them.
func (f *Federation) ForgetUser(ctx context.Context, uid int64) error {
	us, err := f.userStores(ctx, uid)
	if err != nil {
		return err
	}
	ids := map[int]bool{0: true}
	for _, u := range us {
		ids[u.id] = true
	}
	var errs []error
	for id := range ids {
		m := f.Member(id)
		if m == nil {
			f.Control.W.ExecContext(ctx, `DELETE FROM user_stores WHERE user_id=? AND store_id=?`, uid, id)
			continue
		}
		if m.B == nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, ErrUnavailable))
			continue
		}
		if err := m.B.ForgetUser(ctx, uid); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, err))
			continue
		}
		f.Control.W.ExecContext(ctx, `DELETE FROM user_stores WHERE user_id=? AND store_id=?`, uid, id)
	}
	return errors.Join(errs...)
}

// Prune drops dangling unlocks in every store.
func (f *Federation) Prune(ctx context.Context) error {
	var errs []error
	for _, m := range f.Members() {
		if m.B != nil {
			errs = append(errs, m.B.Prune(ctx))
		}
	}
	return errors.Join(errs...)
}

// Renormalize applies the current alias tables (normalize.Version) to every
// store once, merging dictionary values that now mean the same.
func (f *Federation) Renormalize(ctx context.Context) (int, error) {
	total := 0
	var errs []error
	for _, m := range f.Members() {
		if m.B == nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, ErrUnavailable))
			continue
		}
		n, err := m.B.Renormalize(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, err))
			continue
		}
		total += n
		if n > 0 && m.remote() {
			if err := f.syncMirror(ctx, m); err != nil {
				errs = append(errs, fmt.Errorf("%s mirror: %w", m.Name, err))
			}
		}
	}
	if total > 0 {
		f.bump(ctx)
		f.init()
		f.Cache.Purge()
	}
	return total, errors.Join(errs...)
}

// ---------- removals ----------

// Remove deletes every contact matching a removal identity in every store
// (a legal request must reach all of them). Suppression keys are written
// first, so even a partly failed removal can't be undone by a later upload.
func (f *Federation) Remove(ctx context.Context, kind, value string, now int64) (int, error) {
	total := 0
	var errs []error
	for _, m := range f.Members() {
		if m.B == nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, ErrUnavailable))
			continue
		}
		ids, err := m.B.FindByIdentity(ctx, kind, value)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, err))
			continue
		}
		if len(ids) == 0 {
			continue
		}
		keys, err := m.B.SuppressKeys(ctx, ids)
		if err == nil {
			err = importer.AddSuppress(ctx, f.Control, keys, now)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, err))
			continue
		}
		n, err := m.B.Delete(ctx, ids)
		total += n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Name, err))
		}
		if n > 0 && m.remote() {
			if err := f.syncMirror(ctx, m); err != nil {
				f.log().Warn("mirror refresh after removal", "store", m.Name, "err", err)
			}
		}
	}
	if total > 0 {
		f.bump(ctx)
	}
	return total, errors.Join(errs...)
}

// ---------- mirror sync ----------

// syncMirror copies a remote store's per-value counts and stats into the
// control database (after imports, removals, or when its data changed).
func (f *Federation) syncMirror(ctx context.Context, m *Member) error {
	if !m.remote() || m.B == nil {
		return nil
	}
	ver := m.B.Version()
	dims, err := m.B.Dims(ctx)
	if err != nil {
		return err
	}
	st, err := m.B.Stats(ctx)
	if err != nil {
		return err
	}
	mr := newMirror()
	mr.ver, mr.stats = ver, st
	for _, d := range dims {
		if d.Dim >= 1 && d.Dim <= 12 {
			mr.cnt[d.Dim][d.Value], mr.label[d.Dim][d.Value] = d.Count, d.Label
		}
	}
	sb, _ := json.Marshal(st)
	err = f.Control.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM store_dims WHERE store_id=?`, m.ID); err != nil {
			return err
		}
		for start := 0; start < len(dims); start += 200 {
			chunk := dims[start:min(start+200, len(dims))]
			args := make([]any, 0, len(chunk)*5)
			for _, d := range chunk {
				args = append(args, m.ID, d.Dim, d.Value, d.Label, d.Count)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO store_dims(store_id, dim, value_n, label, cnt) VALUES `+
				strings.TrimSuffix(strings.Repeat("(?,?,?,?,?),", len(chunk)), ","), args...); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE stores SET stats=?, mirror_ver=?, contacts=? WHERE id=?`, string(sb), ver, st.Contacts, m.ID)
		return err
	})
	if err != nil {
		return err
	}
	m.mir.Store(mr)
	m.count.Store(st.Contacts)
	return nil
}

// loadMirrors reads every stored mirror (startup).
func (f *Federation) loadMirrors(ctx context.Context) error {
	byID := map[int]*mirror{}
	rows, err := f.Control.R.QueryContext(ctx, `SELECT id, stats, mirror_ver FROM stores WHERE kind<>'local'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		var stats string
		var ver int64
		if err := rows.Scan(&id, &stats, &ver); err != nil {
			rows.Close()
			return err
		}
		mr := newMirror()
		mr.ver = ver
		json.Unmarshal([]byte(stats), &mr.stats)
		byID[id] = mr
	}
	rows.Close()
	rows, err = f.Control.R.QueryContext(ctx, `SELECT store_id, dim, value_n, label, cnt FROM store_dims`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, dim int
		var v, l string
		var n int64
		if err := rows.Scan(&id, &dim, &v, &l, &n); err != nil {
			return err
		}
		if mr := byID[id]; mr != nil && dim >= 1 && dim <= 12 {
			mr.cnt[dim][v], mr.label[dim][v] = n, l
		}
	}
	for _, m := range f.Members() {
		if mr := byID[m.ID]; mr != nil && m.remote() {
			m.mir.Store(mr)
		}
	}
	return rows.Err()
}

// rebuildUserStores records which users hold unlocks in a (re-)attached store.
func (f *Federation) rebuildUserStores(ctx context.Context, m *Member) error {
	sum, err := m.B.UnlockSummary(ctx)
	if err != nil || len(sum) == 0 {
		return err
	}
	return f.Control.Tx(ctx, func(tx *sql.Tx) error {
		for _, u := range sum {
			if _, err := tx.ExecContext(ctx, `INSERT INTO user_stores(user_id, store_id, n, min_batch, max_batch) VALUES(?,?,?,?,?)
				ON CONFLICT(user_id, store_id) DO UPDATE SET n=excluded.n, min_batch=excluded.min_batch, max_batch=excluded.max_batch`,
				u.UserID, m.ID, u.N, u.MinBatch, u.MaxBatch); err != nil {
				return err
			}
		}
		return nil
	})
}
