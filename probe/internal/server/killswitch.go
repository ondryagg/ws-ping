package server

import (
	"errors"
	"io/fs"
	"os"
	"sync/atomic"
	"time"

	"github.com/ondryadev/ws-ping/probe/internal/limiter"
)

// killSwitch is on when --maintenance is set or the disable file exists. The
// file is stat'ed at most once a second.
type killSwitch struct {
	maintenance bool
	file        string
	now         limiter.Clock
	last        atomic.Int64
	present     atomic.Bool
}

const killSwitchTTL = time.Second

func newKillSwitch(maintenance bool, file string, now limiter.Clock) *killSwitch {
	return &killSwitch{maintenance: maintenance, file: file, now: now}
}

func (k *killSwitch) Active() bool {
	if k.maintenance {
		return true
	}
	if k.file == "" {
		return false
	}
	t := k.now().UnixNano()
	if l := k.last.Load(); l == 0 || t-l >= int64(killSwitchTTL) {
		k.last.Store(t)
		_, err := os.Stat(k.file)
		switch {
		case err == nil:
			k.present.Store(true)
		case errors.Is(err, fs.ErrNotExist):
			k.present.Store(false)
		default:
			// EACCES, EIO and the like say nothing about the file; keep the
			// last known state instead of silently switching the kill switch off.
		}
	}
	return k.present.Load()
}
