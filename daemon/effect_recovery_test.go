package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commitJob(t *testing.T, tree, base string) engine.Job { return effectJob(t, tree, base, "commit") }
func effectJob(t *testing.T, tree, base, target string) engine.Job {
	t.Helper()
	j, e := engine.NewLive("effect-work", 1, defaults.Workflow(), tree)
	if e != nil {
		t.Fatal(e)
	}
	j.Base, j.Disposition = base, domain.Disposition{Mode: domain.Local}
	runner := engine.NewScripted(defaults.HappyScript())
	for n := 0; n < 100; n++ {
		var dispatch *engine.Dispatch
		j, dispatch, _, e = engine.Prepare(j)
		if e != nil {
			t.Fatal(e)
		}
		if dispatch == nil {
			continue
		}
		if dispatch.Stage == target {
			return j
		}
		r, e := runner.Execute(context.Background(), *dispatch)
		if e != nil {
			t.Fatal(e)
		}
		r.ExecutionKind = "live"
		j, _, e = engine.Complete(j, dispatch.ID, r)
		if e != nil {
			t.Fatal(e)
		}
	}
	t.Fatal("commit stage not reached", j.State, j.Reason)
	return j
}
func TestEffectCrashHelper(t *testing.T) {
	path := os.Getenv("EFFECT_CRASH")
	if path == "" {
		return
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var cfg Config
	if e = json.Unmarshal(raw, &cfg); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	authorize(t, d)
	w, e := d.live.manager.Ensure(ctx, "effect-run", "effect-work", cfg.Live.Base)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(w.Path, "answer.go"), []byte("package sample\n\nfunc answer() int { return 42 }\n"), 0600)
	os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(liveTestSource), 0600)
	snapshot, e := d.live.broker.Snapshot(ctx, w, cfg.Live.Files)
	if e != nil {
		t.Fatal(e)
	}
	phase := os.Getenv("EFFECT_PHASE")
	tampered := strings.HasSuffix(phase, "-tampered")
	phase = strings.TrimSuffix(phase, "-tampered")
	target := "commit"
	if strings.HasPrefix(phase, "host-") {
		target = "format"
	}
	j := effectJob(t, snapshot.Tree, snapshot.Base, target)
	if strings.HasPrefix(phase, "integrate-") {
		commit, e := d.live.broker.Commit(ctx, w, snapshot, cfg.Live.CommitMessage)
		if e != nil {
			t.Fatal(e)
		}
		output, _ := json.Marshal(map[string]any{"sha": commit.SHA, "files": commit.Files})
		dispatch := *j.InFlight
		j, _, e = engine.Complete(j, dispatch.ID, engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: j.TaskID, Tree: j.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: output})
		if e != nil {
			t.Fatal(e)
		}
		for j.InFlight == nil {
			j, _, _, e = engine.Prepare(j)
			if e != nil {
				t.Fatal(e)
			}
		}
		if j.InFlight.Config.Primitive != workflow.Integrate {
			t.Fatal("not integration", j.State)
		}
	}
	env := envelope{Job: j, Admission: AdmitParams{ID: j.TaskID, RunID: "effect-run", AuthorizingMessage: "message"}}
	payload, _ := json.Marshal(env)
	stored, e := d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "admit effect boundary fixture", Outcome: "ok"}, "effect-fixture", controlplane.JobSnapshot{ID: j.TaskID, WorkflowID: j.Document.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: string(j.State), Payload: payload})
	if e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	d.jobs[j.TaskID] = stored
	d.mu.Unlock()
	dispatch := *j.InFlight
	if strings.HasSuffix(phase, "completed") {
		if _, e = executeAdmitted(t, ctx, d.live, dispatch); e != nil {
			t.Fatal(e)
		}
	} else {
		binding := execution.Binding{Worktree: w.Path, Base: j.Base, Document: j.Document, Mode: "local"}
		if _, e = admitted(t, "effect admission", func() (func() error, error) {
			return d.live.guard.Acquire(ctx, dispatch, binding, dispatch.Sessions[0])
		}); e != nil {
			t.Fatal(e)
		}
		if e = d.live.guard.Launching(ctx, dispatch.Sessions[0].ID); e != nil {
			t.Fatal(e)
		}
		broker := *d.live.broker
		broker.ProcessSession = dispatch.Sessions[0].ID
		if strings.HasPrefix(phase, "host-") {
			if _, e = d.live.effectIntent(ctx, dispatch, w, nil, nil); e != nil {
				t.Fatal(e)
			}
			if _, e = broker.Run(ctx, w, effects.Command{Argv: dispatch.Config.Command, Timeout: time.Minute}); e != nil {
				t.Fatal(e)
			}
		} else if strings.HasPrefix(phase, "integrate-") {
			if _, e = d.live.effectIntent(ctx, dispatch, w, nil, nil); e != nil {
				t.Fatal(e)
			}
			sha := liveGit(t, w.Path, "rev-parse", "HEAD")
			if _, e = broker.Integrate(ctx, w, effects.CommitEvidence{SHA: sha, Tree: dispatch.Tree, Files: cfg.Live.Files}, cfg.Live.IntegrationRef, dispatch.Base); e != nil {
				t.Fatal(e)
			}
		} else {
			prepared, e := broker.PrepareCommit(ctx, w, snapshot, cfg.Live.CommitMessage+"\n\nFae dispatch: "+dispatch.ID)
			if e != nil {
				t.Fatal(e)
			}
			if phase == "prepared" {
				intent := effectIntent{Dispatch: dispatch, Worktree: w, Head: snapshot.Base, TargetRef: "refs/heads/" + w.Branch, TargetBefore: snapshot.Base, TargetAfter: prepared.SHA, Commit: &prepared}
				raw, _ := json.Marshal(intent)
				if e = d.store.RecordEffect(ctx, effectKey(dispatch)+"/intent", j.TaskID, "intent", raw); e != nil {
					t.Fatal(e)
				}
			} else {
				if _, e = d.live.effectIntent(ctx, dispatch, w, &prepared, nil); e != nil {
					t.Fatal(e)
				}
				// Establish broker-owned terminal process evidence, then materialize
				// the persisted post-CAS/pre-index state before the parent kills us.
				// Missing receipts still require genuine broker retirement proof.
				if _, e = broker.ApplyCommit(ctx, w, prepared); e != nil {
					t.Fatal(e)
				}
				if _, e = effectGit(ctx, w, "read-tree", prepared.Snapshot.Base); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
	if tampered {
		// A changed tree must defeat automatic receipt adoption at recovery.
		os.WriteFile(filepath.Join(w.Path, "foreign"), []byte("changed"), 0600)
	}
	if e = writeReady(path+".ready", payload); e != nil {
		t.Fatal(e)
	}
	select {}
}
func TestEffectSIGKILLBoundariesAndAuditedAdoption(t *testing.T) {
	for _, phase := range []string{"prepared", "post-cas", "completed", "completed-tampered", "host-completed", "host-unrecorded", "integrate-post-cas", "integrate-completed"} {
		t.Run(phase, func(t *testing.T) {
			cfg, ctx, _ := liveDaemonFixture(t)
			path := filepath.Join(filepath.Dir(cfg.Workspace), "effect-crash.json")
			raw, _ := json.Marshal(cfg)
			os.WriteFile(path, raw, 0600)
			helper := startSubprocess(t, "TestEffectCrashHelper", path+".log", "EFFECT_CRASH="+path, "EFFECT_PHASE="+phase)
			var e error
			helper.await(t, 30*time.Second+pressureAllowance, func() bool {
				raw, e = os.ReadFile(path + ".ready")
				return e == nil
			})
			var prior envelope
			json.Unmarshal(raw, &prior)
			helper.kill()
			var d *Daemon
			// The killed helper's process watchdog stops its Dolt asynchronously;
			// a loaded host can take a while, so this restart guard is generous.
			deadline := time.Now().Add(30 * time.Second)
			for {
				d, e = Start(ctx, cfg)
				if e == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(e)
				}
				time.Sleep(50 * time.Millisecond)
			}
			defer stop(t, d)
			if _, e = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); e != nil {
				t.Fatal(e)
			}
			dispatchID := prior.Job.InFlight.ID
			recovered, e := d.store.GetJob(ctx, prior.Job.TaskID)
			if e != nil {
				t.Fatal(e)
			}
			var after envelope
			json.Unmarshal(recovered.Payload, &after)
			if phase == "completed" || phase == "host-completed" || phase == "integrate-completed" {
				// FAE-ROB-012/-016: a completed daemon receipt with exact
				// postconditions is adopted at recovery: nothing re-runs, no
				// budget is charged and no operator accept is needed.
				adopted, provenance := false, false
				for _, h := range after.Job.History {
					adopted = adopted || h.Kind == "effect-receipt-adopted" && h.DispatchID == dispatchID
				}
				for _, p := range after.Job.Provenance {
					provenance = provenance || p.DispatchID == dispatchID
				}
				if !adopted || !provenance || after.Job.Failures["crash-recovery"] != 0 || len(after.Job.ResolvedDispatches) != 1 || (after.Job.InFlight != nil && after.Job.InFlight.ID == dispatchID) {
					t.Fatal("completed receipt not adopted at recovery", after.Job.State, after.Job.Parked, after.Job.Reason, after.Job.Failures)
				}
				if _, e = call(t, d, "ResolveUncertain", ResolveUncertainParams{ID: prior.Job.TaskID, DispatchID: dispatchID, Action: "accept", AuthorizingMessage: "message"}, ""); e == nil {
					t.Fatal("adopted effect still resolvable")
				}
				return
			}
			if phase == "host-unrecorded" {
				// FAE-ROB-013: an interrupted QA run is infrastructure. With its
				// workload proven retired on an unchanged tree it re-runs at
				// recovery, without a failure verdict or an operator verb; the
				// crash consumes only dispatch-recovery budget.
				rerun := false
				for _, h := range after.Job.History {
					rerun = rerun || h.Kind == "crash-rerun" && h.DispatchID == dispatchID
					if h.Kind == "failure" || h.Kind == "park" && h.DispatchID == dispatchID {
						t.Fatal("interrupted QA charged a failure or parked", h)
					}
				}
				if !rerun || after.Job.State == "Escalated" || after.Job.Failures["dispatch-recovery"] != 1 || after.Job.Failures["crash-recovery"] != 0 || len(after.Job.ResolvedDispatches) != 1 || after.Job.ResolvedDispatches[0].ID != dispatchID || (after.Job.InFlight != nil && after.Job.InFlight.ID == dispatchID) {
					t.Fatal("interrupted QA not re-run at recovery", after.Job.State, after.Job.Parked, after.Job.Reason, after.Job.Failures)
				}
				if proofs, e := d.live.guard.journal.Entries(); e != nil || len(proofs) != 0 {
					t.Fatal("unreleased effect lease", proofs, e)
				}
				return
			}
			// Without a completed receipt, or with changed postconditions, the
			// conservative rule holds: the effect parks for the operator.
			if !after.Job.Parked || after.Job.Reason != "UncertainDispatch" || after.Job.InFlight == nil || after.Job.InFlight.ID != dispatchID {
				t.Fatal("unproven effect not parked", after.Job.State, after.Job.Parked, after.Job.Reason)
			}
			action := "accept"
			if phase == "prepared" {
				action = "redispatch"
			}
			p := ResolveUncertainParams{ID: prior.Job.TaskID, DispatchID: prior.Job.InFlight.ID, Action: action, AuthorizingMessage: "message"}
			if phase == "prepared" {
				key := effectKey(*prior.Job.InFlight) + "/intent"
				original, e := d.store.GetMarker(ctx, key)
				if e != nil {
					t.Fatal(e)
				}
				if e = d.store.RecordEffect(ctx, key, p.ID, "intent", original); e != nil {
					t.Fatal("idempotent immutable receipt", e)
				}
				if e = d.store.RecordEffect(ctx, key, p.ID, "intent", json.RawMessage(`{}`)); e == nil {
					t.Fatal("rewrote immutable intent")
				}
				var foreign effectIntent
				json.Unmarshal(original, &foreign)
				foreign.Dispatch.TaskID = "foreign"
				changed, _ := json.Marshal(foreign)
				audit := controlplane.Audit{Actor: "test", Action: "inject malformed intent", Outcome: "fixture"}
				if e = d.store.PutMarker(ctx, audit, key, changed); e != nil {
					t.Fatal(e)
				}
				if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
					t.Fatal("foreign intent authorized redispatch")
				}
				if e = d.store.PutMarker(ctx, audit, key, original); e != nil {
					t.Fatal(e)
				}
			}

			bad := p
			bad.Result = &engine.Result{}
			if _, e = call(t, d, "ResolveUncertain", bad, ""); e == nil {
				t.Fatal("trusted caller JSON")
			}
			if phase != "prepared" {
				replay := p
				replay.Action = "redispatch"
				if _, e = call(t, d, "ResolveUncertain", replay, ""); phase != "host-unrecorded" && e == nil {
					t.Fatal("blindly replayed effect")
				}
				if phase == "host-unrecorded" {
					// FAE-ROB-013: an interrupted QA run is infrastructure and
					// re-runs on the unchanged tree without a failure verdict.
					if e != nil {
						t.Fatal("interrupted host command not re-runnable", e)
					}
					stored, e := d.store.GetJob(ctx, p.ID)
					if e != nil {
						t.Fatal(e)
					}
					var rerun envelope
					json.Unmarshal(stored.Payload, &rerun)
					if rerun.Job.InFlight != nil || rerun.Job.State == "Escalated" || len(rerun.Job.ResolvedDispatches) != 1 || rerun.Job.Failures[rerun.Job.ResolvedDispatches[0].Stage] != 0 {
						t.Fatal("interrupted QA charged a failure or lost its record", rerun.Job.State, rerun.Job.Failures)
					}
					if proofs, e := d.live.guard.journal.Entries(); e != nil || len(proofs) != 0 {
						t.Fatal("unreleased effect lease", proofs, e)
					}
					return
				}
				w, e := d.live.manager.Ensure(ctx, "effect-run", p.ID, cfg.Live.Base)
				if e != nil {
					t.Fatal(e)
				}
				os.WriteFile(filepath.Join(w.Path, "foreign"), []byte("changed"), 0600)
				if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
					t.Fatal("adopted changed tree")
				}
				os.Remove(filepath.Join(w.Path, "foreign"))
			}
			if strings.HasPrefix(phase, "integrate-") {
				w, e := d.live.manager.Ensure(ctx, "effect-run", p.ID, cfg.Live.Base)
				if e != nil {
					t.Fatal(e)
				}
				tip := liveGit(t, w.Path, "rev-parse", cfg.Live.IntegrationRef)
				liveGit(t, w.Path, "update-ref", cfg.Live.IntegrationRef, cfg.Live.Base, tip)
				if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
					t.Fatal("adopted changed integration ref")
				}
				liveGit(t, w.Path, "update-ref", cfg.Live.IntegrationRef, tip, cfg.Live.Base)
			}
			if phase == "host-unrecorded" {
				if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
					t.Fatal("fabricated missing host result")
				}
				p.Action = "escalate"
			}
			if _, e = call(t, d, "ResolveUncertain", p, ""); e != nil {
				t.Fatal(e)
			}
			stored, e := d.store.GetJob(ctx, p.ID)
			if e != nil {
				t.Fatal(e)
			}
			var final envelope
			json.Unmarshal(stored.Payload, &final)
			if final.Job.InFlight != nil || len(final.Job.ResolvedDispatches) != 1 {
				t.Fatal("lost uncertainty evidence")
			}
			if phase == "post-cas" {
				if status := liveGit(t, d.live.manager.Binding("effect-run", p.ID).Path, "status", "--porcelain=v1"); status != "" {
					t.Fatal("adopted commit left stale index", status)
				}
			}
			if phase != "prepared" && phase != "host-unrecorded" && len(final.Job.Provenance) != len(prior.Job.Provenance)+1 {
				t.Fatal("missing adopted commit")
			}
			proofs, e := d.live.guard.journal.Entries()
			if e != nil || len(proofs) != 0 {
				t.Fatal("unreleased effect lease", proofs, e)
			}
		})
	}
}
func TestEffectGuardTruthTable(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		if kernel.CanAdoptEffect(bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0) != (bits == 15) {
			t.Fatal(bits)
		}
	}
}

func TestEffectIntentPreservesLargeIntegerIdentity(t *testing.T) {
	original := effectIntent{Dispatch: engine.Dispatch{TaskID: "precise", ID: "precise/1", Inputs: []workflow.Evidence{{Fields: map[string]any{"large": json.Number("9007199254740993")}}}}}
	raw, e := json.Marshal(original)
	if e != nil {
		t.Fatal(e)
	}
	var restored effectIntent
	if e = decodeEffect(raw, &restored); e != nil {
		t.Fatal(e)
	}
	if effectKey(restored.Dispatch) != effectKey(original.Dispatch) {
		t.Fatal("effect identity changed across JSON persistence")
	}
	payload, _ := json.Marshal(envelope{Job: engine.Job{InFlight: &original.Dispatch}})
	var env envelope
	if e = decodeEnvelope(payload, &env); e != nil {
		t.Fatal(e)
	}
	if effectKey(*env.Job.InFlight) != effectKey(original.Dispatch) {
		t.Fatal("uncertain envelope lost integer precision")
	}
}

func TestAutoAdoptEffectGuardTruthTable(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		if CanAutoAdoptEffect(bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0) != (bits == 15) {
			t.Fatal(bits)
		}
	}
}
