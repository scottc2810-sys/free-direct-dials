package auth

import (
	"math"
	"sync"
	"time"
)

// Limiter is a memory-bounded token-bucket rate limiter keyed by string
// (usually client IP or email). It is per-process, which matches our single
// instance; Cloudflare's WAF rule is the outer layer.
type Limiter struct {
	mu    sync.Mutex
	rate  float64 // tokens per second
	burst float64
	max   int
	m     map[string]*bucket
	now   func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter allows n events per period with a burst of n.
func NewLimiter(n int, per time.Duration, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: float64(n) / per.Seconds(), burst: float64(n), max: maxKeys, m: map[string]*bucket{}, now: now}
}

func (l *Limiter) refill(b *bucket, t time.Time) {
	b.tokens = math.Min(l.burst, b.tokens+t.Sub(b.last).Seconds()*l.rate)
	b.last = t
}

// Allow consumes one token for key.
func (l *Limiter) Allow(key string) bool {
	ok, _ := l.Take(key)
	return ok
}

// Take consumes one token and, when refused, says how long until one is free.
func (l *Limiter) Take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	b, ok := l.m[key]
	if !ok {
		if len(l.m) >= l.max {
			l.gc(t)
		}
		b = &bucket{tokens: l.burst, last: t}
		l.m[key] = b
	}
	l.refill(b, t)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// gc drops buckets that have fully refilled; if still full, starts fresh
// (fails open under a key-flooding attack rather than growing without bound).
func (l *Limiter) gc(t time.Time) {
	for k, b := range l.m {
		l.refill(b, t)
		if b.tokens >= l.burst {
			delete(l.m, k)
		}
	}
	if len(l.m) >= l.max {
		l.m = map[string]*bucket{}
	}
}
