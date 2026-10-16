package search

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// LRU is a byte-bounded, TTL-aware cache of immutable byte slices.
type LRU struct {
	mu   sync.Mutex
	max  int64
	cur  int64
	ll   *list.List
	m    map[string]*list.Element
	now  func() time.Time
	hits atomic.Int64
	miss atomic.Int64
}

type entry struct {
	key string
	val []byte
	exp time.Time
}

const entryOverhead = 96

func NewLRU(maxBytes int64, now func() time.Time) *LRU {
	if now == nil {
		now = time.Now
	}
	return &LRU{max: maxBytes, ll: list.New(), m: map[string]*list.Element{}, now: now}
}

func (c *LRU) Get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if !ok {
		c.miss.Add(1)
		return nil, false
	}
	e := el.Value.(*entry)
	if !e.exp.IsZero() && c.now().After(e.exp) {
		c.remove(el)
		c.miss.Add(1)
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return e.val, true
}

func (c *LRU) Set(k string, v []byte, ttl time.Duration) {
	size := int64(len(k) + len(v) + entryOverhead)
	if size > c.max/4 {
		return // never let one value evict a quarter of the cache
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	if el, ok := c.m[k]; ok {
		e := el.Value.(*entry)
		c.cur += int64(len(v) - len(e.val))
		e.val, e.exp = v, exp
		c.ll.MoveToFront(el)
	} else {
		c.m[k] = c.ll.PushFront(&entry{key: k, val: v, exp: exp})
		c.cur += size
	}
	for c.cur > c.max && c.ll.Len() > 0 {
		c.remove(c.ll.Back())
	}
}

func (c *LRU) remove(el *list.Element) {
	e := el.Value.(*entry)
	c.ll.Remove(el)
	delete(c.m, e.key)
	c.cur -= int64(len(e.key) + len(e.val) + entryOverhead)
}

// Purge drops everything (admin "purge cache").
func (c *LRU) Purge() {
	c.mu.Lock()
	c.ll.Init()
	c.m = map[string]*list.Element{}
	c.cur = 0
	c.mu.Unlock()
}

type CacheStats struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
}

func (c *LRU) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Entries: c.ll.Len(), Bytes: c.cur, Hits: c.hits.Load(), Misses: c.miss.Load()}
}

// Group collapses concurrent calls for the same key into one execution.
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

type call struct {
	wg  sync.WaitGroup
	val []byte
	err error
}

func (g *Group) Do(key string, fn func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*call{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &call{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		c.wg.Done()
	}()
	c.val, c.err = fn()
	return c.val, c.err
}
