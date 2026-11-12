package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

func TestCanReleaseIntoMolecule(t *testing.T) {
	for _, c := range []struct {
		running                               bool
		total, limit, workflow, workflowLimit int
		want                                  bool
	}{
		{false, 5, -1, 5, -1, true},
		{true, 9, 1, 9, 1, true},
		{false, 1, 1, 0, -1, false},
		{false, 0, 1, 0, -1, true},
		{false, 1, 2, 1, 1, false},
		{false, 1, 2, 0, 1, true},
	} {
		if got := CanReleaseIntoMolecule(c.running, c.total, c.limit, c.workflow, c.workflowLimit); got != c.want {
			t.Fatalf("%+v: %v", c, got)
		}
	}
	if tighter(-1, 3) != 3 || tighter(2, -1) != 2 || tighter(2, 1) != 1 || tighter(-1, -1) != -1 {
		t.Fatal("tighter limit")
	}
}

// FAE-SCH-007: under the −1 defaults two revisions run concurrently. With an
// instance-wide limit of one, a second revision's molecule waits, withheld as
// molecule_limit, while the running molecule's tasks are never capped; it
// runs once the first molecule ends. FAE-WF-006: revisions with identical
// content bind distinct molecules, and one revision's tasks share one.
func TestRunningMoleculeLimits(t *testing.T) {
	// "content" declares max_running 1 in revision 2's own content instead.
	for _, limit := range []int{-1, 1, 0} {
		t.Run(map[int]string{-1: "unbounded", 1: "one", 0: "content"}[limit], func(t *testing.T) {
			cfg, ctx := fixture(t)
			cfg.MaxInFlight, cfg.MaxMolecules = 4, max(limit, -1)
			script := defaults.HappyScript()
			script["intake"] = []engine.ScriptStep{{Output: script["intake"][0].Output, Delay: 2 * time.Second}}
			cfg.Executor = engine.NewScripted(script)
			d, e := Start(ctx, cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { stop(t, d) }()
			authorize(t, d)
			doc := defaults.Workflow()
			if limit == 0 {
				doc.MoleculeConcurrency.MaxRunning = 1
			}
			if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
				t.Fatal(e)
			}
			molecules := map[string]string{}
			for _, task := range []struct {
				id       string
				revision int64
			}{{"a1", 1}, {"a2", 1}, {"b1", 2}} {
				r, e := call(t, d, "Admit", AdmitParams{ID: task.id, Revision: task.revision, Tree: "tree", AuthorizingMessage: "message"}, task.id)
				var admitted AdmissionResult
				decodeResult(t, r, e, &admitted)
				if admitted.MoleculeID == "" {
					t.Fatal("admission reported no molecule", string(r.Result))
				}
				molecules[task.id] = admitted.MoleculeID
			}
			if molecules["a1"] != molecules["a2"] || molecules["a1"] == molecules["b1"] {
				t.Fatal("molecule binding", molecules)
			}
			if limit == -1 {
				deadline := time.Now().Add(time.Minute)
				for {
					a1, _ := d.store.GetJob(ctx, "a1")
					b1, _ := d.store.GetJob(ctx, "b1")
					if a1.State != "Pending" && b1.State != "Pending" && a1.State != "Done" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("two revisions did not run concurrently", a1.State, b1.State)
					}
					time.Sleep(20 * time.Millisecond)
				}
			} else {
				deadline := time.Now().Add(time.Minute)
				for {
					w := d.withheld.Load()
					a1, _ := d.store.GetJob(ctx, "a1")
					a2, _ := d.store.GetJob(ctx, "a2")
					if w != nil && w.Tasks["b1"] == withheldMolecule && a1.State != "Pending" && a2.State != "Pending" {
						break
					}
					if a1.State == "Done" || time.Now().After(deadline) {
						t.Fatal("second molecule not withheld while the first ran", w, a1.State, a2.State)
					}
					time.Sleep(20 * time.Millisecond)
				}
				if b1, e := d.store.GetJob(ctx, "b1"); e != nil || b1.State != "Pending" {
					t.Fatal("limited molecule released", b1.State, e)
				}
			}
			if limit == 1 {
				r, e := call(t, d, "Status", nil, "")
				if e != nil || !strings.Contains(string(r.Result), `"molecules"`) {
					t.Fatal(string(r.Result), e)
				}
				var status struct {
					Snapshot struct {
						Scheduler struct {
							Molecules struct {
								Running int            `json:"running"`
								Limit   int            `json:"limit"`
								ByFlow  map[string]int `json:"running_by_workflow"`
							} `json:"molecules"`
						} `json:"scheduler"`
					} `json:"snapshot"`
				}
				if e = json.Unmarshal(r.Result, &status); e != nil || status.Snapshot.Scheduler.Molecules.Limit != 1 || status.Snapshot.Scheduler.Molecules.Running != 1 || status.Snapshot.Scheduler.Molecules.ByFlow["default"] != 1 {
					t.Fatal("molecule status", string(r.Result), e)
				}
			}
			for _, id := range []string{"a1", "a2", "b1"} {
				awaitState(t, d, id, "Done")
			}
			m, e := d.store.GetMolecule(ctx, molecules["a1"])
			if e != nil || m.EndedAt == nil {
				t.Fatal("molecule did not end with its tasks", m, e)
			}
		})
	}
}
