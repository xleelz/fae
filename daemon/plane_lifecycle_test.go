package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// FAE-CP-002: a graceful shutdown stops Dolt within the configured wait and
// logs its termination and duration.
func TestGracefulShutdownLogsDoltTermination(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.DoltStopWait = 7 * time.Second
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pid := d.child.PID()
	stop(t, d)
	raw, err := os.ReadFile(filepath.Join(cfg.Root, cfg.Instance, "supervision.log"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("Dolt termination pid=%d duration=", pid)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, want) {
			if !strings.Contains(line, "wait=7s forced=false already_exited=false error=<nil>") || !strings.Contains(line, "plane") {
				t.Fatalf("termination line %q", line)
			}
			if running(pid) {
				t.Fatal("Dolt survived shutdown")
			}
			return
		}
	}
	t.Fatalf("no Dolt termination line in the supervision log:\n%s", raw)
}

// running reports a live, non-zombie process.
func running(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return syscall.Kill(pid, 0) == nil
	}
	end := strings.LastIndexByte(string(raw), ')')
	return end < 0 || end+2 >= len(raw) || raw[end+2] != 'Z'
}

// processesMatching lists the pids whose arguments contain every fragment.
func processesMatching(t *testing.T, fragments ...string) []int {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	pids := []int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		matched := true
		for _, f := range fragments {
			matched = matched && strings.Contains(line, f)
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && matched {
			pids = append(pids, pid)
		}
	}
	return pids
}

// FAE-CP-003: an orphaned Dolt of the instance's own data directory, left
// running because its parent and watchdog both died, is reaped at startup
// with terminate-then-kill and an audit row; the foreign instance's Dolt is
// untouched.
func TestDaemonReapsUnwatchedDoltOrphanAndAuditsIt(t *testing.T) {
	cfg, ctx := fixture(t)
	foreignCfg := cfg
	foreignCfg.Instance = "foreign"
	foreign, err := Start(ctx, foreignCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, foreign)
	foreignDolt := foreign.child.PID()
	cfgPath := filepath.Join(filepath.Dir(cfg.Workspace), "unwatched-config.json")
	raw, _ := json.Marshal(cfg)
	if err = os.WriteFile(cfgPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	parent := exec.Command(exe, "-test.run=^TestDaemonOrphanSubprocess$")
	parent.Env = append(os.Environ(), "DAEMON_ORPHAN_FIXTURE="+cfgPath)
	if err = parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { parent.Process.Kill(); parent.Wait() }()
	var pids []int
	deadline := time.Now().Add(60 * time.Second)
	for {
		if raw, err = os.ReadFile(cfgPath + ".ready"); err == nil {
			if err = json.Unmarshal(raw, &pids); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper not ready")
		}
		time.Sleep(30 * time.Millisecond)
	}
	orphan := pids[0]
	defer syscall.Kill(-orphan, syscall.SIGKILL)
	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	watchers := processesMatching(t, "--fae-internal-process-watch", filepath.Join(layout.Run, "plane-processes"))
	if len(watchers) == 0 {
		t.Fatal("no plane watchdog to disable")
	}
	for _, w := range watchers {
		_ = syscall.Kill(w, syscall.SIGKILL)
	}
	if err = parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	parent.Wait()
	time.Sleep(500 * time.Millisecond)
	if !running(orphan) {
		t.Fatal("planted Dolt orphan did not survive its parent")
	}
	recovered, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, recovered)
	if running(orphan) {
		t.Fatal("own Dolt orphan not reaped")
	}
	if recovered.child.PID() == orphan || !running(foreignDolt) {
		t.Fatal("adopted the orphan or disturbed the foreign Dolt")
	}
	if _, err = call(t, foreign, "Status", nil, ""); err != nil {
		t.Fatal("foreign instance disturbed:", err)
	}
	if n := countRows(t, recovered, `SELECT COUNT(*) FROM audit WHERE action='owned Dolt orphan recovered' AND inputs LIKE '%`+strconv.Itoa(orphan)+`%'`); n != 1 {
		t.Fatal("orphan reap not audited", n)
	}
}
