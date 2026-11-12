package daemon

import (
	"github.com/xleelz/fae/scratchfs"
	"path/filepath"
	"time"
)

// The reaper is paged. Keep making progress after a crash even when no new
// task is launched, without placing filesystem cleanup on the dispatch lock.
func (d *Daemon) maintainSessionCopies() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	root := filepath.Join(d.layout.Root, "sessions")
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if err := scratchfs.ReapSessionCopies(root); err != nil {
				d.logLine("scratch", "session copy cleanup deferred: %v\n", err)
			}
		}
	}
}
