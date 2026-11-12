package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// FAE-VER-010 (pane loss), FAE-ROB-016, FAE-RT-011: with the multiplexer
// adapter, a turn whose Herdr pane vanishes mid-turn (its process group is
// killed after the provider was released) neither waits out its turn bound
// nor counts as done. The lost pane ends the turn at once: the task parks
// UncertainDispatch with the dispatch in flight and no artifact from it; the
// pane, its private Herdr server, its lease proof, pane reservation and
// leases are all retired; and the operator's re-dispatch runs the stage again
// to completion.
func TestDaemonMultiplexerPaneLossParksUncertain(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.Adapter, cfg.Live.HerdrBinary = "multiplexer", testHerdr(t)
	holds := filepath.Join(filepath.Dir(cfg.Workspace), "holds")
	if err := os.Mkdir(holds, 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{cfg.Live.Command[0], "-test.run=^TestLiveMatrixBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment["LIVE_MATRIX_HOLDS"] = holds
	// A turn bound the loss must not wait for.
	cfg.Live.TimeoutSeconds = 300
	if err := os.WriteFile(filepath.Join(holds, "lost.hold"), []byte("intake#1"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "lost", AuthorizingMessage: "message"}, "lost"); err != nil {
		t.Fatal(err)
	}
	var held liveMatrixHeld
	guard := newHangGuard(time.Minute)
	for {
		raw, e := os.ReadFile(filepath.Join(holds, "lost.held"))
		if e == nil && json.Unmarshal(raw, &held) == nil {
			break
		}
		if !guard.observe(anyDeferredPark(d)) {
			t.Fatal("the held turn never ran in its pane", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The bridge runs as a child of the pane's shell, the pane process group
	// leader; the whole group vanishing is the pane's loss.
	pane, err := syscall.Getpgid(held.Parent)
	if err != nil || pane != held.Parent {
		t.Fatal("bridge parent is not the pane process group leader", held, pane, err)
	}
	sessions := filepath.Join(d.layout.Root, "sessions") + "/"
	if err = syscall.Kill(-pane, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	lost := time.Now()
	_, env := awaitJob(t, d, "lost", time.Minute, func(_ controlplane.JobSnapshot, e envelope) bool {
		return e.Job.Parked && e.Job.Reason == "UncertainDispatch"
	})
	j := env.Job
	if elapsed := time.Since(lost); elapsed >= time.Duration(cfg.Live.TimeoutSeconds)*time.Second/2 {
		t.Fatal("the lost pane was noticed only near the turn bound", elapsed)
	}
	cause := ""
	for _, h := range j.History {
		if h.DispatchID == held.Dispatch && h.Kind == "execution-diagnostic" {
			cause = h.Cause
		}
	}
	if !strings.Contains(cause, "herdr pane lost") {
		t.Fatal("the turn did not end on its lost pane", cause)
	}
	if j.InFlight == nil || j.InFlight.ID != held.Dispatch || j.State != "intake" || j.Failures["crash-recovery"] != 0 {
		t.Fatal("lost pane did not park its own dispatch uncertain without a crash charge", j.State, j.InFlight, j.Failures)
	}
	for _, p := range j.Provenance {
		if p.DispatchID == held.Dispatch {
			t.Fatal("the lost turn was counted as done")
		}
	}
	// FAE-RT-011: the pane's session, its private server and its admission
	// are retired; nothing of the turn survives.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var left []string
		for pid, p := range userProcesses() {
			if strings.Contains(p.cmdline, sessions) || strings.HasPrefix(p.cwd, d.layout.Worktrees+"/") {
				left = append(left, fmt.Sprintf("%d %s", pid, p.cmdline))
			}
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lost pane's processes survived", left)
		}
	}
	if proofs, e := d.live.guard.journal.Entries(); e != nil || len(proofs) != 0 {
		t.Fatal("the lost turn's lease proof survived", proofs, e)
	}
	if panes, e := d.live.guard.panes.Snapshot(ctx); e != nil || len(panes) != 0 {
		t.Fatal("leaked pane reservations", panes, e)
	}
	if pool, e := d.live.guard.host.Snapshot(ctx); e != nil || len(pool.Holders) != 0 {
		t.Fatal("leaked host leases", pool, e)
	}
	if role, e := d.store.LeaseSnapshot(ctx, fmt.Sprintf("role/%x", sha256.Sum256([]byte("default\x00global\x00spec")))); e != nil || len(role.Holders) != 0 {
		t.Fatal("leaked role leases", role, e)
	}
	// FAE-ROB-016: only the operator's verb resolves it; the re-dispatched
	// stage then runs to completion in a fresh pane.
	if _, err = call(t, d, "ResolveUncertain", ResolveUncertainParams{ID: "lost", DispatchID: held.Dispatch, Action: "redispatch", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitJob(t, d, "lost", time.Minute, func(_ controlplane.JobSnapshot, e envelope) bool {
		return len(e.Job.Provenance) > 0 && e.Job.Provenance[0].Stage == "intake"
	})
}
