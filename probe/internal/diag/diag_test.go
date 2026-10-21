package diag

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/sysinfo"
)

func TestRingKeepsTheLastN(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 5; i++ {
		r.Add(time.Unix(int64(i), 0), "k", fmt.Sprint(i))
	}
	got := r.Snapshot()
	if len(got) != 3 || got[0].Msg != "2" || got[2].Msg != "4" {
		t.Fatalf("%v", got)
	}
	r2 := NewRing(50)
	r2.Add(time.Now(), "k", strings.Repeat("x", 1000))
	if len(r2.Snapshot()[0].Msg) != 300 {
		t.Fatal("messages must be capped")
	}
}

func TestBundleHasTheEvidenceTheSpecListsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	ring := NewRing(50)
	ring.Add(time.Now(), "accept", "accept tcp: too many open files")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var stderr bytes.Buffer
	var path string
	for i := 0; i < 13; i++ {
		path = Emit(dir, &stderr, Info{
			Reason: "test", Time: time.Unix(1700000000+int64(i), 0), Version: "v", Commit: "c",
			ActiveConns: 42, SelftestFails: 3, SelftestErrors: []string{"dial: timeout"}, Errors: ring,
		}, log)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"active_connections: 42", "open_fds:", "fd_limit", "goroutine dump", "dial: timeout",
		"too many open files", "heap_alloc=", "goroutine "} {
		if !strings.Contains(string(b), want) {
			t.Errorf("bundle lacks %q", want)
		}
	}
	if !strings.Contains(stderr.String(), "ondrya-probe diagnostic bundle") {
		t.Fatal("bundle must also go to stderr")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "diag-*.txt"))
	if len(files) != keepBundles {
		t.Fatalf("kept %d bundles, want %d", len(files), keepBundles)
	}
}

func TestUnwritableDirStillWritesStderr(t *testing.T) {
	var stderr bytes.Buffer
	p := Emit("/proc/nonexistent", &stderr, Info{Time: time.Now(), Reason: "x"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if p != "" || !strings.Contains(stderr.String(), "goroutine dump") {
		t.Fatalf("path %q", p)
	}
}

func TestSpareDescriptorsAreHeldAndReleased(t *testing.T) {
	before, _ := sysinfo.OpenFDs()
	s := NewSpare(3)
	during, _ := sysinfo.OpenFDs()
	if s.Held() != 3 || during != before+3 {
		t.Fatalf("held %d, fds %d -> %d", s.Held(), before, during)
	}
	s.Release()
	s.Release() // idempotent
	after, _ := sysinfo.OpenFDs()
	if s.Held() != 0 || after != before {
		t.Fatalf("not released: held %d, fds %d -> %d", s.Held(), before, after)
	}
}
