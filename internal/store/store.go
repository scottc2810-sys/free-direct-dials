// Package store spreads contact data over several databases ("stores"): the
// built-in SQLite file plus any number of Postgres databases (Supabase free
// projects) or extra SQLite files. The control plane (users, sessions,
// settings, ad tokens, mail, suppression list) always stays in the built-in
// database; each store holds its own contacts and the unlocks of those contacts.
//
// A Federation fans reads out to every readable store and merges the results;
// the order of stores (by id) is the order contacts are previewed and claimed in.
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"markemind/dials/internal/filter"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/search"
)

const (
	KindLocal    = "local"    // the built-in database (id 0)
	KindSQLite   = "sqlite"   // an extra SQLite file under DATA_DIR
	KindPostgres = "postgres" // e.g. a Supabase project
)

// IDBits is how many bits of a public contact id belong to the store-local id:
// public id = storeID<<IDBits | localID. 2^40 local ids per store, 8191 stores,
// and every id stays below 2^53 (exact in JavaScript).
const IDBits = 40

const localMask = 1<<IDBits - 1

// GlobalID makes a public contact id.
func GlobalID(store int, local int64) int64 { return int64(store)<<IDBits | local }

// SplitID reverses GlobalID.
func SplitID(id int64) (store int, local int64) { return int(id >> IDBits), id & localMask }

// FacetCount is one dimension value in one store.
type FacetCount struct {
	Value string // normalised (value_n)
	Label string // most common spelling in that store
	Count int64
}

// DimCount is one dictionary value with its contact count (mirror sync).
type DimCount struct {
	Dim          int
	Value, Label string
	Count        int64
}

// UserUnlocks summarises one user's unlocks in a store (re-attaching a store).
type UserUnlocks struct {
	UserID, N, MinBatch, MaxBatch int64
}

// Combo is a country × industry pair with its contact count.
type Combo struct {
	Country, Industry string // value_n
	Count             int64
}

// Cursor is a keyset position in a user's unlocked list, which is ordered by
// batch (newest first), then store, then contact id. The zero value is the start.
type Cursor struct {
	Batch int64
	Store int
	ID    int64 // store-local
}

func (c Cursor) IsZero() bool { return c == Cursor{} }

// String encodes the cursor for URLs ("" for the start).
func (c Cursor) String() string {
	if c.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", c.Batch, c.Store, c.ID)
}

// ParseCursor accepts what String produced; anything else is the start.
func ParseCursor(s string) Cursor {
	p := strings.Split(s, ".")
	if len(p) != 3 {
		return Cursor{}
	}
	b, e1 := strconv.ParseInt(p[0], 10, 64)
	st, e2 := strconv.Atoi(p[1])
	id, e3 := strconv.ParseInt(p[2], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || b < 0 || st < 0 || id < 0 {
		return Cursor{}
	}
	return Cursor{b, st, id}
}

// cond renders the keyset predicate for store self, in the store's dialect.
// Order is batch DESC, store ASC, id ASC, so rows strictly after the cursor are:
// an older batch; or the same batch in a later store; or the same batch and store, a larger id.
func (c Cursor) cond(self int, batchCol, idCol string, ph func(any) string) string {
	if c.IsZero() {
		return ""
	}
	switch {
	case self < c.Store:
		return batchCol + " < " + ph(c.Batch)
	case self > c.Store:
		return batchCol + " <= " + ph(c.Batch)
	}
	return "(" + batchCol + " < " + ph(c.Batch) + " OR (" + batchCol + " = " + ph(c.Batch) + " AND " + idCol + " > " + ph(c.ID) + "))"
}

// ErrUnavailable means a store could not be reached; results may be partial.
var ErrUnavailable = errors.New("a contacts database is unreachable")

// Backend is one contacts database. Row ids in and out are store-local; the
// Federation converts them to public ids.
type Backend interface {
	Kind() string
	// Version changes whenever this store's contact data changes.
	Version() int64

	// Reads. Filters arrive canonical; values the store has never seen match nothing.
	Count(ctx context.Context, f filter.Filter) (int64, error)
	Facet(ctx context.Context, f filter.Filter, dim, q string, limit int) (vals []FacetCount, truncated bool, err error)
	Page(ctx context.Context, f filter.Filter, offset, limit int) ([]search.Row, error) // by id, unmasked
	Stats(ctx context.Context) (importer.HeadlineStats, error)
	Values(ctx context.Context, dim string, minCount, limit int) ([]FacetCount, error) // top values (no blanks)
	Combos(ctx context.Context, minCount, limit int) ([]Combo, error)
	Labels(ctx context.Context, dim string, values []string) (map[string]string, error)
	Dims(ctx context.Context) ([]DimCount, error) // every value with contacts (blanks included)

	// Unlocks.
	Claim(ctx context.Context, uid, batch int64, f filter.Filter, n int, now int64) ([]search.Row, error)
	Unlocked(ctx context.Context, uid int64, f filter.Filter) (int64, error)
	ViewPage(ctx context.Context, uid int64, f filter.Filter, after Cursor, self int, onlyBatch int64, limit int, withExtra bool) ([]search.Row, error)
	ForgetUser(ctx context.Context, uid int64) error
	UnlockSummary(ctx context.Context) ([]UserUnlocks, error)

	// Data management.
	Target() importer.Target
	ExistingKeys(ctx context.Context, email, li, h []string) (map[string]bool, error) // keys as "e:"/"l:"/"h:"+key
	FindByIdentity(ctx context.Context, kind, value string) ([]int64, error)
	SuppressKeys(ctx context.Context, ids []int64) ([]string, error)
	Delete(ctx context.Context, ids []int64) (int, error)
	Prune(ctx context.Context) error // drop unlocks whose contact is gone
	// Renormalize re-applies the current alias tables to the dictionary once per
	// normalize.Version, merging values that now mean the same; returns how many changed.
	Renormalize(ctx context.Context) (int, error)

	Ping(ctx context.Context) error
	Keepalive(ctx context.Context, now int64) error // a real write: keeps free-tier projects from pausing
	Size(ctx context.Context) (int64, error)        // bytes on disk
	Close() error
}
