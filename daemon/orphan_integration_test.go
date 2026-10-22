package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
)

func TestDaemonOrphanSubprocess(t *testing.T) {
	path := os.Getenv("FAE_DAEMON_ORPHAN_FIXTURE")
	if path == "" {
		return
	}
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var cfg Config
	if e = json.Unmarshal(b, &cfg); e != nil {
		t.Fatal(e)
	}
	if e = os.Chdir(cfg.Workspace); e != nil {
		t.Fatal(e)
	}
	d, e := Start(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	// A representative host-command group lives alongside the real private Dolt.
	command := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	r, e := processowner.Start(command, filepath.Join(d.layout.Run, "effect-processes"), d.layout.Worktrees)
	if e != nil {
		t.Fatal(e)
	}
	ready, _ := json.Marshal([]int{d.child.PID(), r.PID})
	if e = os.WriteFile(path+".ready", ready, 0600); e != nil {
		t.Fatal(e)
	}
	select {}
}
func TestDaemonSIGKILLOrphansRecoveredAndForeignInstancePreserved(t *testing.T) {
	cfg, ctx := fixture(t)
	foreignCfg := cfg
	foreignCfg.Instance = "foreign"
	foreign, e := Start(ctx, foreignCfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, foreign)
	cfgPath := filepath.Join(filepath.Dir(cfg.Workspace), "orphan-config.json")
	b, marshalErr := json.Marshal(cfg)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if e = os.WriteFile(cfgPath, b, 0600); e != nil {
		t.Fatal(e)
	}
	exe, _ := os.Executable()
	parent := exec.Command(exe, "-test.run=^TestDaemonOrphanSubprocess$")
	parent.Env = append(os.Environ(), "FAE_DAEMON_ORPHAN_FIXTURE="+cfgPath)
	log, e := os.Create(cfgPath + ".log")
	if e != nil {
		t.Fatal(e)
	}
	defer log.Close()
	parent.Stdout = log
	parent.Stderr = log
	if e = parent.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { parent.Process.Kill(); parent.Wait() }()
	var pids []int
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, e = os.ReadFile(cfgPath + ".ready")
		if e == nil {
			if e = json.Unmarshal(b, &pids); e != nil {
				t.Fatal(e)
			}
			break
		}
		if time.Now().After(deadline) {
			log.Sync()
			output, _ := os.ReadFile(cfgPath + ".log")
			t.Fatalf("helper not ready: %s", output)
		}
		time.Sleep(30 * time.Millisecond)
	}
	defer func() {
		for _, pid := range pids {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
	if e = parent.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	parent.Wait()
	deadline = time.Now().Add(8 * time.Second)
	for {
		alive := false
		out, probeErr := exec.Command("ps", "-axo", "pgid=,stat=").Output()
		if probeErr != nil {
			t.Fatal(probeErr)
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			group, e := strconv.Atoi(fields[0])
			if e != nil {
				t.Fatal(e)
			}
			for _, pid := range pids {
				if group == pid && !strings.HasPrefix(fields[1], "Z") {
					alive = true
				}
			}
		}

		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGKILL left owned leaders alive: %v", pids)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, e = call(t, foreign, "Status", nil, ""); e != nil {
		t.Fatalf("foreign instance disturbed: %v", e)
	}
	recovered, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, recovered)
	if _, e = call(t, recovered, "Status", nil, ""); e != nil {
		t.Fatal(e)
	}
	receipts, e := filepath.Glob(filepath.Join(recovered.layout.Run, "plane-processes", "*.retired"))
	if e != nil || len(receipts) == 0 {
		t.Fatalf("missing Dolt retirement audit receipt: %v %v", receipts, e)
	}
	if recovered.child.PID() == pids[0] {
		t.Fatal("adopted former child")
	}
}
