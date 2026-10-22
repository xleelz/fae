package runtime

import (
	"context"
	"github.com/xleelz/fae/providerhealth"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// activity tracks only positive work, never terminal silence. An unreadable
// CPU census is unknown liveness and cannot authorize a stall cancellation.
type activity struct {
	cpu             time.Duration
	events, handoff int64
	known           bool
}

func cpuDuration(s string) (time.Duration, error) {
	var days int64
	if p := strings.Split(s, "-"); len(p) == 2 {
		d, e := strconv.ParseInt(p[0], 10, 64)
		if e != nil {
			return 0, e
		}
		days = d
		s = p[1]
	}
	p := strings.Split(s, ":")
	var seconds float64
	for _, v := range p {
		n, e := strconv.ParseFloat(v, 64)
		if e != nil {
			return 0, e
		}
		seconds = seconds*60 + n
	}
	return time.Duration((float64(days)*86400 + seconds) * float64(time.Second)), nil
}
func processCPU(ctx context.Context, root int) (time.Duration, bool) {
	b, e := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,time=").Output()
	if e != nil {
		return 0, false
	}
	type row struct {
		pid, parent int
		cpu         time.Duration
	}
	rows := []row{}
	selected := map[int]bool{root: true}
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		p := strings.Fields(line)
		if len(p) != 3 {
			continue
		}
		pid, e := strconv.Atoi(p[0])
		if e != nil {
			continue
		}
		parent, e := strconv.Atoi(p[1])
		if e != nil {
			continue
		}
		cpu, e := cpuDuration(p[2])
		if e != nil {
			return 0, false
		}
		rows = append(rows, row{pid, parent, cpu})
		if pid == root {
			found = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, r := range rows {
			if selected[r.parent] && !selected[r.pid] {
				selected[r.pid] = true
				changed = true
			}
		}
	}
	var total time.Duration
	for _, r := range rows {
		if selected[r.pid] {
			total += r.cpu
		}
	}
	return total, found
}
func sampleActivity(ctx context.Context, pid int, dir, handoff string) activity {
	cpu, known := processCPU(ctx, pid)
	a := activity{cpu: cpu, known: known}
	a.events = providerhealth.ActivityEvents(dir)
	if st, e := os.Lstat(handoff); e == nil && st.Mode().IsRegular() {
		a.handoff = st.Size()
	}
	return a
}
func (a activity) progressed(old activity) bool {
	return a.cpu > old.cpu || a.events > old.events || a.handoff > old.handoff
}
func monitorStall(ctx context.Context, pid func() int, dir, handoff string, limit, interval time.Duration, cancel context.CancelFunc, done chan<- bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	old := sampleActivity(ctx, pid(), dir, handoff)
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			done <- false
			return
		case now := <-ticker.C:
			next := sampleActivity(ctx, pid(), dir, handoff)
			if !next.known || !old.known || next.progressed(old) {
				last = now
			}
			old = next
			if now.Sub(last) >= limit {
				cancel()
				done <- true
				return
			}
		}
	}
}
