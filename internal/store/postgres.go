package store

import (
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/normalize"
	"markemind/dials/internal/search"
)

//go:embed pgmigrations/*.sql
var pgMigrations embed.FS

// Postgres is a contacts store in a Postgres database, sized for Supabase's
// free tier: a small pool (the session pooler allows ~15 clients), queries
// without named prepared statements (works through PgBouncer/Supavisor in
// either mode), bounded concurrency and per-query timeouts.
type Postgres struct {
	pool   *pgxpool.Pool
	ver    atomic.Int64
	sem    chan struct{}
	values *search.LRU // version|dim|value_n → id
	labels *search.LRU // version|id → value_n \x00 label
	ident  string

	QueryTimeout time.Duration
	WaitTimeout  time.Duration

	statsMu  sync.Mutex
	statsVer int64
	stats    importer.HeadlineStats
}

const pgMaxConns = 4

// ensureTLS makes TLS mandatory unless the DSN already says how to use it.
func ensureTLS(dsn string) string {
	if strings.Contains(dsn, "sslmode=") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if strings.Contains(dsn, "?") {
			return dsn + "&sslmode=require"
		}
		return dsn + "?sslmode=require"
	}
	return dsn + " sslmode=require"
}

// redactErr strips the password from driver errors before they reach logs or the UI.
func redactErr(dsn string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if u, perr := url.Parse(dsn); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "*****")
			msg = strings.ReplaceAll(msg, url.QueryEscape(pw), "*****")
		}
	}
	for _, kv := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(kv, "password="); ok && v != "" {
			msg = strings.ReplaceAll(msg, v, "*****")
		}
	}
	return errors.New(msg)
}

func pgConfig(dsn string) (*pgxpool.Config, error) {
	dsn = ensureTLS(strings.TrimSpace(dsn))
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, redactErr(dsn, fmt.Errorf("connection string: %w", err))
	}
	cc := cfg.ConnConfig
	host := cc.Host
	local := host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "/")
	if cc.TLSConfig == nil && !local {
		return nil, errors.New("this connection would not be encrypted: remove sslmode=disable (Supabase strings work as copied)")
	}
	if !local { // "prefer" falls back to plain text; never allow that over the internet
		fb := cc.Fallbacks[:0]
		for _, f := range cc.Fallbacks {
			if f.TLSConfig != nil {
				fb = append(fb, f)
			}
		}
		cc.Fallbacks = fb
	}
	cfg.MaxConns = pgMaxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	cc.ConnectTimeout = 10 * time.Second
	cc.DefaultQueryExecMode = pgx.QueryExecModeExec
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	cc.RuntimeParams["application_name"] = "free-direct-dials"
	return cfg, nil
}

// OpenPostgres connects, creates or upgrades the fdd schema and loads state.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgConfig(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, redactErr(dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, redactErr(dsn, fmt.Errorf("can't connect: %w", err))
	}
	p := &Postgres{pool: pool, sem: make(chan struct{}, pgMaxConns*2), values: search.NewLRU(4<<20, nil), labels: search.NewLRU(4<<20, nil),
		QueryTimeout: 8 * time.Second, WaitTimeout: 3 * time.Second}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, redactErr(dsn, err)
	}
	if err := p.loadVersion(ctx); err != nil {
		pool.Close()
		return nil, redactErr(dsn, err)
	}
	pool.QueryRow(ctx, `SELECT v FROM fdd.meta WHERE k='store_uuid'`).Scan(&p.ident)
	return p, nil
}

// ProbePostgres reports on a database without changing it.
func ProbePostgres(ctx context.Context, dsn string) (Probe, error) {
	var pr Probe
	cfg, err := pgConfig(dsn)
	if err != nil {
		return pr, err
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return pr, redactErr(dsn, err)
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx, `SELECT version(), pg_database_size(current_database())`).Scan(&pr.Server, &pr.Bytes); err != nil {
		return pr, redactErr(dsn, fmt.Errorf("can't connect: %w", err))
	}
	if i := strings.Index(pr.Server, " on "); i > 0 {
		pr.Server = pr.Server[:i]
	}
	pool.QueryRow(ctx, `SELECT to_regclass('fdd.contacts') IS NOT NULL`).Scan(&pr.HasSchema)
	if pr.HasSchema {
		var raw string
		pool.QueryRow(ctx, `SELECT v FROM fdd.meta WHERE k='stats'`).Scan(&raw)
		var h importer.HeadlineStats
		json.Unmarshal([]byte(raw), &h)
		pr.Contacts = h.Contacts
	}
	return pr, nil
}

func (p *Postgres) migrate(ctx context.Context) error {
	entries, err := pgMigrations.ReadDir("pgmigrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{
		`SELECT pg_advisory_xact_lock(7212003)`, // two app instances never migrate at once
		`CREATE SCHEMA IF NOT EXISTS fdd`,
		`CREATE TABLE IF NOT EXISTS fdd.meta(k text PRIMARY KEY, v text NOT NULL)`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("prepare schema: %w", err)
		}
	}
	current := 0
	var v string
	if err := tx.QueryRow(ctx, `SELECT v FROM fdd.meta WHERE k='schema_version'`).Scan(&v); err == nil {
		current, _ = strconv.Atoi(v)
	}
	for _, e := range entries {
		num, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil || num <= current {
			continue
		}
		body, err := pgMigrations.ReadFile("pgmigrations/" + e.Name())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body), pgx.QueryExecModeSimpleProtocol); err != nil { // multi-statement
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO fdd.meta(k, v) VALUES('schema_version', $1) ON CONFLICT (k) DO UPDATE SET v=excluded.v`, strconv.Itoa(num)); err != nil {
			return err
		}
		current = num
	}
	return tx.Commit(ctx)
}

func (p *Postgres) loadVersion(ctx context.Context) error {
	var v int64
	if err := p.pool.QueryRow(ctx, `SELECT v::bigint FROM fdd.meta WHERE k='dataset_version'`).Scan(&v); err != nil {
		return err
	}
	p.ver.Store(v)
	return nil
}

func (p *Postgres) bump(ctx context.Context) error {
	var v int64
	err := p.pool.QueryRow(ctx, `UPDATE fdd.meta SET v = (v::bigint + 1)::text WHERE k='dataset_version' RETURNING v::bigint`).Scan(&v)
	if err == nil {
		p.ver.Store(v)
	}
	return err
}

func (p *Postgres) Kind() string                        { return KindPostgres }
func (p *Postgres) Version() int64                      { return p.ver.Load() }
func (p *Postgres) Identity(ctx context.Context) string { return "pg:" + p.ident }
func (p *Postgres) Close() error                        { p.pool.Close(); return nil }

// acquire bounds concurrent queries; past the queue callers get ErrBusy (503).
func (p *Postgres) acquire(ctx context.Context) (context.Context, func(), error) {
	t := time.NewTimer(p.WaitTimeout)
	defer t.Stop()
	select {
	case p.sem <- struct{}{}:
	case <-t.C:
		return nil, nil, search.ErrBusy
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	qctx, cancel := context.WithTimeout(ctx, p.QueryTimeout)
	return qctx, func() { cancel(); <-p.sem }, nil
}

// ---------- filters ----------

type pgArgs []any

func (a *pgArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

type pgResolved struct {
	f     filter.Filter
	ids   map[string][]int64
	empty map[string]bool
	kw    string // tsquery input
	tech  string
}

func (r *pgResolved) impossible() bool { return len(r.empty) > 0 }

func tsLexeme(w string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(w, `\`, `\\`), "'", "''") + "'"
}

// tsPrefixAll renders "every word prefix-matches" as tsquery input. Lexemes are
// quoted literally (no parser, no dictionary), mirroring the stored tokens.
func tsPrefixAll(words []string) string {
	parts := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" {
			parts = append(parts, tsLexeme(w)+":*")
		}
	}
	return strings.Join(parts, " & ")
}

func techTSQuery(terms []string) string {
	var ors []string
	for _, g := range filter.TechQuery(terms) {
		if q := tsPrefixAll(g); q != "" {
			ors = append(ors, "("+q+")")
		}
	}
	return strings.Join(ors, " | ")
}

func (p *Postgres) resolve(ctx context.Context, f filter.Filter) (*pgResolved, error) {
	r := &pgResolved{f: f, ids: map[string][]int64{}, empty: map[string]bool{}, tech: techTSQuery(f.Tech)}
	if f.Keyword != "" {
		r.kw = tsPrefixAll(strings.Split(f.Keyword, " "))
	}
	ver := strconv.FormatInt(p.ver.Load(), 10)
	var qd []int16
	var qv []string
	for _, d := range filter.Dims {
		for _, v := range f.Values[d.Key] {
			if v == filter.Blank {
				r.ids[d.Key] = append(r.ids[d.Key], int64(d.ID))
				continue
			}
			if b, ok := p.values.Get(ver + "|" + strconv.Itoa(d.ID) + "|" + v); ok {
				if id := int64(binary.LittleEndian.Uint64(b)); id > 0 {
					r.ids[d.Key] = append(r.ids[d.Key], id)
				}
				continue
			}
			qd, qv = append(qd, int16(d.ID)), append(qv, v)
		}
	}
	if len(qv) > 0 {
		rows, err := p.pool.Query(ctx, `SELECT d.dim, d.value_n, d.id FROM fdd.dims d
			JOIN unnest($1::smallint[], $2::text[]) AS q(dim, v) ON d.dim = q.dim AND d.value_n = q.v`, qd, qv)
		if err != nil {
			return nil, err
		}
		found := map[string]int64{}
		for rows.Next() {
			var dim int16
			var v string
			var id int64
			if err := rows.Scan(&dim, &v, &id); err != nil {
				rows.Close()
				return nil, err
			}
			found[strconv.Itoa(int(dim))+"|"+v] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for i, v := range qv {
			k := strconv.Itoa(int(qd[i])) + "|" + v
			id := found[k]
			b := make([]byte, 8)
			binary.LittleEndian.PutUint64(b, uint64(id))
			p.values.Set(ver+"|"+k, b, 10*time.Minute)
			if id > 0 {
				key := filter.Dims[qd[i]-1].Key
				r.ids[key] = append(r.ids[key], id)
			}
		}
	}
	for _, d := range filter.Dims {
		if len(f.Values[d.Key]) > 0 && len(r.ids[d.Key]) == 0 {
			r.empty[d.Key] = true
		}
		ids := r.ids[d.Key]
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return r, nil
}

// where renders the predicate over alias c (fdd.contacts), optionally ignoring one dimension.
func (r *pgResolved) where(exclude string, a *pgArgs) string {
	var conds []string
	for _, d := range filter.Dims {
		if d.Key == exclude {
			continue
		}
		if r.empty[d.Key] {
			return "false"
		}
		ids := r.ids[d.Key]
		switch len(ids) {
		case 0:
		case 1:
			conds = append(conds, "c."+d.Key+" = "+a.add(ids[0])+"::bigint")
		default:
			conds = append(conds, "c."+d.Key+" = ANY("+a.add(ids)+"::bigint[])")
		}
	}
	if r.kw != "" {
		conds = append(conds, "c.id IN (SELECT id FROM fdd.contact_info WHERE kw @@ "+a.add(r.kw)+"::tsquery)")
	}
	if r.tech != "" {
		conds = append(conds, "c.id IN (SELECT id FROM fdd.contact_info WHERE tech @@ "+a.add(r.tech)+"::tsquery)")
	}
	if r.f.HasEmail {
		conds = append(conds, "c.has_email")
	}
	if r.f.HasPhone {
		conds = append(conds, "c.has_phone")
	}
	if len(conds) == 0 {
		return "true"
	}
	return strings.Join(conds, " AND ")
}

// ---------- reads ----------

func (p *Postgres) Count(ctx context.Context, f filter.Filter) (int64, error) {
	if f.IsEmpty() {
		st, err := p.Stats(ctx)
		return st.Contacts, err
	}
	ctx, release, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	r, err := p.resolve(ctx, f)
	if err != nil || r.impossible() {
		return 0, err
	}
	var n int64
	if dim, ok := f.OnlyDim(); ok { // per-value counts are precomputed
		err = p.pool.QueryRow(ctx, `SELECT COALESCE(sum(cnt), 0)::bigint FROM fdd.dims WHERE id = ANY($1::bigint[])`, r.ids[dim]).Scan(&n)
		return n, err
	}
	var a pgArgs
	err = p.pool.QueryRow(ctx, `SELECT count(*) FROM fdd.contacts c WHERE `+r.where("", &a), a...).Scan(&n)
	return n, err
}

func likePattern(dim, q string) string {
	qn := normalize.Dim(dim, q)
	if qn == "" {
		return ""
	}
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(qn) + "%"
}

func (p *Postgres) Facet(ctx context.Context, f filter.Filter, dim, q string, limit int) ([]FacetCount, bool, error) {
	d, ok := filter.DimByKey(dim)
	if !ok {
		return nil, false, fmt.Errorf("unknown dimension %q", dim)
	}
	ctx, release, err := p.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	r, err := p.resolve(ctx, f)
	if err != nil {
		return nil, false, err
	}
	like := likePattern(dim, q)
	rest := f.Without(dim)
	othersImpossible := false
	for k := range r.empty {
		if k != dim {
			othersImpossible = true
		}
	}
	type item struct {
		id   int64
		v, l string
		n    int64
	}
	var items []item
	seen := map[int64]bool{}
	scan := func(rows pgx.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.v, &it.l, &it.n); err != nil {
				return err
			}
			if !seen[it.id] {
				seen[it.id] = true
				items = append(items, it)
			}
		}
		return rows.Err()
	}
	if !othersImpossible {
		var a pgArgs
		var sqlq string
		if rest.IsEmpty() {
			sqlq = `SELECT id, value_n, label, cnt FROM fdd.dims WHERE dim = ` + a.add(int16(d.ID)) + ` AND cnt > 0`
			if like != "" {
				sqlq += ` AND value_n LIKE ` + a.add(like) + ` ESCAPE '\'`
			}
			sqlq += ` ORDER BY cnt DESC, id LIMIT ` + a.add(limit+1)
		} else {
			inner := `SELECT c.` + d.Key + ` AS id, count(*) AS n FROM fdd.contacts c WHERE ` + r.where(dim, &a)
			if like != "" {
				inner += ` AND c.` + d.Key + ` IN (SELECT id FROM fdd.dims WHERE dim = ` + a.add(int16(d.ID)) + ` AND value_n LIKE ` + a.add(like) + ` ESCAPE '\')`
			}
			inner += ` GROUP BY 1 ORDER BY 2 DESC LIMIT ` + a.add(limit+1)
			sqlq = `SELECT d.id, d.value_n, d.label, g.n FROM (` + inner + `) g JOIN fdd.dims d ON d.id = g.id ORDER BY g.n DESC, d.id`
		}
		rows, err := p.pool.Query(ctx, sqlq, a...)
		if err != nil {
			return nil, false, err
		}
		if err := scan(rows); err != nil {
			return nil, false, err
		}
	}
	truncated := false
	if len(items) > limit {
		for _, it := range items[limit:] {
			delete(seen, it.id)
		}
		items, truncated = items[:limit], true
	}
	// selected values are always present, with their count under the other filters
	var missing []int64
	for _, id := range r.ids[dim] {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		var a pgArgs
		var sqlq string
		if othersImpossible {
			sqlq = `SELECT id, value_n, label, 0::bigint FROM fdd.dims WHERE id = ANY(` + a.add(missing) + `::bigint[])`
		} else if rest.IsEmpty() {
			sqlq = `SELECT id, value_n, label, cnt FROM fdd.dims WHERE id = ANY(` + a.add(missing) + `::bigint[])`
		} else {
			w := r.where(dim, &a)
			sqlq = `SELECT d.id, d.value_n, d.label, COALESCE(g.n, 0) FROM fdd.dims d LEFT JOIN (SELECT c.` + d.Key + ` AS id, count(*) AS n
				FROM fdd.contacts c WHERE ` + w + ` AND c.` + d.Key + ` = ANY(` + a.add(missing) + `::bigint[]) GROUP BY 1) g ON g.id = d.id
				WHERE d.id = ANY(` + a.add(missing) + `::bigint[])`
		}
		rows, err := p.pool.Query(ctx, sqlq, a...)
		if err != nil {
			return nil, false, err
		}
		if err := scan(rows); err != nil {
			return nil, false, err
		}
	}
	out := make([]FacetCount, len(items))
	for i, it := range items {
		out[i] = FacetCount{Value: it.v, Label: it.l, Count: it.n}
	}
	return out, truncated, nil
}

func (p *Postgres) Page(ctx context.Context, f filter.Filter, offset, limit int) ([]search.Row, error) {
	ctx, release, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := p.resolve(ctx, f)
	if err != nil {
		return nil, err
	}
	if r.impossible() || limit <= 0 {
		return []search.Row{}, nil
	}
	var a pgArgs
	w := r.where("", &a)
	ids, err := p.ids(ctx, `SELECT c.id FROM fdd.contacts c WHERE `+w+` ORDER BY c.id LIMIT `+a.add(limit)+` OFFSET `+a.add(max(offset, 0)), a...)
	if err != nil {
		return nil, err
	}
	return p.fetchRows(ctx, ids, false)
}

func (p *Postgres) ids(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// fetchRows loads display rows for ids, preserving their order.
func (p *Postgres) fetchRows(ctx context.Context, ids []int64, withExtra bool) ([]search.Row, error) {
	if len(ids) == 0 {
		return []search.Row{}, nil
	}
	byID := make(map[int64]*search.Row, len(ids))
	dimsOf := make(map[int64][12]int64, len(ids))
	var dimIDs []int64
	for start := 0; start < len(ids); start += 1000 {
		chunk := ids[start:min(start+1000, len(ids))]
		rows, err := p.pool.Query(ctx, `SELECT c.id, c.country, c.state, c.city, c.industry, c.sector, c.ctype, c.seniority, c.division,
			c.decision, c.csize, c.revenue, c.econf, i.name, i.title, i.company, i.website, i.linkedin, i.email, i.phone,
			CASE WHEN $2::bool THEN i.extra END
			FROM fdd.contacts c JOIN fdd.contact_info i ON i.id = c.id WHERE c.id = ANY($1::bigint[])`, chunk, withExtra)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r search.Row
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
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	labels, err := p.dimLabels(ctx, dimIDs)
	if err != nil {
		return nil, err
	}
	lab := func(id int64) string {
		if id <= 12 {
			return ""
		}
		return labels[id]
	}
	out := make([]search.Row, 0, len(byID))
	for _, id := range ids {
		r, ok := byID[id]
		if !ok {
			continue
		}
		dm := dimsOf[id]
		r.Country, r.State, r.City = lab(dm[0]), lab(dm[1]), lab(dm[2])
		r.Industry, r.Sector, r.CType = lab(dm[3]), lab(dm[4]), lab(dm[5])
		r.Seniority, r.Division, r.Decision = lab(dm[6]), lab(dm[7]), lab(dm[8])
		r.CSize, r.Revenue, r.EConf = lab(dm[9]), lab(dm[10]), lab(dm[11])
		out = append(out, *r)
	}
	return out, nil
}

func (p *Postgres) dimLabels(ctx context.Context, ids []int64) (map[int64]string, error) {
	ver := strconv.FormatInt(p.ver.Load(), 10) + "|"
	out := map[int64]string{}
	var missing []int64
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 12 || seen[id] {
			continue
		}
		seen[id] = true
		if b, ok := p.labels.Get(ver + strconv.FormatInt(id, 10)); ok {
			_, l, _ := strings.Cut(string(b), "\x00")
			out[id] = l
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `SELECT id, value_n, label FROM fdd.dims WHERE id = ANY($1::bigint[])`, missing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v, l string
		if err := rows.Scan(&id, &v, &l); err != nil {
			return nil, err
		}
		out[id] = l
		p.labels.Set(ver+strconv.FormatInt(id, 10), []byte(v+"\x00"+l), 30*time.Minute)
	}
	return out, rows.Err()
}

func (p *Postgres) Stats(ctx context.Context) (importer.HeadlineStats, error) {
	ver := p.ver.Load()
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	if p.statsVer == ver && ver != 0 {
		return p.stats, nil
	}
	var raw string
	if err := p.pool.QueryRow(ctx, `SELECT v FROM fdd.meta WHERE k='stats'`).Scan(&raw); err != nil {
		return importer.HeadlineStats{}, err
	}
	var h importer.HeadlineStats
	json.Unmarshal([]byte(raw), &h)
	p.stats, p.statsVer = h, ver
	return h, nil
}

func (p *Postgres) refreshStats(ctx context.Context) error {
	var h importer.HeadlineStats
	if err := p.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE has_email), count(*) FILTER (WHERE has_phone) FROM fdd.contacts`).
		Scan(&h.Contacts, &h.WithEmail, &h.WithPhone); err != nil {
		return err
	}
	p.pool.QueryRow(ctx, `SELECT count(*) FROM fdd.dims WHERE dim = 1 AND id > 12 AND cnt > 0`).Scan(&h.Countries)
	p.pool.QueryRow(ctx, `SELECT COALESCE(max(updated), 0) FROM fdd.contact_info`).Scan(&h.Updated)
	b, _ := json.Marshal(h)
	_, err := p.pool.Exec(ctx, `INSERT INTO fdd.meta(k, v) VALUES('stats', $1) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, string(b))
	return err
}

func (p *Postgres) Values(ctx context.Context, dim string, minCount, limit int) ([]FacetCount, error) {
	d, ok := filter.DimByKey(dim)
	if !ok {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `SELECT value_n, label, cnt FROM fdd.dims WHERE dim = $1 AND id > 12 AND cnt >= $2 ORDER BY cnt DESC, id LIMIT $3`,
		int16(d.ID), int64(max(minCount, 1)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FacetCount
	for rows.Next() {
		var v FacetCount
		if err := rows.Scan(&v.Value, &v.Label, &v.Count); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (p *Postgres) Combos(ctx context.Context, minCount, limit int) ([]Combo, error) {
	rows, err := p.pool.Query(ctx, `SELECT dc.value_n, di.value_n, g.n FROM (SELECT country, industry, count(*) AS n FROM fdd.contacts
		WHERE country > 12 AND industry > 12 GROUP BY country, industry HAVING count(*) >= $1 ORDER BY n DESC LIMIT $2) g
		JOIN fdd.dims dc ON dc.id = g.country JOIN fdd.dims di ON di.id = g.industry ORDER BY g.n DESC`, int64(max(minCount, 1)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Combo
	for rows.Next() {
		var c Combo
		if err := rows.Scan(&c.Country, &c.Industry, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *Postgres) Labels(ctx context.Context, dim string, values []string) (map[string]string, error) {
	out := map[string]string{}
	d, ok := filter.DimByKey(dim)
	if !ok || len(values) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `SELECT value_n, label FROM fdd.dims WHERE dim = $1 AND value_n = ANY($2::text[])`, int16(d.ID), values)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v, l string
		if err := rows.Scan(&v, &l); err != nil {
			return nil, err
		}
		out[v] = l
	}
	return out, rows.Err()
}

func (p *Postgres) Dims(ctx context.Context) ([]DimCount, error) {
	rows, err := p.pool.Query(ctx, `SELECT dim, value_n, label, cnt FROM fdd.dims WHERE cnt > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DimCount
	for rows.Next() {
		var d DimCount
		var dim int16
		if err := rows.Scan(&dim, &d.Value, &d.Label, &d.Count); err != nil {
			return nil, err
		}
		d.Dim = int(dim)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- unlocks ----------

func (p *Postgres) UnlockSummary(ctx context.Context) ([]UserUnlocks, error) {
	rows, err := p.pool.Query(ctx, `SELECT user_id, count(*), min(batch), max(batch) FROM fdd.unlocked GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserUnlocks
	for rows.Next() {
		var u UserUnlocks
		if err := rows.Scan(&u.UserID, &u.N, &u.MinBatch, &u.MaxBatch); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (p *Postgres) Claim(ctx context.Context, uid, batch int64, f filter.Filter, n int, now int64) ([]search.Row, error) {
	ctx, release, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := p.resolve(ctx, f)
	if err != nil {
		return nil, err
	}
	if r.impossible() || n <= 0 {
		return []search.Row{}, nil
	}
	a := pgArgs{uid}
	w := r.where("", &a)
	ids, err := p.ids(ctx, `WITH picked AS (
			SELECT c.id FROM fdd.contacts c WHERE `+w+`
			AND NOT EXISTS (SELECT 1 FROM fdd.unlocked u WHERE u.user_id = $1 AND u.contact_id = c.id)
			ORDER BY c.id LIMIT `+a.add(n)+`)
		INSERT INTO fdd.unlocked(user_id, contact_id, batch, ts) SELECT $1, id, `+a.add(batch)+`, `+a.add(now)+` FROM picked
		ON CONFLICT DO NOTHING RETURNING contact_id`, a...)
	if err != nil || len(ids) == 0 {
		return []search.Row{}, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows, err := p.fetchRows(ctx, ids, false)
	for i := range rows {
		rows[i].Batch, rows[i].New = batch, true
	}
	return rows, err
}

func (p *Postgres) Unlocked(ctx context.Context, uid int64, f filter.Filter) (int64, error) {
	ctx, release, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	var n int64
	if f.IsEmpty() {
		err := p.pool.QueryRow(ctx, `SELECT count(*) FROM fdd.unlocked WHERE user_id = $1`, uid).Scan(&n)
		return n, err
	}
	r, err := p.resolve(ctx, f)
	if err != nil || r.impossible() {
		return 0, err
	}
	a := pgArgs{uid}
	w := r.where("", &a)
	err = p.pool.QueryRow(ctx, `SELECT count(*) FROM fdd.unlocked u JOIN fdd.contacts c ON c.id = u.contact_id WHERE u.user_id = $1 AND `+w, a...).Scan(&n)
	return n, err
}

func (p *Postgres) ViewPage(ctx context.Context, uid int64, f filter.Filter, after Cursor, self int, onlyBatch int64, limit int, withExtra bool) ([]search.Row, error) {
	ctx, release, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := p.resolve(ctx, f)
	if err != nil {
		return nil, err
	}
	if r.impossible() || limit <= 0 {
		return []search.Row{}, nil
	}
	a := pgArgs{uid}
	q := `SELECT u.contact_id, u.batch FROM fdd.unlocked u JOIN fdd.contacts c ON c.id = u.contact_id WHERE u.user_id = $1 AND ` + r.where("", &a)
	if onlyBatch > 0 {
		q += ` AND u.batch = ` + a.add(onlyBatch)
	}
	if cond := after.cond(self, "u.batch", "u.contact_id", func(v any) string { return a.add(v) }); cond != "" {
		q += " AND " + cond
	}
	q += ` ORDER BY u.batch DESC, u.contact_id LIMIT ` + a.add(limit)
	rows, err := p.pool.Query(ctx, q, a...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	batches := map[int64]int64{}
	for rows.Next() {
		var id, b int64
		if err := rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		batches[id] = b
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out, err := p.fetchRows(ctx, ids, withExtra)
	for i := range out {
		out[i].Batch = batches[out[i].ID]
	}
	return out, err
}

func (p *Postgres) ForgetUser(ctx context.Context, uid int64) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM fdd.unlocked WHERE user_id = $1`, uid)
	return err
}

// ---------- data management ----------

func (p *Postgres) Target() importer.Target { return &pgTarget{p: p} }

func (p *Postgres) ExistingKeys(ctx context.Context, email, li, h []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(email)+len(li)+len(h) == 0 {
		return out, nil
	}
	want := map[string]bool{}
	for _, k := range email {
		want["e:"+k] = true
	}
	for _, k := range li {
		want["l:"+k] = true
	}
	for _, k := range h {
		want["h:"+k] = true
	}
	rows, err := p.pool.Query(ctx, `SELECT COALESCE(email_key, ''), COALESCE(li_key, ''), COALESCE(h_key, '') FROM fdd.contact_info
		WHERE email_key = ANY($1::text[]) OR li_key = ANY($2::text[]) OR h_key = ANY($3::text[])`, nonNil(email), nonNil(li), nonNil(h))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e, l, hk string
		if err := rows.Scan(&e, &l, &hk); err != nil {
			return nil, err
		}
		for _, k := range []string{"e:" + e, "l:" + l, "h:" + hk} {
			if want[k] {
				out[k] = true
			}
		}
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (p *Postgres) FindByIdentity(ctx context.Context, kind, value string) ([]int64, error) {
	var q string
	var arg any
	switch kind {
	case "email":
		e, ok, _ := emailcheck.Syntax(value)
		if !ok {
			return nil, errors.New("invalid email")
		}
		q, arg = `SELECT id FROM fdd.contact_info WHERE email_key = $1`, normalize.EmailKey(e)
	case "linkedin":
		k := normalize.LinkedInKey(value)
		if k == "" {
			return nil, errors.New("invalid LinkedIn profile URL")
		}
		q, arg = `SELECT id FROM fdd.contact_info WHERE li_key = $1`, k
	case "phone":
		d := normalize.PhoneDigits(value)
		if len(d) < 6 {
			return nil, errors.New("invalid phone number")
		}
		q = `SELECT id FROM fdd.contact_info WHERE phone <> '' AND regexp_replace(phone, '[ ()+.-]', '', 'g') LIKE $1`
		arg = "%" + d
	default:
		return nil, errors.New("unknown identity kind")
	}
	return p.ids(ctx, q, arg)
}

func (p *Postgres) SuppressKeys(ctx context.Context, ids []int64) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT COALESCE(email_key, ''), COALESCE(li_key, ''), COALESCE(h_key, '') FROM fdd.contact_info WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var e, l, h string
		if err := rows.Scan(&e, &l, &h); err != nil {
			return nil, err
		}
		keys = append(keys, importer.SuppressKeys(e, l, h)...)
	}
	return keys, rows.Err()
}

func (p *Postgres) Delete(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE fdd.dims d SET cnt = GREATEST(d.cnt - g.n, 0)
		FROM (SELECT v.x AS id, count(*) AS n FROM fdd.contacts c
			CROSS JOIN LATERAL (VALUES (c.country), (c.state), (c.city), (c.industry), (c.sector), (c.ctype),
				(c.seniority), (c.division), (c.decision), (c.csize), (c.revenue), (c.econf)) AS v(x)
			WHERE c.id = ANY($1::bigint[]) GROUP BY v.x) g
		WHERE d.id = g.id`, ids); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM fdd.contacts WHERE id = ANY($1::bigint[])`, ids) // cascades to contact_info and unlocked
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	n := int(tag.RowsAffected())
	if n > 0 {
		if err := p.refreshStats(ctx); err != nil {
			return n, err
		}
		return n, p.bump(ctx)
	}
	return 0, nil
}

func (p *Postgres) Prune(ctx context.Context) error { return nil } // foreign keys cascade

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) Keepalive(ctx context.Context, now int64) error {
	if _, err := p.pool.Exec(ctx, `INSERT INTO fdd.meta(k, v) VALUES('heartbeat', $1) ON CONFLICT (k) DO UPDATE SET v = excluded.v`,
		strconv.FormatInt(now, 10)); err != nil {
		return err
	}
	return p.loadVersion(ctx) // picks up changes made by another process (CLI import)
}

func (p *Postgres) Size(ctx context.Context) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&n)
	return n, err
}
