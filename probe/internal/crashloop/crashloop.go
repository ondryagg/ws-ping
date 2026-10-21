// Package crashloop remembers recent watchdog exits across restarts so a
// permanently broken node degrades to 503 instead of restarting forever.
package crashloop

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	Window   = 10 * time.Minute
	Limit    = 5 // more than this many exits inside Window trips the guard
	maxLines = 64
	fileName = "watchdog-exits"
)

type Guard struct {
	path string
	now  func() time.Time
}

func New(stateDir string, now func() time.Time) *Guard {
	return &Guard{path: filepath.Join(stateDir, fileName), now: now}
}

func (g *Guard) read() []time.Time {
	f, err := os.Open(g.path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []time.Time
	cut := g.now().Add(-Window)
	sc := bufio.NewScanner(f)
	for sc.Scan() && len(out) < maxLines {
		sec, err := strconv.ParseInt(strings.TrimSpace(sc.Text()), 10, 64)
		if err != nil {
			continue
		}
		if t := time.Unix(sec, 0); t.After(cut) {
			out = append(out, t)
		}
	}
	return out
}

// Count is the number of watchdog exits inside the window.
func (g *Guard) Count() int { return len(g.read()) }

// Tripped reports whether the restart budget is exhausted.
func (g *Guard) Tripped() bool { return g.Count() > Limit }

// Record appends an exit. The write is atomic (temp file then rename).
func (g *Guard) Record() error {
	ts := append(g.read(), g.now())
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(strconv.FormatInt(t.Unix(), 10))
		b.WriteByte('\n')
	}
	return g.write(b.String())
}

// Reset clears the history after a confirmed recovery.
func (g *Guard) Reset() error {
	if err := os.Remove(g.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (g *Guard) write(content string) error {
	tmp := g.path + ".tmp"
	_ = os.Remove(tmp)                                                                      // a stale or planted entry must not be followed
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // fixed name inside the operator-owned state dir
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, g.path)
}
