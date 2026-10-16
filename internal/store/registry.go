package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"markemind/dials/internal/db"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/search"
)

var (
	ErrNotFound   = errors.New("no such database")
	ErrDuplicate  = errors.New("this database is already connected")
	ErrNotEmpty   = errors.New("this database still serves contacts: turn off \"Serve searches\" first (its data stays where it is)")
	ErrStoreFull  = errors.New("the chosen database is over 90% of its quota; pick another or choose Auto")
	ErrNoRoom     = errors.New("every writable database is over 90% of its quota: add a database or raise a quota")
	ErrStoresDown = errors.New("a connected database is unreachable; uploads need every database online so contacts are never duplicated")
)

// KeyFrom derives the key that seals stored connection strings from the app secret.
func KeyFrom(secret []byte) []byte {
	h := sha256.Sum256(append([]byte("fdd-store-dsn\x00"), secret...))
	return h[:]
}

func seal(key []byte, plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return "v1:" + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), []byte("dsn"))), nil
}

func unseal(key []byte, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, ok := strings.CutPrefix(sealed, "v1:")
	if !ok {
		return "", errors.New("unknown connection-string format")
	}
	b, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(b) < gcm.NonceSize() {
		return "", errors.New("connection string is corrupt")
	}
	p, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte("dsn"))
	if err != nil {
		return "", errors.New("can't decrypt the stored connection string (was SECRET_KEY changed?)")
	}
	return string(p), nil
}

// DisplayHost renders a connection string without its password.
func DisplayHost(kind, dsn string) string {
	if kind != KindPostgres {
		return filepath.Base(dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		// key=value form: show host and dbname only
		var host, dbn, user string
		for _, kv := range strings.Fields(dsn) {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "host":
				host = v
			case "dbname":
				dbn = v
			case "user":
				user = v
			}
		}
		return strings.Trim(user+"@"+host+"/"+dbn, "@/")
	}
	user := ""
	if u.User != nil {
		user = u.User.Username() + "@"
	}
	return user + u.Host + u.Path
}

// NewLocal is a federation over the built-in database only (tests, CLI tools).
func NewLocal(d *db.DB) *Federation {
	f := &Federation{Control: d, Key: make([]byte, 32), Cache: search.NewLRU(4<<20, nil), Budget: -1}
	f.members = []*Member{{ID: 0, Name: "This server", Kind: KindLocal, Read: true, Write: true, B: NewSQLite(d, true)}}
	f.init()
	return f
}

type memberRow struct {
	m   *Member
	dsn string
}

// Open loads the registry and connects every store. A store that can't be
// reached doesn't stop startup: it is marked down and retried by Monitor.
func (f *Federation) Open(ctx context.Context) error {
	if f.Key == nil {
		return errors.New("store: no key")
	}
	rows, err := f.Control.R.QueryContext(ctx, `SELECT id, name, kind, dsn, cap_bytes, readable, writable, bytes, contacts, last_ok, last_err FROM stores ORDER BY id`)
	if err != nil {
		return err
	}
	var list []memberRow
	for rows.Next() {
		m := &Member{}
		var sealed, lastErr string
		var rd, wr int
		var bytes, contacts, lastOK int64
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &sealed, &m.CapBytes, &rd, &wr, &bytes, &contacts, &lastOK, &lastErr); err != nil {
			rows.Close()
			return err
		}
		m.Read, m.Write = rd == 1, wr == 1
		m.bytes.Store(bytes)
		m.count.Store(contacts)
		m.lastErr = lastErr
		if lastOK > 0 {
			m.lastOK = time.Unix(lastOK, 0)
		}
		dsn, err := unseal(f.Key, sealed)
		if err != nil {
			m.lastErr = err.Error()
		}
		list = append(list, memberRow{m, dsn})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	hasLocal := false
	for _, r := range list {
		if r.m.Kind == KindLocal {
			hasLocal = true
			r.m.B = NewSQLite(f.Control, true)
			continue
		}
		r.m.dsn = r.dsn
		if r.dsn == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		b, err := f.dial(cctx, r.m.Kind, r.dsn)
		cancel()
		if err != nil {
			r.m.failed(f.now(), err)
			f.log().Warn("store unreachable at startup; will retry", "store", r.m.Name, "err", err)
			continue
		}
		r.m.B = b
		r.m.ok(f.now())
	}
	if !hasLocal { // registry predates the built-in row (or tests): it always exists
		list = append([]memberRow{{m: &Member{ID: 0, Name: "This server", Kind: KindLocal, Read: true, Write: true, B: NewSQLite(f.Control, true)}}}, list...)
	}
	ms := make([]*Member, len(list))
	for i, r := range list {
		ms[i] = r.m
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].ID < ms[j].ID })
	f.mu.Lock()
	f.members = ms
	f.mu.Unlock()
	f.gen.Add(1)
	f.init()
	if err := f.loadMirrors(ctx); err != nil {
		return err
	}
	for _, m := range ms { // a store changed while we were down (or was never mirrored): refresh it
		if m.remote() && m.B != nil && m.fresh() == nil {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := f.syncMirror(cctx, m); err != nil {
				f.log().Warn("mirror refresh at startup", "store", m.Name, "err", err)
			}
			cancel()
		}
	}
	return nil
}

// Close disconnects every store (the built-in database is closed by its owner).
func (f *Federation) Close() {
	for _, m := range f.Members() {
		if m.B != nil {
			m.B.Close()
		}
	}
}

func (f *Federation) dial(ctx context.Context, kind, dsn string) (Backend, error) {
	switch kind {
	case KindPostgres:
		return OpenPostgres(ctx, dsn)
	case KindSQLite:
		p, err := f.sqlitePath(dsn)
		if err != nil {
			return nil, err
		}
		return OpenSQLite(ctx, p)
	}
	return nil, fmt.Errorf("unknown database kind %q", kind)
}

// sqlitePath keeps extra SQLite stores inside DATA_DIR (no arbitrary file writes).
func (f *Federation) sqlitePath(p string) (string, error) {
	if f.DataDir == "" {
		return "", errors.New("extra SQLite stores need DATA_DIR")
	}
	if !strings.HasSuffix(p, ".db") {
		return "", errors.New("the file name must end in .db")
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(f.DataDir, p)
	}
	base, err := filepath.Abs(f.DataDir)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.Base(abs) == "dials.db" {
		return "", errors.New("the file must be inside the data directory and can't be the main database")
	}
	return abs, nil
}

// Probe is what "Test connection" reports.
type Probe struct {
	Server    string `json:"server"`
	Bytes     int64  `json:"bytes"`
	HasSchema bool   `json:"has_schema"`
	Contacts  int64  `json:"contacts"`
	Latency   int64  `json:"latency_ms"`
}

// Test connects without changing anything.
func (f *Federation) Test(ctx context.Context, kind, dsn string) (Probe, error) {
	start := time.Now()
	switch kind {
	case KindPostgres:
		p, err := ProbePostgres(ctx, dsn)
		p.Latency = time.Since(start).Milliseconds()
		return p, err
	case KindSQLite:
		path, err := f.sqlitePath(dsn)
		return Probe{Server: "SQLite file " + filepath.Base(path), Latency: time.Since(start).Milliseconds()}, err
	}
	return Probe{}, fmt.Errorf("unknown database kind %q", kind)
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 60 {
		return "", errors.New("give the database a name (up to 60 characters)")
	}
	return name, nil
}

// Add connects a new store (creating its schema if needed) and registers it.
// Re-adding a detached store re-attaches its existing contacts and unlocks.
func (f *Federation) Add(ctx context.Context, name, kind, dsn string, capBytes int64) (*Member, error) {
	name, err := validName(name)
	if err != nil {
		return nil, err
	}
	if kind != KindPostgres && kind != KindSQLite {
		return nil, errors.New("kind must be postgres or sqlite")
	}
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, errors.New("paste the connection string")
	}
	if capBytes < 0 || capBytes > 1<<50 {
		return nil, errors.New("quota out of range")
	}
	b, err := f.dial(ctx, kind, dsn)
	if err != nil {
		return nil, err
	}
	ident := storeIdentity(ctx, b)
	for _, m := range f.Members() {
		if m.B != nil && ident != "" && storeIdentity(ctx, m.B) == ident {
			b.Close()
			return nil, ErrDuplicate
		}
	}
	sealed, err := seal(f.Key, dsn)
	if err != nil {
		b.Close()
		return nil, err
	}
	m := &Member{Name: name, Kind: kind, CapBytes: capBytes, Read: true, Write: true, B: b, dsn: dsn}
	if n, err := b.Size(ctx); err == nil {
		m.bytes.Store(n)
	}
	if st, err := b.Stats(ctx); err == nil {
		m.count.Store(st.Contacts)
	}
	now := f.now().Unix()
	err = f.Control.W.QueryRowContext(ctx, `INSERT INTO stores(name, kind, dsn, host, cap_bytes, readable, writable, created, bytes, contacts, last_ok)
		VALUES(?,?,?,?,?,1,1,?,?,?,?) RETURNING id`, name, kind, sealed, DisplayHost(kind, dsn), capBytes, now, m.bytes.Load(), m.count.Load(), now).Scan(&m.ID)
	if err != nil {
		b.Close()
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errors.New("another database already has that name")
		}
		return nil, err
	}
	m.ok(f.now())
	if err := f.syncMirror(ctx, m); err != nil {
		f.log().Warn("mirror of new store", "store", m.Name, "err", err)
	}
	if err := f.rebuildUserStores(ctx, m); err != nil { // re-attached: its unlocks are users' again
		f.log().Warn("unlock index of new store", "store", m.Name, "err", err)
	}
	f.mu.Lock()
	f.members = append(f.members, m)
	sort.Slice(f.members, func(i, j int) bool { return f.members[i].ID < f.members[j].ID })
	f.mu.Unlock()
	f.bump(ctx)
	return m, nil
}

// identityer is implemented by backends that carry a stable store id.
type identityer interface {
	Identity(ctx context.Context) string
}

func storeIdentity(ctx context.Context, b Backend) string {
	if i, ok := b.(identityer); ok {
		return i.Identity(ctx)
	}
	if s, ok := b.(*SQLite); ok {
		p, _ := filepath.Abs(s.DB.Path)
		return "sqlite:" + strings.ToLower(p)
	}
	return ""
}

// Update changes a store's name, quota and roles.
func (f *Federation) Update(ctx context.Context, id int, name string, read, write bool, capBytes int64) error {
	m := f.Member(id)
	if m == nil {
		return ErrNotFound
	}
	name, err := validName(name)
	if err != nil {
		return err
	}
	if id == 0 && !read {
		return errors.New("the built-in database always serves searches (it holds accounts and settings too)")
	}
	if capBytes < 0 || capBytes > 1<<50 {
		return errors.New("quota out of range")
	}
	if _, err := f.Control.W.ExecContext(ctx, `UPDATE stores SET name=?, readable=?, writable=?, cap_bytes=? WHERE id=?`,
		name, b2i(read), b2i(write), capBytes, id); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return errors.New("another database already has that name")
		}
		return err
	}
	f.mu.Lock()
	m.Name, m.Read, m.Write, m.CapBytes = name, read, write, capBytes
	f.mu.Unlock()
	f.bump(ctx)
	return nil
}

// Detach removes a store from the registry. Its data is left untouched where it is; adding the
// same connection string again re-attaches it.
func (f *Federation) Detach(ctx context.Context, id int) error {
	m := f.Member(id)
	if m == nil {
		return ErrNotFound
	}
	if id == 0 {
		return errors.New("the built-in database can't be removed")
	}
	if m.Read && m.count.Load() > 0 {
		return ErrNotEmpty
	}
	for _, q := range []string{`DELETE FROM stores WHERE id=?`, `DELETE FROM store_dims WHERE store_id=?`, `DELETE FROM user_stores WHERE store_id=?`} {
		if _, err := f.Control.W.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	f.mu.Lock()
	for i, x := range f.members {
		if x == m {
			f.members = append(f.members[:i:i], f.members[i+1:]...)
			break
		}
	}
	f.mu.Unlock()
	if m.B != nil {
		m.B.Close()
	}
	f.bump(ctx)
	return nil
}

// Statuses reports every store for the admin panel.
func (f *Federation) Statuses(ctx context.Context) []Status {
	hosts := map[int]string{}
	if rows, err := f.Control.R.QueryContext(ctx, `SELECT id, host FROM stores`); err == nil {
		for rows.Next() {
			var id int
			var h string
			if rows.Scan(&id, &h) == nil {
				hosts[id] = h
			}
		}
		rows.Close()
	}
	now := f.now()
	var out []Status
	for _, m := range f.Members() {
		if !m.remote() && m.B != nil { // the built-in store is cheap to read live
			if st, err := m.B.Stats(ctx); err == nil {
				m.count.Store(st.Contacts)
			}
			if n, err := m.B.Size(ctx); err == nil {
				m.bytes.Store(n)
			}
			m.ok(now)
		}
		m.mu.Lock()
		s := Status{ID: m.ID, Name: m.Name, Kind: m.Kind, Host: hosts[m.ID], Read: m.Read, Write: m.Write, CapBytes: m.CapBytes,
			Bytes: m.bytes.Load(), Contacts: m.count.Load(), LastErr: m.lastErr}
		if !m.lastOK.IsZero() {
			s.LastOK = m.lastOK.Unix()
		}
		down := m.B == nil || now.Before(m.until)
		m.mu.Unlock()
		switch {
		case down:
			s.State = "down"
		case !m.Read:
			s.State = "detached"
		case !m.Write:
			s.State = "read-only"
		default:
			s.State = "ok"
		}
		if m.CapBytes > 0 {
			s.Pct = int(min(100, s.Bytes*100/m.CapBytes))
		}
		out = append(out, s)
	}
	return out
}

// Warning is a store crossing a quota threshold (for alerts).
type Warning struct {
	Store string
	Pct   int
	Err   string
}

// Monitor reconnects down stores, writes a keepalive to remote ones (free
// Supabase projects pause after a week without activity), refreshes sizes and
// counts, and reports stores past 80 % of quota or unreachable.
func (f *Federation) Monitor(ctx context.Context) []Warning {
	var warns []Warning
	for _, m := range f.Members() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if m.B == nil && m.dsn != "" {
			if b, err := f.dial(cctx, m.Kind, m.dsn); err == nil {
				f.mu.Lock()
				m.B = b
				f.mu.Unlock()
				f.gen.Add(1)
			} else {
				m.failed(f.now(), err)
			}
		}
		if m.B == nil {
			cancel()
			warns = append(warns, Warning{Store: m.Name, Err: m.lastErr})
			continue
		}
		err := m.B.Keepalive(cctx, f.now().Unix())
		var bytes, contacts int64
		if err == nil {
			bytes, err = m.B.Size(cctx)
		}
		if err == nil {
			var s importer.HeadlineStats
			s, err = m.B.Stats(cctx)
			contacts = s.Contacts
		}
		cancel()
		if err != nil {
			m.failed(f.now(), err)
			f.Control.W.ExecContext(ctx, `UPDATE stores SET last_err=? WHERE id=?`, err.Error(), m.ID)
			warns = append(warns, Warning{Store: m.Name, Err: err.Error()})
			continue
		}
		m.ok(f.now())
		m.bytes.Store(bytes)
		m.count.Store(contacts)
		f.Control.W.ExecContext(ctx, `UPDATE stores SET bytes=?, contacts=?, last_ok=?, last_err='' WHERE id=?`, bytes, contacts, f.now().Unix(), m.ID)
		if m.remote() && m.fresh() == nil { // its data changed (e.g. a CLI import): refresh the mirror
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			if err := f.syncMirror(cctx, m); err != nil {
				warns = append(warns, Warning{Store: m.Name, Err: "mirror refresh: " + err.Error()})
			}
			cancel()
			f.bump(ctx)
		}
		if m.CapBytes > 0 && bytes*100 >= m.CapBytes*80 {
			warns = append(warns, Warning{Store: m.Name, Pct: int(bytes * 100 / m.CapBytes)})
		}
	}
	return warns
}

// refreshSize updates one store's size and count after a write.
func (f *Federation) refreshSize(ctx context.Context, m *Member) {
	if m.B == nil {
		return
	}
	if n, err := m.B.Size(ctx); err == nil {
		m.bytes.Store(n)
	}
	if st, err := m.B.Stats(ctx); err == nil {
		m.count.Store(st.Contacts)
	}
	f.Control.W.ExecContext(ctx, `UPDATE stores SET bytes=?, contacts=? WHERE id=?`, m.bytes.Load(), m.count.Load(), m.ID)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
