// Package limiter holds the bounded admission-control primitives. No lock
// here is ever held across I/O, and every map evicts or refuses to grow.
package limiter

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// Clock is injected so tests control time.
type Clock func() time.Time

// Semaphore is a non-blocking bounded counter.
type Semaphore struct{ ch chan struct{} }

func NewSemaphore(n int) *Semaphore { return &Semaphore{ch: make(chan struct{}, n)} }

// TryAcquire never blocks.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Semaphore) Release() {
	select {
	case <-s.ch:
	default:
	}
}

func (s *Semaphore) InUse() int { return len(s.ch) }

// Counter limits concurrent holders per key. A key is deleted when its count
// returns to zero, so the map is bounded by the number of live holders.
type Counter struct {
	mu  sync.Mutex
	max int
	m   map[netip.Addr]int
}

func NewCounter(max int) *Counter { return &Counter{max: max, m: make(map[netip.Addr]int)} }

func (c *Counter) TryAcquire(k netip.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.m[k]
	if n >= c.max {
		return false
	}
	c.m[k] = n + 1
	return true
}

func (c *Counter) Release(k netip.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.m[k]; n > 1 {
		c.m[k] = n - 1
	} else {
		delete(c.m, k)
	}
}

// Len is the number of keys currently tracked.
func (c *Counter) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Buckets is a per-key token bucket. Entries whose bucket has refilled are
// removed by Sweep (equivalent to a full bucket), and the table has a hard
// size cap: when it is full, unknown keys are refused rather than allowed to
// grow memory. Under a flood from more distinct sources than maxKeys that
// fails closed for new sources only.
type Buckets struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	maxKeys int
	now     Clock
	m       map[netip.Addr]*bucket
}

// NewBuckets refills at ratePerSec up to burst tokens.
func NewBuckets(ratePerSec float64, burst, maxKeys int, now Clock) *Buckets {
	return &Buckets{rate: ratePerSec, burst: float64(burst), maxKeys: maxKeys, now: now, m: make(map[netip.Addr]*bucket)}
}

func (b *Buckets) Allow(k netip.Addr) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	bk, ok := b.m[k]
	if !ok {
		if len(b.m) >= b.maxKeys {
			return false
		}
		bk = &bucket{tokens: b.burst, last: t}
		b.m[k] = bk
	} else if el := t.Sub(bk.last).Seconds(); el > 0 {
		bk.tokens = min(b.burst, bk.tokens+el*b.rate)
		bk.last = t
	}
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// Sweep drops entries that have fully refilled.
func (b *Buckets) Sweep() {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	for k, bk := range b.m {
		if bk.tokens+t.Sub(bk.last).Seconds()*b.rate >= b.burst {
			delete(b.m, k)
		}
	}
}

func (b *Buckets) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// Run sweeps until ctx ends. Owner: the caller's goroutine; exit: ctx.Done.
func (b *Buckets) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Sweep()
		}
	}
}
