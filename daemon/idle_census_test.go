package daemon

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
)

// FAE-HOST-022 with FAE-ARC-002: while the drive is idle and the daemon's
// process set is unchanged, the set-change detector sees nothing, yet the
// own-tree census still runs every observeIdleCensus. Memory growth in an
// unchanged idle tree is therefore caught within 60 s: a child that grows its
// resident memory past the bound, spawning nothing that outlives the growth,
// is reported at the first own-tree census and not before.
func TestIdleUnchangedTreeCensusesOwnTreeEveryMinute(t *testing.T) {
	grower := exec.Command("/bin/sh", "-c", `read go; x=$(head -c 67108864 /dev/zero | tr '\0' a); read stop`)
	grower.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := grower.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = grower.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-grower.Process.Pid, syscall.SIGKILL); _ = grower.Wait() }()
	clock := time.Now()
	d := &Daemon{}
	d.obs.now = func() time.Time { return clock }
	d.obs.limits = processowner.LeakLimits{Processes: 1 << 20, RSS: 32 << 20, Defunct: 1 << 20}
	// One observability round of an idle drive (observeRound's census step;
	// without a plane the alert itself is not recorded).
	round := func() leakScope {
		d.obs.driveIdleAt.Store(clock.UnixNano())
		_, scope := d.observeIdle()
		if scope != leakSkip {
			_ = d.detectLeaks(context.Background(), scope == leakHostWide)
		}
		return scope
	}
	grown := func() bool {
		r := d.processReport()
		for _, l := range r.Leaks {
			if l.Command == "sh" && l.Reason == "rss" {
				return true
			}
		}
		return false
	}
	tree := func() string {
		pids, err := processowner.DescendantPIDs(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		sort.Ints(pids)
		return fmt.Sprint(pids)
	}
	baseline := tree()
	if scope := round(); scope != leakHostWide || grown() {
		t.Fatalf("first idle round: scope=%d grown=%v", scope, grown())
	}
	if scope := round(); scope != leakSkip {
		t.Fatal("an unchanged idle tree was censused again at once", scope)
	}
	if _, err = fmt.Fprintln(input, "go"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); residentKiB(t, grower.Process.Pid) < 48<<10 || tree() != baseline; {
		if time.Now().After(deadline) {
			t.Fatal("child memory never grew with an unchanged tree", residentKiB(t, grower.Process.Pid), tree(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
	clock = clock.Add(observeIdleCensus - time.Second)
	if scope := round(); scope != leakSkip || grown() {
		t.Fatalf("own-tree census before its bound: scope=%d grown=%v", scope, grown())
	}
	clock = clock.Add(time.Second)
	if scope := round(); scope != leakOwnTree {
		t.Fatal("no own-tree census within observeIdleCensus of an unchanged idle tree", scope)
	}
	if !grown() {
		t.Fatalf("memory growth in an unchanged idle tree not caught: %+v", d.processReport().Commands)
	}
	clock = clock.Add(time.Second)
	if scope := round(); scope != leakSkip {
		t.Fatal("own-tree census repeated before its bound", scope)
	}
}

// residentKiB reads a process's VmRSS.
func residentKiB(t *testing.T, pid int) int {
	t.Helper()
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for s := bufio.NewScanner(f); s.Scan(); {
		if rest, ok := strings.CutPrefix(s.Text(), "VmRSS:"); ok {
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
			return n
		}
	}
	return 0
}
