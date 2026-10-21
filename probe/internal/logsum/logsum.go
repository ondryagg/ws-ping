// Package logsum turns high-rate events into at most one log line per key per
// interval, so abuse cannot flood the log and nothing is logged per request.
package logsum

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Summarizer counts events by key. Keys come from small fixed sets.
type Summarizer struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func New() *Summarizer { return &Summarizer{counts: make(map[string]uint64)} }

func (s *Summarizer) Inc(key string) {
	s.mu.Lock()
	s.counts[key]++
	s.mu.Unlock()
}

// Drain returns and resets the counts.
func (s *Summarizer) Drain() map[string]uint64 {
	s.mu.Lock()
	out := s.counts
	s.counts = make(map[string]uint64, len(out))
	s.mu.Unlock()
	return out
}

// Run logs one line per non-zero key every interval. Owner: the caller;
// exit: ctx.Done, after a final flush.
func (s *Summarizer) Run(ctx context.Context, log *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	flush := func() {
		d := s.Drain()
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			log.Warn("events in window", "event", k, "count", d[k], "window", every.String())
		}
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-t.C:
			flush()
		}
	}
}
