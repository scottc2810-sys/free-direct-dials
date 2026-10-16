package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"markemind/dials/internal/importer"
)

// Auto lets the router pick the first writable store with room.
const Auto = -1

// Router is the import target for a federation: a record whose identity is
// already stored in some database merges there (never a duplicate across
// databases); new contacts go to the primary store. In Auto mode the primary
// moves to the next writable store when one passes 90 % of its quota.
type Router struct {
	f       *Federation
	choice  int
	members []*Member
	primary *Member
	targets map[int]importer.Target
	changed map[int]int64
	batches int

	// Written counts rows written per store id (for the upload record).
	Written map[int]int64
}

// ImportTarget prepares a router; choice is a store id or Auto.
func (f *Federation) ImportTarget(choice int) *Router {
	return &Router{f: f, choice: choice, targets: map[int]importer.Target{}, changed: map[int]int64{}, Written: map[int]int64{}}
}

func (r *Router) Begin(ctx context.Context) error {
	r.members = nil
	for _, m := range r.f.Members() {
		if m.B == nil {
			return fmt.Errorf("%w (%s)", ErrStoresDown, m.Name)
		}
		if err := m.B.Ping(ctx); err != nil {
			m.failed(r.f.now(), err)
			return fmt.Errorf("%w (%s: %v)", ErrStoresDown, m.Name, err)
		}
		r.members = append(r.members, m)
	}
	if r.choice != Auto {
		for _, m := range r.members {
			if m.ID == r.choice {
				if !m.Write {
					return fmt.Errorf("%s doesn't accept uploads (turn on \"Receive uploads\")", m.Name)
				}
				r.f.refreshSize(ctx, m)
				if m.full() {
					return ErrStoreFull
				}
				r.primary = m
				return nil
			}
		}
		return ErrNotFound
	}
	return r.pickPrimary(ctx, nil)
}

func (r *Router) pickPrimary(ctx context.Context, after *Member) error {
	passed := after == nil
	for _, m := range r.members {
		if !passed {
			passed = m == after
			continue
		}
		if !m.Write || !m.Read {
			continue
		}
		r.f.refreshSize(ctx, m)
		if !m.full() {
			r.primary = m
			return nil
		}
	}
	return ErrNoRoom
}

func (r *Router) target(ctx context.Context, m *Member) (importer.Target, error) {
	if t, ok := r.targets[m.ID]; ok {
		return t, nil
	}
	t := m.B.Target()
	if err := t.Begin(ctx); err != nil {
		return nil, err
	}
	r.targets[m.ID] = t
	return t, nil
}

func (r *Router) Write(ctx context.Context, recs []*importer.Record, now int64) (importer.Counts, error) {
	groups := map[int][]*importer.Record{}
	rest := recs
	if len(r.members) > 1 {
		for _, m := range r.members {
			if m == r.primary || len(rest) == 0 || (m.remote() && m.count.Load() == 0) {
				continue // an empty store can't hold any of these people
			}
			var e, l, h []string
			for _, rec := range rest {
				if rec.EmailKey != "" {
					e = append(e, rec.EmailKey)
				}
				if rec.LiKey != "" {
					l = append(l, rec.LiKey)
				}
				if rec.HKey != "" {
					h = append(h, rec.HKey)
				}
			}
			ex, err := m.B.ExistingKeys(ctx, e, l, h)
			if err != nil {
				return importer.Counts{}, fmt.Errorf("checking duplicates in %s: %w", m.Name, err)
			}
			if len(ex) == 0 {
				continue
			}
			keep := rest[:0:0]
			for _, rec := range rest {
				if (rec.EmailKey != "" && ex["e:"+rec.EmailKey]) || (rec.LiKey != "" && ex["l:"+rec.LiKey]) || (rec.HKey != "" && ex["h:"+rec.HKey]) {
					groups[m.ID] = append(groups[m.ID], rec)
				} else {
					keep = append(keep, rec)
				}
			}
			rest = keep
		}
	}
	r.batches++
	if r.primary.CapBytes > 0 && r.batches%10 == 0 {
		r.f.refreshSize(ctx, r.primary)
		if r.primary.full() {
			if r.choice != Auto {
				return importer.Counts{}, ErrStoreFull
			}
			if err := r.pickPrimary(ctx, r.primary); err != nil {
				return importer.Counts{}, err
			}
		}
	}
	if len(rest) > 0 {
		groups[r.primary.ID] = append(groups[r.primary.ID], rest...)
	}
	ids := make([]int, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var total importer.Counts
	for _, id := range ids {
		m := r.f.Member(id)
		if m == nil || m.B == nil {
			return total, ErrStoresDown
		}
		t, err := r.target(ctx, m)
		if err != nil {
			return total, fmt.Errorf("%s: %w", m.Name, err)
		}
		c, err := t.Write(ctx, groups[id], now)
		if err != nil {
			return total, fmt.Errorf("%s: %w", m.Name, err)
		}
		total.Added += c.Added
		total.Updated += c.Updated
		total.Unchanged += c.Unchanged
		r.changed[id] += c.Added + c.Updated
		r.Written[id] += c.Added + c.Updated + c.Unchanged
	}
	return total, nil
}

func (r *Router) End(ctx context.Context, changed int64) error {
	var errs []error
	for id, t := range r.targets {
		if err := t.End(ctx, r.changed[id]); err != nil {
			errs = append(errs, err)
		}
	}
	r.finish(ctx)
	return errors.Join(errs...)
}

func (r *Router) Abort(ctx context.Context, changed int64) {
	for id, t := range r.targets {
		t.Abort(ctx, r.changed[id])
	}
	r.finish(ctx)
}

func (r *Router) finish(ctx context.Context) {
	for id := range r.targets {
		if m := r.f.Member(id); m != nil {
			r.f.refreshSize(ctx, m)
			if err := r.f.syncMirror(ctx, m); err != nil {
				r.f.log().Warn("mirror refresh after import", "store", m.Name, "err", err)
			}
		}
	}
	r.f.bump(ctx)
}

// Primary is the store new contacts are currently going to.
func (r *Router) Primary() string {
	if r.primary == nil {
		return ""
	}
	return r.primary.Name
}
