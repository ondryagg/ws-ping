package crashloop

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTripsAfterMoreThanFiveExitsInTenMinutes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := New(t.TempDir(), func() time.Time { return now })
	for i := 0; i < 5; i++ {
		if err := g.Record(); err != nil {
			t.Fatal(err)
		}
		now = now.Add(30 * time.Second)
	}
	if g.Tripped() || g.Count() != 5 {
		t.Fatalf("5 exits must not trip: %d", g.Count())
	}
	if err := g.Record(); err != nil {
		t.Fatal(err)
	}
	if !g.Tripped() {
		t.Fatal("6 exits in the window must trip")
	}
	now = now.Add(Window)
	if g.Tripped() || g.Count() != 0 {
		t.Fatal("old exits must age out")
	}
}

func TestResetAndGarbageTolerance(t *testing.T) {
	dir := t.TempDir()
	g := New(dir, time.Now)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("junk\n\n12x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if g.Count() != 0 {
		t.Fatal("garbage lines are ignored")
	}
	_ = g.Record()
	if err := g.Reset(); err != nil || g.Count() != 0 {
		t.Fatal("reset")
	}
	if err := g.Reset(); err != nil {
		t.Fatal("reset of a missing file is fine")
	}
}

func TestUnwritableStateDirIsAnErrorNotAPanic(t *testing.T) {
	g := New("/proc/nonexistent", time.Now)
	if g.Record() == nil {
		t.Fatal("expected an error")
	}
	if g.Tripped() {
		t.Fatal("no state means not tripped")
	}
}

func TestPlantedSymlinkAtTheTempNameIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := New(dir, time.Now)
	if err := os.Symlink(victim, g.path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := g.Record(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("symlink target was overwritten: %q", b)
	}
	if g.Count() != 1 {
		t.Fatal("record must still be written")
	}
}
