package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCrashTurnHelper(t *testing.T) {
	path := os.Getenv("FAE_CRASH_TURN_CONFIG")
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err = json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "crash-work", AuthorizingMessage: "message"}, "crash-work"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, e := d.store.GetJob(context.Background(), "crash-work")
		var env envelope
		if e == nil && json.Unmarshal(s.Payload, &env) == nil && env.Job.InFlight != nil {
			proofs, e := d.live.guard.journal.Entries()
			if e == nil {
				for _, p := range proofs {
					if p.State == "launching" {
						records, _ := filepath.Glob(filepath.Join(d.layout.Root, "sessions", ".processes", "*.json"))
						if len(records) > 0 {
							// Publish the complete checkpoint atomically: the parent may
							// observe a newly created file before WriteFile finishes.
							if e = os.WriteFile(path+".ready.tmp", s.Payload, 0600); e != nil {
								t.Fatal(e)
							}
							if e = os.Rename(path+".ready.tmp", path+".ready"); e != nil {
								t.Fatal(e)
							}
							select {}
						}
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("turn did not launch")
}
func TestCrashDuringLiveTurnExplicitResolution(t *testing.T) {
	for _, action := range []string{"redispatch", "accept", "escalate"} {
		t.Run(action, func(t *testing.T) {
			cfg, ctx, _ := liveDaemonFixture(t)
			cfg.Live.Environment["LIVE_DAEMON_WAIT"] = "1"
			path := filepath.Join(filepath.Dir(cfg.Workspace), "crash-"+action+".json")
			b, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			exe, _ := os.Executable()
			cmd := exec.Command(exe, "-test.run=^TestCrashTurnHelper$")
			cmd.Env = append(os.Environ(), "FAE_CRASH_TURN_CONFIG="+path)
			log, err := os.Create(path + ".log")
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout = log
			cmd.Stderr = log
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			deadline := time.Now().Add(40 * time.Second)
			var before envelope
			for {
				b, err = os.ReadFile(path + ".ready")
				if err == nil {
					if err = json.Unmarshal(b, &before); err != nil {
						t.Fatal(err)
					}
					break
				}
				if time.Now().After(deadline) {
					out, _ := os.ReadFile(path + ".log")
					t.Fatalf("helper: %s", out)
				}
				time.Sleep(25 * time.Millisecond)
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()
			var d *Daemon
			deadline = time.Now().Add(8 * time.Second)
			for {
				d, err = Start(ctx, cfg)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond)
			}
			defer stop(t, d)
			s, err := d.store.GetJob(ctx, "crash-work")
			if err != nil {
				t.Fatal(err)
			}
			var after envelope
			json.Unmarshal(s.Payload, &after)
			if !after.Job.Parked || after.Job.Reason != "UncertainDispatch" || after.Job.InFlight.ID != before.Job.InFlight.ID || after.Job.Failures["crash-recovery"] != 1 || after.Job.Sequence != before.Job.Sequence {
				t.Fatalf("replayed/lost crash: %+v", after.Job)
			}
			proofs, err := d.live.guard.journal.Entries()
			if err != nil || len(proofs) != 0 {
				t.Fatal("unrecovered leases", proofs, err)
			}
			pool, err := d.live.guard.host.Snapshot(ctx)
			if err != nil || len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
				t.Fatal("leaked leases", pool, err)
			}
			if _, err = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); err != nil {
				t.Fatal(err)
			}
			p := ResolveUncertainParams{ID: "crash-work", DispatchID: before.Job.InFlight.ID, Action: action, AuthorizingMessage: "message"}
			if action == "accept" {
				r, e := engine.NewScripted(defaults.HappyScript()).Execute(ctx, *before.Job.InFlight)
				if e != nil {
					t.Fatal(e)
				}
				r.ExecutionKind = "live"
				p.Result = &r
			}
			bad := p
			bad.AuthorizingMessage = "missing"
			if _, err = call(t, d, "ResolveUncertain", bad, ""); err == nil {
				t.Fatal("accepted unaudited resolution")
			}
			if action != "escalate" {
				binding, e := d.live.manager.Ensure(ctx, after.Admission.RunID, after.Job.TaskID, d.live.config.Base)
				if e != nil {
					t.Fatal(e)
				}
				changed := filepath.Join(binding.Path, "unknown-effect.txt")
				if e = os.WriteFile(changed, []byte("uncertain effect"), 0600); e != nil {
					t.Fatal(e)
				}
				if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
					t.Fatal("accepted changed worktree")
				}
				if e = os.Remove(changed); e != nil {
					t.Fatal(e)
				}
			}
			bad = p
			bad.DispatchID = "stale"
			if _, err = call(t, d, "ResolveUncertain", bad, ""); err == nil {
				t.Fatal("accepted stale identity")
			}
			if _, err = call(t, d, "ResolveUncertain", p, ""); err != nil {
				t.Fatal(err)
			}
			if _, err = call(t, d, "ResolveUncertain", p, ""); err == nil {
				t.Fatal("accepted duplicate resolution")
			}
			s, err = d.store.GetJob(ctx, "crash-work")
			if err != nil {
				t.Fatal(err)
			}
			after = envelope{}
			json.Unmarshal(s.Payload, &after)
			if len(after.Job.ResolvedDispatches) != 1 || after.Job.ResolvedDispatches[0].ID != before.Job.InFlight.ID || after.Job.InFlight != nil || after.Job.Failures["crash-recovery"] != 1 {
				t.Fatal("resolution lost evidence", after.Job)
			}
			if action == "accept" && len(after.Job.Provenance) != 1 {
				t.Fatal("accept missing artifact")
			}
			if action == "escalate" && after.Job.Reason != "UncertainDispatchEscalated" {
				t.Fatal("not escalated")
			}
		})
	}
}
