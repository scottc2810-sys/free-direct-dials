// Package jobs runs background work: the scheduler, disk guard, backups,
// cleanup/maintenance and operator alerts.
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"markemind/dials/internal/platform"
)

var (
	ErrDiskFull = errors.New("storage is nearly full: uploads, imports and backups are paused")
	ErrReadOnly = errors.New("the site is in read-only mode right now; browsing still works")
)

// Disk levels.
const (
	LevelOK       = 0
	LevelWarn     = 1 // ≥ 80 %: alert the operator
	LevelNoHeavy  = 2 // ≥ 90 %: no uploads / imports / backups
	LevelReadOnly = 3 // ≥ 95 %: reject user writes too
)

// Guard tracks disk pressure and the manual maintenance switch.
type Guard struct {
	Path     string
	Stat     platform.DiskStatFunc
	Manual   func() bool // settings: site.read_only
	OnChange func(old, new int, pct float64)

	level atomic.Int32
	pct   atomic.Uint64
}

func (g *Guard) Check() (int, float64, error) {
	st, err := g.Stat(g.Path)
	if err != nil {
		return int(g.level.Load()), g.Pct(), err
	}
	p := st.UsedPct()
	lv := LevelOK
	switch {
	case p >= 95:
		lv = LevelReadOnly
	case p >= 90:
		lv = LevelNoHeavy
	case p >= 80:
		lv = LevelWarn
	}
	g.pct.Store(math.Float64bits(p))
	if old := int(g.level.Swap(int32(lv))); old != lv && g.OnChange != nil {
		g.OnChange(old, lv, p)
	}
	return lv, p, nil
}

func (g *Guard) Level() int     { return int(g.level.Load()) }
func (g *Guard) Pct() float64   { return math.Float64frombits(g.pct.Load()) }
func (g *Guard) ManualOn() bool { return g.Manual != nil && g.Manual() }

// AllowHeavy gates uploads, imports and backups.
func (g *Guard) AllowHeavy() error {
	if g.Level() >= LevelNoHeavy {
		return ErrDiskFull
	}
	return nil
}

// AllowWrite gates visitor writes (sign-ups, unlocks).
func (g *Guard) AllowWrite() error {
	if g.ManualOn() || g.Level() >= LevelReadOnly {
		return ErrReadOnly
	}
	return nil
}

// Alerter emails the operator, at most once per kind per window.
type Alerter struct {
	Send   func(ctx context.Context, subject, body string) error
	Clock  platform.Clock
	Log    *slog.Logger
	Window time.Duration // default 6 h

	mu   sync.Mutex
	last map[string]time.Time
}

func (a *Alerter) Alert(ctx context.Context, kind, subject, body string) {
	if a == nil {
		return
	}
	w := a.Window
	if w == 0 {
		w = 6 * time.Hour
	}
	a.mu.Lock()
	if a.last == nil {
		a.last = map[string]time.Time{}
	}
	now := a.Clock.Now()
	if t, ok := a.last[kind]; ok && now.Sub(t) < w {
		a.mu.Unlock()
		return
	}
	a.last[kind] = now
	a.mu.Unlock()
	a.Log.Error("ALERT", "kind", kind, "subject", subject, "body", body)
	if a.Send != nil {
		if err := a.Send(ctx, subject, body); err != nil {
			a.Log.Error("alert email failed", "err", err)
		}
	}
}
