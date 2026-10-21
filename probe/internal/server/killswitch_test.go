package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKillSwitchFileIsCheckedAtMostOncePerSecond(t *testing.T) {
	f := filepath.Join(t.TempDir(), "disabled")
	now := time.Unix(1000, 0)
	k := newKillSwitch(false, f, func() time.Time { return now })
	if k.Active() {
		t.Fatal("inactive without the file")
	}
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if k.Active() {
		t.Fatal("cached for one second")
	}
	now = now.Add(1100 * time.Millisecond)
	if !k.Active() {
		t.Fatal("must notice the file after the TTL")
	}
	_ = os.Remove(f)
	now = now.Add(1100 * time.Millisecond)
	if k.Active() {
		t.Fatal("must notice removal")
	}
	if !newKillSwitch(true, "", time.Now).Active() {
		t.Fatal("--maintenance always active")
	}
}

func TestKillSwitchKeepsItsStateWhenTheFileCannotBeChecked(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "disabled")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	k := newKillSwitch(false, f, func() time.Time { return now })
	if !k.Active() {
		t.Fatal("active while the file exists")
	}
	// A path below a regular file fails with ENOTDIR, not ENOENT: unknown, so keep the state.
	k.file = filepath.Join(f, "x")
	now = now.Add(2 * time.Second)
	if !k.Active() {
		t.Fatal("an unexpected stat error must not switch the kill switch off")
	}
}
