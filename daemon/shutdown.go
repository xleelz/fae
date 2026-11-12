package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/provider"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// DefaultDrainBound is the Appendix A drain-down bound for in-flight commit and
// integrate steps after a graceful shutdown begins (FAE-ROB-010).
const DefaultDrainBound = 120 * time.Second

// stopMarker is the durable graceful-stop record. It lists the turns left in
// flight so the next start can tell a planned interruption from a crash.
type stopMarker struct {
	Graceful  bool              `json:"graceful"`
	Overrun   bool              `json:"overrun,omitempty"`
	StoppedAt time.Time         `json:"stopped_at,omitempty"`
	InFlight  []stoppedInFlight `json:"in_flight"`
	legacy    bool
	// isolated recovers a dispatch whose outcome quarantine dropped at run
	// time: never charged; scripted replays it, live parks it.
	isolated bool
}
type stoppedInFlight struct {
	Task     string `json:"task"`
	Dispatch string `json:"dispatch"`
	Stage    string `json:"stage"`
}

// parseStopMarker accepts the legacy boolean marker; it never lists turns.
func parseStopMarker(raw []byte) stopMarker {
	var legacy bool
	if json.Unmarshal(raw, &legacy) == nil {
		return stopMarker{Graceful: legacy, legacy: legacy}
	}
	var m stopMarker
	if json.Unmarshal(raw, &m) != nil {
		return stopMarker{}
	}
	return m
}

// planned reports whether this dispatch was interrupted by a recorded graceful stop.
func (m stopMarker) planned(task, dispatch string) bool {
	for _, f := range m.InFlight {
		if m.Graceful && f.Task == task && f.Dispatch == dispatch {
			return true
		}
	}
	return false
}

// recoverInFlight decides a stored in-flight dispatch at startup. Scripted
// rehearsal keeps exact-token replay. A live local effect with a completed,
// exactly remeasured receipt is adopted without charge. A live turn listed by
// the graceful-stop marker is replayed without charge only after the workspace
// and session retirement are proved; everything else parks for resolution.
func (d *Daemon) recoverInFlight(ctx context.Context, e envelope, marker stopMarker) (engine.Job, error) {
	j := e.Job
	if j.Fan != nil && j.Fan.Baseline != nil {
		return engine.RecoverBranchFrames(j, func(view engine.Job) (engine.Job, error) {
			child := e
			child.Job = view
			return d.recoverInFlight(ctx, child, marker)
		})
	}
	if d.live != nil {
		if adopted, ok, err := d.adoptCompletedEffect(ctx, e); ok {
			return adopted, nil
		} else if err != nil {
			d.taskLog("task %s completed effect receipt of %s not adopted: %v\n", j.TaskID, j.InFlight.ID, err)
		}
	}
	_, safe := d.executor.(*engine.Scripted)
	if marker.isolated {
		recovered, _, err := engine.Recover(j, true, safe && d.live == nil)
		return recovered, err
	}
	planned := marker.planned(j.TaskID, j.InFlight.ID)
	if d.live == nil {
		recovered, _, err := engine.Recover(j, marker.Graceful && (marker.legacy || planned), safe)
		return recovered, err
	}
	if cp, hasCP := d.findInFlightCheckpoint(ctx, j); hasCP {
		if d.live.guard != nil {
			_ = d.live.guard.recoverProofs(ctx)
		}
		_ = d.retireReceiptLeases(ctx, *j.InFlight)
		if d.live.manager != nil {
			w := d.live.manager.Binding(e.Admission.RunID, j.TaskID)
			if w.Path != "" {
				if _, statErr := os.Stat(w.Path); statErr == nil {
					scope, scopeErr := d.live.resolveLiveTaskScope(j.TaskID, j.InFlight.Scope, j.InFlight.Config, func() (envelope, error) { return e, nil })
					var editableScope, readOnlyScope []string
					if scopeErr == nil {
						editableScope = slices.Clone(scope.Files)
						readOnlyScope = slices.Clone(scope.ReadOnlyFiles)
						if j.InFlight.Stage == "implement" && scope.TestFile != "" {
							editableScope = slices.DeleteFunc(editableScope, func(p string) bool { return p == scope.TestFile })
						}
					}
					changeProd := j.InFlight.Config.ChangeProducer && j.InFlight.Config.Primitive != workflow.Review
					_, _ = provider.VerifyAndRevertWorktreeScope(ctx, w.Path, j.Tree, editableScope, readOnlyScope, changeProd)
					if inScopeTree, snapErr := provider.SnapshotInScopeTree(ctx, w.Path, j.Tree, editableScope); snapErr == nil && inScopeTree != "" && (inScopeTree != j.Tree || cp.WorktreeTree == "") {
						cp.WorktreeTree = inScopeTree
					}
					_ = provider.RestoreWorktreeCheckpoint(ctx, w.Path, j.Tree)
				}
			}
		}
		j.SaveDispatchCheckpoint(*j.InFlight, cp)
		if d.store != nil {
			saved, _ := j.CheckpointForStage(j.InFlight.Stage)
			_ = d.store.SaveTurnCheckpoint(ctx, controlplane.TurnCheckpointRecord{
				TaskID:          saved.TaskID,
				StageID:         engine.CheckpointStorageStage(*j.InFlight),
				Visit:           int64(saved.Visit),
				Attempt:         int64(saved.Attempt),
				ProviderSession: saved.ProviderSession,
				WorktreeTree:    saved.WorktreeTree,
				AssistantTurns:  int64(saved.AssistantTurns),
				UpdatedAt:       saved.UpdatedAt,
			})
		}
		recovered, _, err := engine.Recover(j, marker.Graceful, false)
		return recovered, err
	}
	if planned {
		err := d.replaySafe(ctx, e)
		if err == nil {
			var replayed engine.Job
			if replayed, _, err = engine.ReplayInterrupted(j, j.InFlight.ID); err == nil {
				return replayed, nil
			}
		}
		if replayed, ok, _ := d.replayUnlaunched(ctx, e); ok {
			return replayed, nil
		}
		d.taskLog("task %s planned replay of %s refused: %v\n", j.TaskID, j.InFlight.ID, err)
	} else if replayed, ok, err := d.replayUnlaunched(ctx, e); ok {
		return replayed, nil
	} else if err != nil {
		d.taskLog("task %s unlaunched replay of %s refused: %v\n", j.TaskID, j.InFlight.ID, err)
	} else if rerun, ok, err := d.rerunInterrupted(ctx, e); ok {
		return rerun, nil
	} else if err != nil {
		d.taskLog("task %s interrupted engine run %s not re-run: %v\n", j.TaskID, j.InFlight.ID, err)
	}
	recovered, _, err := engine.Recover(j, marker.Graceful, false)
	return recovered, err
}

// rerunInterrupted re-runs, at recovery, a crash-interrupted engine run whose
// re-execution is idempotent (FAE-ROB-013): a host command or identity
// measurement with no completed receipt, on its unchanged workspace, whose
// session workloads are proven retired (their owners provably dead). An
// interrupted QA run is infrastructure: it re-runs without a failure verdict
// or operator verb. Anything unproven keeps the conservative park.
func (d *Daemon) rerunInterrupted(ctx context.Context, e envelope) (engine.Job, bool, error) {
	j := e.Job
	dispatch := *j.InFlight
	if j.OperatorStage != "" || e.Admission.TargetConfig != "" || !d.rerunnableEffect(ctx, dispatch) {
		return j, false, nil
	}
	if err := d.retireReceiptLeases(ctx, dispatch); err != nil {
		return j, false, err
	}
	if err := d.replaySafe(ctx, e); err != nil {
		return j, false, err
	}
	rerun, _, err := engine.RerunInterrupted(j, dispatch.ID)
	return rerun, err == nil, err
}

// replayUnlaunched reissues, at recovery, an interrupted live turn whose
// admission journal and effect markers prove no session or effect ever reached
// its launch boundary (FAE-RT-004): nothing ran, so no attempt or recovery
// budget is charged.
func (d *Daemon) replayUnlaunched(ctx context.Context, e envelope) (engine.Job, bool, error) {
	j := e.Job
	dispatch := *j.InFlight
	if j.OperatorStage != "" || e.Admission.TargetConfig != "" || d.live == nil || d.live.guard == nil || d.live.guard.journal == nil {
		return j, false, nil
	}
	refused := d.hasRefusedBeforeExec(ctx, dispatch)
	for _, session := range dispatch.Sessions {
		if d.live.recoveredLaunches[session.ID] && !refused {
			return j, false, nil
		}
		if d.sessionLaunchedOnDisk(session.ID) {
			return j, false, nil
		}
	}
	proofs, err := d.live.guard.journal.Entries()
	if err != nil {
		return j, false, err
	}
	hasProof := false
	for _, proof := range proofs {
		for _, session := range dispatch.Sessions {
			if proof.Session != session.ID {
				continue
			}
			if proof.State != "reserved" && !refused {
				return j, false, nil
			}
			if !processowner.ProvedDead(proof.Owner.PID, proof.Birth) {
				return j, false, nil
			}
			hasProof = true
		}
	}
	if _, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/result"); err == nil {
		return j, false, nil
	}
	if !refused {
		if _, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/started"); err == nil {
			return j, false, nil
		}
		if _, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/intent"); err == nil {
			return j, false, nil
		}
	}
	w := d.live.manager.Binding(e.Admission.RunID, j.TaskID)
	if _, statErr := os.Stat(w.Path); os.IsNotExist(statErr) {
		if len(j.Provenance) != 0 || j.ExecutionHead != "" {
			return j, false, nil
		}
	} else if _, err := d.live.checkControlWorkspace(ctx, e); err != nil {
		return j, false, nil
	}
	if hasProof {
		if refused {
			if err := d.retireReceiptLeases(ctx, dispatch); err != nil {
				return j, false, err
			}
		} else if err := d.live.guard.recoverProofs(ctx); err != nil {
			return j, false, err
		}
		proofs, err = d.live.guard.journal.Entries()
		if err != nil {
			return j, false, err
		}
		for _, proof := range proofs {
			for _, session := range dispatch.Sessions {
				if proof.Session == session.ID {
					return j, false, nil
				}
			}
		}
	}
	replayed, _, err := engine.ReplayUnlaunched(j, dispatch.ID)
	return replayed, err == nil, err
}

func (d *Daemon) findInFlightCheckpoint(ctx context.Context, j engine.Job) (engine.TurnCheckpoint, bool) {
	if j.InFlight == nil {
		return engine.TurnCheckpoint{}, false
	}
	stage := j.InFlight.Stage
	cp, ok := j.CheckpointForStage(stage)
	if !ok && j.InFlight.ResumeCheckpoint != nil && j.InFlight.ResumeCheckpoint.ProviderSession != "" {
		cp = *j.InFlight.ResumeCheckpoint
		ok = true
	}
	if d.store != nil {
		if storedCP, found, err := d.store.LatestTurnCheckpoint(ctx, j.TaskID, engine.CheckpointStorageStage(*j.InFlight)); err == nil && found && storedCP.ProviderSession != "" {
			cp = engine.TurnCheckpoint{
				TaskID:          storedCP.TaskID,
				StageID:         stage,
				Visit:           int(storedCP.Visit),
				Attempt:         int(storedCP.Attempt),
				ProviderSession: storedCP.ProviderSession,
				WorktreeTree:    storedCP.WorktreeTree,
				AssistantTurns:  int(storedCP.AssistantTurns),
				UpdatedAt:       storedCP.UpdatedAt,
			}
			ok = true
		}
	}
	if diskCP := d.readDiskSessionCheckpoint(*j.InFlight); diskCP != nil && diskCP.ProviderSession != "" {
		cp.TaskID = j.TaskID
		cp.StageID = stage
		cp.Visit = j.InFlight.Visit
		cp.Attempt = j.InFlight.Attempt
		cp.ProviderSession = diskCP.ProviderSession
		if diskCP.WorktreeTree != "" {
			cp.WorktreeTree = diskCP.WorktreeTree
		}
		if diskCP.AssistantTurns > 0 {
			cp.AssistantTurns = diskCP.AssistantTurns
		}
		cp.UpdatedAt = time.Now().UTC()
		ok = true
	}
	return cp, ok && cp.ProviderSession != ""
}

func (d *Daemon) readDiskSessionCheckpoint(dispatch engine.Dispatch) *agentruntime.TurnCheckpoint {
	if d.layout.Root == "" {
		return nil
	}
	sessionsDir := filepath.Join(d.layout.Root, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}
	var cfg struct {
		SessionID  string `json:"session_id"`
		DispatchID string `json:"dispatch_id"`
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") {
			continue
		}
		dir := filepath.Join(sessionsDir, entry.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			continue
		}
		cfg.SessionID, cfg.DispatchID = "", ""
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		matched := cfg.DispatchID == dispatch.ID
		if !matched {
			for _, s := range dispatch.Sessions {
				if cfg.SessionID == s.ID {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		if cp, err := agentruntime.ReadTurnCheckpoint(dir); err == nil && cp != nil && cp.ProviderSession != "" {
			return cp
		}
	}
	return nil
}

func (d *Daemon) sessionLaunchedOnDisk(sessionID string) bool {
	if sessionID == "" || d.layout.Root == "" {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(d.layout.Root, "sessions"))
	if err != nil {
		return false
	}
	var cfg struct {
		SessionID string `json:"session_id"`
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.layout.Root, "sessions", entry.Name(), "config.json"))
		if err != nil {
			continue
		}
		cfg.SessionID = ""
		if json.Unmarshal(raw, &cfg) == nil && cfg.SessionID == sessionID {
			return true
		}
	}
	return false
}

// replaySafe proves an interrupted live turn left no trace: the branch HEAD and
// tree equal the durable checkpoint, no session lease proof survives, and no
// effect receipt completed (a completed receipt is adopted, never re-run).
//
// A lease proof is removed only after the session's workload retirement was
// proven and its leases released. Close joins every cancelled turn within the
// drain bound before it writes the marker, so a planned stop leaves no proof
// unless retirement was genuinely unproven. A proof that survives into a new
// process is reclaimed at startup only when its owner is provably dead and its
// linked receipts are complete; an in-process restart shares the owner's pid
// and birth, so its surviving proof is kept and the turn parks.
func (d *Daemon) replaySafe(ctx context.Context, e envelope) error {
	dispatch := *e.Job.InFlight
	switch dispatch.Config.Primitive {
	case workflow.AgentTurn, workflow.Review, workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen:
	default:
		return errors.New("primitive is not replayable")
	}
	if _, err := d.live.checkControlWorkspace(ctx, e); err != nil {
		return err
	}
	if d.live.guard != nil && d.live.guard.journal != nil {
		proofs, err := d.live.guard.journal.Entries()
		if err != nil {
			return err
		}
		for _, proof := range proofs {
			for _, session := range dispatch.Sessions {
				if proof.Session == session.ID {
					return errors.New("session lease retirement unproven")
				}
			}
		}
	}
	if supportedEffect(dispatch.Config.Primitive) {
		if _, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/result"); err == nil {
			return errors.New("completed effect receipt requires adoption")
		}
	}
	return nil
}

// drainCompletions collects finished turns delivered during shutdown, under
// d.mu after the drive stopped. Interrupted or failed turns stay in flight and
// are listed by the stop marker for resume.
func (d *Daemon) drainCompletions(ctx context.Context) {
	d.drainMu.Lock()
	pending := d.drained
	d.drained = nil
	d.drainMu.Unlock()
	if d.pendingCompletion != nil {
		pending = append([]completion{*d.pendingCompletion}, pending...)
		d.pendingCompletion = nil
	}
	for {
		select {
		case c := <-d.results:
			pending = append(pending, c)
			continue
		default:
		}
		break
	}
	for _, c := range pending {
		if c.err != nil || c.result.Class == "interrupted" || c.result.Class == "uncertain" {
			continue
		}
		d.pendingCompletion = &c
		if err := d.collectPending(ctx); err != nil {
			d.taskLog("shutdown collection of %s failed: %v\n", c.dispatch, err)
		}
		d.pendingCompletion = nil
	}
}

// writeStopMarker records the graceful stop and the in-flight set it leaves.
func (d *Daemon) writeStopMarker(ctx context.Context, overrun bool) error {
	m := stopMarker{Graceful: true, Overrun: overrun, StoppedAt: time.Now().UTC(), InFlight: []stoppedInFlight{}}
	for id, snapshot := range d.jobs {
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) != nil || e.Job.InFlight == nil {
			continue
		}
		m.InFlight = append(m.InFlight, stoppedInFlight{Task: id, Dispatch: e.Job.InFlight.ID, Stage: e.Job.InFlight.Stage})
	}
	sort.Slice(m.InFlight, func(i, j int) bool { return m.InFlight[i].Task < m.InFlight[j].Task })
	raw, _ := json.Marshal(m)
	return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "stopped", Outcome: "ok"}, "clean-stop", raw)
}

func waitFor(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// lockWithin takes d.mu unless a wedged drive holds it past the deadline.
func (d *Daemon) lockWithin(ctx context.Context) bool {
	for !d.mu.TryLock() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

var errDrainOverrun = errors.New("shutdown drain-down bound exceeded")
