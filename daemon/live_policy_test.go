package daemon

import (
	"context"
	"database/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/controlplane"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for name, changed := range map[string]string{"helper": strings.Replace(source, "return false", "return true", 1), "import": strings.Replace(source, "import \"testing\"", "import (\"testing\"; _ \"os\")", 1), "comment": source + "// Changed frozen file\n"} {
		t.Run(name, func(t *testing.T) {
			newIdentity, e := workflow.GoTestIdentity([]byte(changed), []string{"TestBehavior"})
			if e != nil || newIdentity != identity {
				t.Fatal("fixture must preserve named-test identity", e)
			}
			if e = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(changed), 0600); e != nil {
				t.Fatal(e)
			}
			if e = x.checkChange(ctx, d, binding); e == nil || !strings.Contains(e.Error(), "file bytes") {
				t.Fatal("frozen bytes mutation accepted", e)
			}
		})
	}
	if err = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	d.Inputs = nil
	if err = x.checkChange(ctx, d, binding); err == nil {
		t.Fatal("missing frozen provenance accepted")
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
