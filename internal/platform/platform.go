// Package platform holds small cross-cutting primitives: clock, ids, hashing,
// logging and disk statistics. Nothing here may import other internal packages.
package platform

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// Clock is injected everywhere time matters so tests can control it.
type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// FakeClock is a manually advanced clock for tests.
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Token returns a URL-safe random token with n bytes of entropy.
func Token(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Digits returns n uniformly random decimal digits.
func Digits(n int) string {
	var sb strings.Builder
	ten := big.NewInt(10)
	for i := 0; i < n; i++ {
		d, err := rand.Int(rand.Reader, ten)
		if err != nil {
			panic("crypto/rand unavailable: " + err.Error())
		}
		sb.WriteByte(byte('0' + d.Int64()))
	}
	return sb.String()
}

func SHA256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func HMACHex(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write([]byte(p))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// ConstantTimeEqual compares two strings without leaking timing.
func ConstantTimeEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// Day returns the UTC calendar day as YYYYMMDD.
func Day(t time.Time) int {
	t = t.UTC()
	return t.Year()*10000 + int(t.Month())*100 + t.Day()
}

// NewLogger builds the process logger. Production logs JSON at WARN by default.
func NewLogger(level string, jsonOut bool) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "info":
		lv = slog.LevelInfo
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelWarn
	}
	opts := &slog.HandlerOptions{Level: lv}
	if jsonOut {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// DiskStat reports filesystem capacity for the volume holding path.
type DiskStat struct {
	Total, Free uint64
}

// UsedPct returns used space as a percentage (0–100).
func (d DiskStat) UsedPct() float64 {
	if d.Total == 0 {
		return 0
	}
	return 100 * float64(d.Total-d.Free) / float64(d.Total)
}

// DiskStatFunc is swappable in tests.
type DiskStatFunc func(path string) (DiskStat, error)
