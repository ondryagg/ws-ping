package limiter

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

var (
	ip1 = netip.MustParseAddr("192.0.2.1")
	ip2 = netip.MustParseAddr("192.0.2.2")
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) Add(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func TestSemaphoreNeverBlocks(t *testing.T) {
	s := NewSemaphore(2)
	got := []bool{s.TryAcquire(), s.TryAcquire(), s.TryAcquire()}
	if !got[0] || !got[1] || got[2] {
		t.Fatal("capacity of 2")
	}
	s.Release()
	if !s.TryAcquire() || s.InUse() != 2 {
		t.Fatal("slot must be reusable")
	}
	s.Release()
	s.Release()
	s.Release() // extra release must not panic or go negative
	if s.InUse() != 0 {
		t.Fatal("in use")
	}
}

func TestCounterEvictsKeysAtZero(t *testing.T) {
	c := NewCounter(2)
	if a, b, over := c.TryAcquire(ip1), c.TryAcquire(ip1), c.TryAcquire(ip1); !a || !b || over {
		t.Fatal("per key cap of 2")
	}
	if !c.TryAcquire(ip2) {
		t.Fatal("other key unaffected")
	}
	c.Release(ip1)
	c.Release(ip1)
	c.Release(ip2)
	c.Release(ip2) // double release is harmless
	if c.Len() != 0 {
		t.Fatalf("map must shrink to zero, has %d", c.Len())
	}
}

func TestBucketsRefillSweepAndCap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	b := NewBuckets(1, 3, 2, clk.Now)
	for i := 0; i < 3; i++ {
		if !b.Allow(ip1) {
			t.Fatalf("burst token %d", i)
		}
	}
	if b.Allow(ip1) {
		t.Fatal("burst exhausted")
	}
	clk.Add(1500 * time.Millisecond)
	if !b.Allow(ip1) || b.Allow(ip1) {
		t.Fatal("1.5 s at 1/s refills exactly one token")
	}
	b.Allow(ip2)
	if b.Allow(netip.MustParseAddr("192.0.2.3")) {
		t.Fatal("table full: unknown keys must be refused, not stored")
	}
	if b.Len() != 2 {
		t.Fatalf("len %d", b.Len())
	}
	clk.Add(time.Minute)
	b.Sweep()
	if b.Len() != 0 {
		t.Fatalf("refilled buckets must be swept, have %d", b.Len())
	}
	if !b.Allow(netip.MustParseAddr("192.0.2.3")) {
		t.Fatal("space again after sweep")
	}
}

func TestBucketsConcurrentUse(t *testing.T) {
	b := NewBuckets(1000, 10, 1000, time.Now)
	c := NewCounter(5)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Allow(ip1)
				if c.TryAcquire(ip1) {
					c.Release(ip1)
				}
				b.Sweep()
			}
		}()
	}
	wg.Wait()
	if c.Len() != 0 {
		t.Fatal("counter leaked")
	}
}
