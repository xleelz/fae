package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/controlplane"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

func TestLiveFrozenFilePolicy(t *testing.T) {
	ctx := context.Background()
	root := reviewPhysical(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "init")
	const source = "package sample\nimport \"testing\"\nfunc check() bool { return false }\nfunc TestBehavior(t *testing.T) { if !check() { t.Fatal(\"bad\") } }\n"
	for name, data := range map[string]string{"answer.go": "package sample\nfunc answer() int { return 0 }\n", "answer_test.go": source} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	liveGit(t, repo, "add", ".")
	liveGit(t, repo, "commit", "-m", "base")
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	w, err := manager.Ensure(ctx, "run", "task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT payload_digest,payload_size FROM daemon_markers").WithArgs(activeConflictKey("")).WillReturnError(sql.ErrNoRows)
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	}()
	x := &liveExecutor{daemon: &Daemon{store: controlplane.New(db, controlplane.Options{})}, manager: manager, broker: &effects.Broker{Manager: manager}, config: LiveConfig{Files: []string{"answer.go", "answer_test.go"}, TestFile: "answer_test.go", TestID: "TestBehavior"}}
	tree, err := x.broker.Tree(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := workflow.GoTestIdentity([]byte(source), []string{"TestBehavior"})
	if err != nil {
		t.Fatal(err)
	}
	d := engine.Dispatch{Tree: tree, Config: workflow.Stage{Phase: "implement"}, Inputs: []workflow.Evidence{{Type: "FrozenIdentity", Tree: tree, Fields: map[string]any{"identity": identity}}}}
	binding := execution.Binding{Worktree: w.Path}
	if err = os.WriteFile(filepath.Join(w.Path, "answer.go"), []byte("package sample\nfunc answer() int { return 42 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = x.checkChange(ctx, d, binding); err != nil {
		t.Fatal("production-only change refused", err)
	}
	// FAE-WF-076: formatting, comments and added tests keep the frozen RED;
	// a changed helper or a side-effect import does not.
	for name, changed := range map[string]string{"comment": source + "// Changed frozen file\n", "formatter": strings.ReplaceAll(source, "{ if !check() { t.Fatal(\"bad\") } }", "{\n\tif !check() {\n\t\tt.Fatal(\"bad\")\n\t}\n}"), "added test": source + "func TestEdge(t *testing.T) {}\n"} {
		t.Run(name, func(t *testing.T) {
			if e := os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(changed), 0600); e != nil {
				t.Fatal(e)
			}
			mock.ExpectQuery("SELECT payload_digest,payload_size FROM daemon_markers").WithArgs(activeConflictKey("")).WillReturnError(sql.ErrNoRows)
			if e := x.checkChange(ctx, d, binding); e != nil {
				t.Fatal("formatter-invariant RED change refused", e)
			}
		})
	}
	for name, changed := range map[string]string{"helper": strings.Replace(source, "return false", "return true", 1), "import": strings.Replace(source, "import \"testing\"", "import (\"testing\"; _ \"os\")", 1), "init": source + "func init() {}\n"} {
		t.Run(name, func(t *testing.T) {
			newIdentity, e := workflow.GoTestIdentity([]byte(changed), []string{"TestBehavior"})
			if e != nil || newIdentity != identity {
				t.Fatal("fixture must preserve named-test identity", e)
			}
			if e = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(changed), 0600); e != nil {
				t.Fatal(e)
			}
			if e = x.checkChange(ctx, d, binding); e == nil || !strings.Contains(e.Error(), "frozen RED") {
				t.Fatal("frozen RED mutation accepted", e)
			}
		})
	}
	if err = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	frozenInputs := d.Inputs
	d.Inputs = nil
	if err = x.checkChange(ctx, d, binding); err == nil {
		t.Fatal("missing frozen provenance accepted")
	}
	// FAE-WF-073: a phase-less change producer of a code-bearing workflow is
	// held to the frozen RED exactly like the implement phase.
	d.Inputs = frozenInputs
	d.Config = workflow.Stage{ChangeProducer: true}
	binding.Document = workflow.Document{CodeBearing: true}
	if err = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(strings.Replace(source, "return false", "return true", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err = x.checkChange(ctx, d, binding); err == nil || !strings.Contains(err.Error(), "frozen RED") {
		t.Fatal("phase-less change producer edited the frozen RED", err)
	}
}

func TestLiveAdmissionRefusesResidue(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, committed := range []bool{false, true} {
		id := "dirty"
		if committed {
			id = "committed"
		}
		w, e := d.live.manager.Ensure(ctx, "residue-run", id, cfg.Live.Base)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(w.Path, "answer.go"), []byte("package sample\nfunc answer() int { return 999 }\n"), 0600); e != nil {
			t.Fatal(e)
		}
		if committed {
			liveGit(t, w.Path, "add", "answer.go")
			liveGit(t, w.Path, "commit", "-m", "leftover previous attempt")
		}
		_, e = call(t, d, "Admit", AdmitParams{ID: id, RunID: "residue-run", AuthorizingMessage: "message"}, id)
		if e == nil || !strings.Contains(e.Error(), "stale task branch residue") {
			t.Fatal("stale admission accepted", id, e)
		}
		if _, e = d.store.GetJob(ctx, id); e == nil {
			t.Fatal("refused task became durable")
		}
	}
}

// FAE-ROB-013 acceptance: a task that declared no-red but has valid frozen RED
// evidence re-validates on the RED: its implementation turn is held to the
// frozen RED and the commit floor is satisfied by it. The no-red gate (the
// target's diff classifier) is consulted only when RED evidence is absent.
func TestNoRedWithValidRedRevalidatesOnRed(t *testing.T) {
	doc := defaults.Workflow()
	// The shipped red-route and green-route send a declared no-red task past
	// RED (FAE-WF-073), so this scenario needs content without them, where a
	// declared no-red task still freezes a RED: the routes and the no-red
	// suite stages are removed and their edges lead straight on.
	gone := []string{"red-route", "green-route", "run-suite", "suite-gate"}
	via := map[string]string{"red-route": "red-loop", "green-route": "run-tests"}
	var edges []workflow.Edge
	for _, e := range doc.Edges {
		if slices.Contains(gone, e.From) {
			continue
		}
		if to, ok := via[e.To]; ok {
			e.To = to
		}
		edges = append(edges, e)
	}
	doc.Edges = edges
	for id, st := range doc.Stages {
		if slices.Contains(gone, id) {
			delete(doc.Stages, id)
			continue
		}
		st.Inputs = slices.DeleteFunc(slices.Clone(st.Inputs), func(in workflow.Input) bool { return in.Stage == "run-suite" })
		doc.Stages[id] = st
	}
	doc.NoRedStages = []string{"spec"}
	doc.TaskClasses = map[string]workflow.TaskClass{"code": {CodeBearing: true}}
	doc.DefaultClass = "code"
	var schema map[string]any
	if err := json.Unmarshal(doc.Artifacts["Spec"].Schema, &schema); err != nil {
		t.Fatal(err)
	}
	schema["properties"].(map[string]any)["no_red"] = map[string]any{"type": "boolean"}
	raw, _ := json.Marshal(schema)
	doc.Artifacts["Spec"] = workflow.ArtifactType{Version: doc.Artifacts["Spec"].Version, Schema: raw}
	if issues := workflow.Validate(doc); len(issues) > 0 {
		t.Fatal(issues)
	}
	script := defaults.HappyScript()
	var spec map[string]any
	_ = json.Unmarshal(script["spec"][0].Output, &spec)
	spec["no_red"] = true
	script["spec"][0].Output, _ = json.Marshal(spec)
	executor := engine.NewScripted(script)
	advance := func(id, stage string) (engine.Job, *engine.Dispatch) {
		j, err := engine.New(id, 1, doc, "tree")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 100; i++ {
			next, d, _, err := engine.Prepare(j)
			if err != nil {
				t.Fatal(err)
			}
			j = next
			if d == nil {
				continue
			}
			if d.Stage == stage {
				return j, d
			}
			r, err := executor.Execute(context.Background(), *d)
			if err != nil {
				t.Fatal(err)
			}
			if j, _, err = engine.Complete(j, d.ID, r); err != nil {
				t.Fatal(err)
			}
		}
		t.Fatal("never dispatched", stage)
		return j, nil
	}
	withRed, implement := advance("with-red", "implement")
	withoutRed, plan := advance("without-red", "plan")
	if !engine.NoRedDeclared(withRed) || !engine.HasFrozenRed(withRed) || engine.HasFrozenRed(withoutRed) {
		t.Fatal("fixture", engine.NoRedDeclared(withRed), engine.HasFrozenRed(withRed))
	}
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	for _, j := range []engine.Job{withRed, withoutRed} {
		payload, _ := json.Marshal(envelope{Job: j})
		d.jobs[j.TaskID] = controlplane.JobSnapshot{ID: j.TaskID, State: string(j.State), Payload: payload}
	}
	x := &liveExecutor{daemon: d}
	if x.noRedExempt(*implement) {
		t.Fatal("valid frozen RED discarded for the no-red fallback")
	}
	// The same implementation turn without RED evidence takes the no-red path.
	bare := *implement
	bare.TaskID, bare.Inputs = withoutRed.TaskID, nil
	for _, in := range implement.Inputs {
		if in.Type != "FrozenIdentity" {
			bare.Inputs = append(bare.Inputs, in)
		}
	}
	if !engine.NoRedDeclared(withoutRed) || !x.noRedExempt(bare) || plan == nil {
		t.Fatal("no-red task without RED evidence was not exempted")
	}
	if !CanCommitWithoutRed(true, true, false, true) || CanCommitWithoutRed(false, true, false, true) || CanCommitWithoutRed(false, true, true, true) {
		t.Fatal("commit floor must prefer RED and consult no-red only without it")
	}
}
