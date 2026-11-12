package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/processowner"
)

// sqlCounter counts client commands (MySQL packets with sequence id 0) on
// every plane connection: each one is a SQL round trip.
type sqlCounter struct {
	commands atomic.Int64
	mu       sync.Mutex
	texts    map[string]int
}

type countingConn struct {
	net.Conn
	c       *sqlCounter
	mu      sync.Mutex
	pending []byte
	skip    int
	capture int
	text    []byte
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	for _, b := range p {
		if c.skip > 0 {
			c.skip--
			if c.capture > 0 {
				c.text = append(c.text, b)
				if c.skip == 0 || len(c.text) >= 90 {
					c.c.mu.Lock()
					if c.c.texts == nil {
						c.c.texts = map[string]int{}
					}
					c.c.texts[string(c.text)]++
					c.c.mu.Unlock()
					c.capture, c.text = 0, nil
				}
			}
			continue
		}
		c.pending = append(c.pending, b)
		if len(c.pending) == 4 {
			size := int(c.pending[0]) | int(c.pending[1])<<8 | int(c.pending[2])<<16
			if c.pending[3] == 0 {
				c.c.commands.Add(1)
				c.capture = size
			}
			c.pending, c.skip = c.pending[:0], size
		}
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

// countPlaneSQL routes every new plane connection through a counter until
// the test ends. Connections opened before it are not counted.
func countPlaneSQL(t *testing.T) *sqlCounter {
	t.Helper()
	counter := &sqlCounter{}
	mysql.RegisterDialContext("tcp", func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return &countingConn{Conn: conn, c: counter}, nil
	})
	t.Cleanup(func() { mysql.DeregisterDialContext("tcp") })
	return counter
}

// FAE-ARC-002: an idle daemon (no admitted work, no schedule due) blocks on
// events. Over an idle window it makes no plane round trip and, measured from
// process accounting, accrues CPU within the acceptance rate of 1 s per 10
// minutes. The window is 30 s (bound 50 ms, no round trip);
// FAE_TEST_IDLE_FULL=1 measures the acceptance's full 10 minutes (bound 1 s,
// one idle plane repair round allowed; run it with -timeout 15m). The figure
// includes the test binary hosting the daemon; the Dolt child's CPU is logged,
// not bounded.
func TestIdleDaemonMakesNoSQLRoundTrips(t *testing.T) {
	window := 30 * time.Second
	if os.Getenv("FAE_TEST_IDLE_FULL") == "1" {
		window = 10 * time.Minute
	}
	bound := window / 600
	cfg, ctx := fixture(t)
	counter := countPlaneSQL(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	if counter.commands.Load() == 0 {
		t.Fatal("plane connections were not counted")
	}
	// Leftover processes of earlier tests in this binary must not raise leak
	// alerts here; the window starts once the drive has judged itself idle.
	d.obs.mu.Lock()
	d.obs.limits = processowner.LeakLimits{Processes: 1 << 20, RSS: 1 << 50, Defunct: 1 << 20}
	d.obs.mu.Unlock()
	time.Sleep(12 * time.Second)
	if !d.driveIdle(time.Now()) {
		t.Fatal("drive never judged the idle daemon idle")
	}
	d.childMu.RLock()
	dolt := d.child.PID()
	d.childMu.RUnlock()
	before := counter.commands.Load()
	var ru0, ru1 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	dolt0 := procCPU(dolt)
	counter.mu.Lock()
	counter.texts = nil
	counter.mu.Unlock()
	time.Sleep(window)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	dolt1 := procCPU(dolt)
	cpu := time.Duration(ru1.Utime.Nano()+ru1.Stime.Nano()-ru0.Utime.Nano()-ru0.Stime.Nano()) * time.Nanosecond
	t.Logf("idle daemon CPU over %s: %s (FAE-ARC-002 bound %s: 1 s per 10 min); Dolt child: %s", window, cpu, bound, dolt1-dolt0)
	counter.mu.Lock()
	for k, v := range counter.texts {
		t.Logf("%5d %q", v, k)
	}
	counter.mu.Unlock()
	idle := counter.commands.Load() - before
	t.Logf("idle plane round trips over %s: %d", window, idle)
	// Idle plane steps still run once per observeIdleRepair, a bounded repair
	// rather than polling: such a round reads the backlog, its edges and the
	// alerts (5 commands), so a full window may hold one.
	if allowed := int64(window/observeIdleRepair) * 10; idle > allowed {
		t.Fatalf("idle daemon made %d plane round trips in %s", idle, window)
	}
	if cpu > bound {
		t.Fatalf("idle daemon used %s of CPU in %s, over the %s acceptance rate", cpu, window, bound)
	}
	// FAE-ROB-020: well past the staleness bound, the cached reads answer
	// within it.
	assertFreshCachedReads(t, d)
}

// FAE-ARC-002: an idle daemon does no periodic snapshot or leak-census work.
// Nothing re-snapshots its status until a cached read asks, and that read is
// answered fresh; an idle observability round with an unchanged tree runs no
// leak census, while a new process in the tree is censused at once
// (FAE-HOST-022).
func TestIdleDaemonSnapshotsAndCensusesOnlyOnDemand(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.obs.mu.Lock()
	d.obs.limits = processowner.LeakLimits{Processes: 1 << 20, RSS: 1 << 50, Defunct: 1 << 20}
	d.obs.mu.Unlock()
	time.Sleep(12 * time.Second)
	if !d.driveIdle(time.Now()) {
		t.Fatal("drive never judged the idle daemon idle")
	}
	updated := func() time.Time { s, _ := d.status.Load().(statusSnapshot); return s.Updated }
	idleSince := updated()
	time.Sleep(statusRefreshAge + 2*driveRoundPeriod)
	if !updated().Equal(idleSince) {
		t.Fatal("idle daemon re-snapshotted its status with no reader")
	}
	assertFreshCachedReads(t, d)
	if !updated().After(idleSince) {
		t.Fatal("cached read did not refresh the idle snapshot")
	}
	report := func() *processowner.LeakReport { d.obs.mu.Lock(); defer d.obs.mu.Unlock(); return d.obs.leaks }
	censused := true
	for i := 0; i < 5 && censused; i++ {
		d.observeRound(ctx)
		before := report()
		d.observeRound(ctx)
		censused = report() != before
	}
	if censused {
		t.Fatal("idle rounds with an unchanged tree ran the leak census")
	}
	child := exec.Command("/bin/sleep", "60")
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	before := report()
	d.observeRound(ctx)
	if report() == before {
		t.Fatal("a new process in the idle daemon's tree was not censused")
	}
}

// procCPU is a process's user plus system CPU time from /proc (0 when gone).
func procCPU(pid int) time.Duration {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
	if len(fields) < 13 {
		return 0
	}
	user, _ := strconv.ParseInt(fields[11], 10, 64)
	system, _ := strconv.ParseInt(fields[12], 10, 64)
	return time.Duration(user+system) * 10 * time.Millisecond
}

// assertFreshCachedReads asks Board, Schedule and Controller, which are served
// from the published snapshot and refused once it is older than 10 s.
func assertFreshCachedReads(t *testing.T, d *Daemon) {
	t.Helper()
	reply, err := call(t, d, "Board", map[string]any{}, "")
	if err != nil {
		t.Fatal("Board:", err)
	}
	raw, _ := json.Marshal(reply.Result)
	var page BoardPage
	if err = json.Unmarshal(raw, &page); err != nil || page.SnapshotAgeMillis > 10000 {
		t.Fatalf("Board page %s %v", raw, err)
	}
	if _, err = call(t, d, "Schedule", ScheduleParams{Action: "list"}, ""); err != nil {
		t.Fatal("Schedule:", err)
	}
	if _, err = call(t, d, "Controller", controller.Params{Action: "health"}, ""); err != nil {
		t.Fatal("Controller:", err)
	}
}

// FAE-ROB-020: a stopped drive runs no rounds, yet the cached reads stay
// fresh past their 10 s bound.
func TestCachedReadsStayFreshWhileDriveStopped(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for d.driveAlive.Load() {
		if time.Now().After(deadline) {
			t.Fatal("drive never stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(12 * time.Second)
	assertFreshCachedReads(t, d)
}
