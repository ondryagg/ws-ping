// Package diag captures the evidence the spec asks for before the process
// exits itself: goroutines, descriptors, connection counts, recent errors.
package diag

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/sysinfo"
)

const (
	maxGoroutineDump = 4 << 20
	keepBundles      = 10
)

// Spare holds a few descriptors in reserve. When the process is out of file
// descriptors it cannot open the bundle file or /proc/self/fd, which is exactly
// when the bundle is needed. Release hands the reserve back just before the dump.
type Spare struct {
	mu    sync.Mutex
	files []*os.File
}

// NewSpare opens n descriptors on /dev/null. Failure to open some is not an
// error: the reserve is best effort.
func NewSpare(n int) *Spare {
	s := &Spare{}
	for i := 0; i < n; i++ {
		if f, err := os.Open(os.DevNull); err == nil {
			s.files = append(s.files, f)
		}
	}
	return s
}

// Release closes the reserve.
func (s *Spare) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		_ = f.Close()
	}
	s.files = nil
}

// Held reports how many descriptors are in reserve.
func (s *Spare) Held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.files)
}

// Entry is one remembered accept or upgrade error.
type Entry struct {
	At   time.Time
	Kind string
	Msg  string
}

// Ring keeps the last N entries. The lock is never held across I/O.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int
	full bool
}

func NewRing(n int) *Ring { return &Ring{buf: make([]Entry, n)} }

func (r *Ring) Add(at time.Time, kind, msg string) {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	r.mu.Lock()
	r.buf[r.next] = Entry{At: at, Kind: kind, Msg: msg}
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Snapshot returns entries oldest first.
func (r *Ring) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]Entry(nil), r.buf[:r.next]...)
	}
	return append(append([]Entry(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}

// Info is what the caller knows that this package cannot discover.
type Info struct {
	Reason         string
	Time           time.Time
	Version        string
	Commit         string
	ActiveConns    int64
	SelftestFails  int
	SelftestErrors []string
	Errors         *Ring
}

type capWriter struct {
	w    io.Writer
	left int
	cut  bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > c.left {
		p = p[:c.left]
		c.cut = true
	}
	c.left -= len(p)
	if _, err := c.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

// WriteBundle renders the human-readable bundle.
func WriteBundle(w io.Writer, in Info) {
	// Writes go to stderr and a file; a failed write must not stop the rest of
	// the bundle, so errors are deliberately dropped.
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	fds, ferr := sysinfo.OpenFDs()
	lim, lerr := sysinfo.FDLimit()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	pf("=== ondrya-probe diagnostic bundle ===\n")
	pf("time: %s\nreason: %s\nversion: %s commit: %s go: %s gomaxprocs: %d\n",
		in.Time.UTC().Format(time.RFC3339), in.Reason, in.Version, in.Commit, runtime.Version(), runtime.GOMAXPROCS(0))
	pf("active_connections: %d\nselftest_consecutive_failures: %d\n", in.ActiveConns, in.SelftestFails)
	if ferr == nil && lerr == nil {
		pf("open_fds: %d fd_limit: %d\n", fds, lim)
	} else {
		pf("open_fds: unavailable (%v / %v)\n", ferr, lerr)
	}
	pf("goroutines: %d\n", runtime.NumGoroutine())
	pf("\n--- recent self-test errors\n")
	for _, e := range in.SelftestErrors {
		pf("%s\n", e)
	}
	pf("\n--- last accept and upgrade errors (max 50)\n")
	if in.Errors != nil {
		for _, e := range in.Errors.Snapshot() {
			pf("%s %s %s\n", e.At.UTC().Format(time.RFC3339), e.Kind, e.Msg)
		}
	}
	pf("\n--- memory\nheap_alloc=%d heap_inuse=%d heap_sys=%d stack_inuse=%d sys=%d num_gc=%d next_gc=%d\n",
		ms.HeapAlloc, ms.HeapInuse, ms.HeapSys, ms.StackInuse, ms.Sys, ms.NumGC, ms.NextGC)
	pf("\n--- goroutine dump (debug=2, capped at %d bytes)\n", maxGoroutineDump)
	cw := &capWriter{w: w, left: maxGoroutineDump}
	if p := pprof.Lookup("goroutine"); p != nil {
		_ = p.WriteTo(cw, 2)
	}
	if cw.cut {
		pf("\n[truncated]\n")
	}
	pf("=== end of bundle ===\n")
}

// Emit writes the bundle to stderr and, when stateDir is writable, to a
// timestamped file there. It returns the file path, or "" if no file could be
// written. Old bundles beyond the newest ten are deleted so disk use is bounded.
func Emit(stateDir string, stderr io.Writer, in Info, log *slog.Logger) string {
	var file *os.File
	path := ""
	if stateDir != "" {
		name := "diag-" + in.Time.UTC().Format("20060102T150405Z") + ".txt"
		p := filepath.Join(stateDir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path is the operator-configured state dir plus a fixed name
		if err != nil {
			log.Error("cannot write diagnostic file", "dir", stateDir, "err", err)
		} else {
			file, path = f, p
		}
	}
	w := stderr
	if file != nil {
		w = io.MultiWriter(stderr, file)
	}
	WriteBundle(w, in)
	if file != nil {
		if err := file.Close(); err != nil {
			log.Error("closing diagnostic file", "err", err)
		}
		prune(stateDir, log)
	}
	return path
}

func prune(dir string, log *slog.Logger) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "diag-") && strings.HasSuffix(e.Name(), ".txt") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keepBundles {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			log.Warn("pruning old diagnostic bundle", "err", err)
		}
		names = names[1:]
	}
}
