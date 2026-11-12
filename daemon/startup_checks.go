package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

type preMigrationSchemaKey struct{}

func withPreMigrationSchema(ctx context.Context) context.Context {
	return context.WithValue(ctx, preMigrationSchemaKey{}, true)
}

func preMigrationSchema(ctx context.Context) bool {
	v, _ := ctx.Value(preMigrationSchemaKey{}).(bool)
	return v
}

// StartupChecks is what this start's plane and workflow checks found (status
// field startup_checks).
type StartupChecks struct {
	// Plane is the control plane's own consistency (FAE-CP-006).
	Plane StartupPlaneCheck `json:"plane"`
	// Revisions is the stored workflow revisions check (FAE-WF-093).
	Revisions StartupRevisionCheck `json:"revisions"`
	// ScratchQuarantine is what scratch-quarantine held after the start's
	// scratch sweeps (FAE-HOST-020).
	ScratchQuarantine *ScratchQuarantineReport `json:"scratch_quarantine,omitempty"`
}

// StartupPlaneCheck is controlplane.Store.CheckConsistency at start, and the
// tasks whose rows diverged, which the start quarantined.
type StartupPlaneCheck struct {
	controlplane.Consistency
	Consistent  bool     `json:"consistent"`
	Quarantined []string `json:"quarantined,omitempty"`
}

// StartupRevisionCheck reports the stored revisions the start checked: every
// one, in two classes (FAE-WF-093, operator decision of 2026-10-30). One this
// binary cannot read (workflow.Load) refuses the start. One it reads but
// cannot run (workflow.Parse) refuses the start while an active task is
// pinned to it, unless `serve --quarantine-unrunnable-revisions` quarantines
// those tasks; one no active task is pinned to is listed.
type StartupRevisionCheck struct {
	Checked int `json:"checked"`
	// Unloadable is always empty on a running daemon (an unreadable revision
	// refuses the start); it is kept for readers written against wave 11.
	Unloadable []UnloadableRevisionRef `json:"unloadable"`
	// Invalid are the stored revisions this binary reads (workflow.Load) but
	// cannot run: they fail a rule admission applies (workflow.Parse), such
	// as the old default's `main` brief keys, a Gate without an expression
	// or a start stage the document lacks. On a running daemon no task in
	// the drive is pinned to one: an active task pinned to it refuses the
	// start, or is quarantined under `serve --quarantine-unrunnable-revisions`
	// (Quarantined). Admission and migration to one are refused, and the
	// default upgrade replaces a builtin default among them (installDefault).
	Invalid []UnloadableRevisionRef `json:"invalid,omitempty"`
	// Quarantined are the active tasks this start quarantined because their
	// pinned revision is unrunnable (`serve --quarantine-unrunnable-revisions`);
	// `fae migrate` moves one to a revision this binary validates, and
	// `fae quarantine retire` retires it keeping its history.
	Quarantined []string `json:"quarantined,omitempty"`
}

// UnloadableRevisionRef is one stored workflow revision this binary cannot
// read, or reads but cannot run, what uses it, and why.
type UnloadableRevisionRef struct {
	WorkflowID string `json:"workflow_id"`
	Revision   int64  `json:"revision"`
	Hash       string `json:"hash"`
	Error      string `json:"error"`
	// Tasks are the active tasks pinned to it (a row an operator retired
	// from quarantine is not counted). On a running daemon they are
	// quarantined, never driven.
	Tasks []string `json:"tasks,omitempty"`
	// Admits reports the latest revision of an active workflow, which a new
	// admission without a named revision would bind; such an admission is
	// refused. The default upgrade clears it for a builtin default it
	// replaced.
	Admits bool `json:"admits,omitempty"`
}

// startupReadBound bounds each plane read of the start's checks. Neither
// check has a bound on its whole: each reads page after page, so every task
// and every stored revision is checked, while a hung plane still fails the
// read it hangs and so the start.
const startupReadBound = 10 * time.Second

// startupUnloadableNamed is how many unloadable revisions the start's refusal
// names in full; the daemon log names every one.
const startupUnloadableNamed = 10

// unrunnableRevisionFlag is the serve flag under which a start quarantines
// the active tasks pinned to an unrunnable revision instead of refusing.
const unrunnableRevisionFlag = "--quarantine-unrunnable-revisions"

// RelaunchArgs returns args without the one-shot serve flags that act on one
// start only (--quarantine-unrunnable-revisions), so a roll, maintenance
// relaunch or run --start does not replay them (FAE-WF-093).
func RelaunchArgs(args []string) []string {
	out := []string{}
	for _, arg := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if strings.HasPrefix(arg, "-") && name == "quarantine-unrunnable-revisions" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

type workflowRevisionKey struct {
	workflowID string
	revision   int64
}

// updateStartupReport changes the status field startup_checks; the start's
// steps fill it in order, before the socket listens.
func (d *Daemon) updateStartupReport(change func(*StartupChecks)) {
	report, _ := d.startupReport.Load().(StartupChecks)
	change(&report)
	d.startupReport.Store(report)
}

// startupRevisions is the start's stored-revision check (FAE-WF-093), run
// before installDefault writes anything, so a refused start writes no
// revision. It records the check for status; the tasks it marks for
// quarantine are quarantined by startupChecks.
func (d *Daemon) startupRevisions(ctx context.Context, snapshots []controlplane.JobSnapshot) error {
	retired, err := d.quarantineDispositions(ctx)
	if err != nil {
		if preMigrationSchema(ctx) && strings.Contains(err.Error(), "daemon_markers") {
			retired, err = nil, nil
		} else {
			return err
		}
	}
	revisions, err := d.startupRevisionCheck(ctx, snapshots, retired)
	d.updateStartupReport(func(r *StartupChecks) { r.Revisions = revisions })
	return err
}

// startupChecks applies the start's plane check and the revision check's
// quarantines before the stored jobs load, records them for status and
// returns the active rows to load.
//
// The plane's consistency (FAE-CP-006) was checked when the plane came up
// (startPlane, before this start writes any event): it is the check a
// restarted plane gets (controlplane.Store.CheckConsistency, by key and
// index, every active task a page at a time). A task whose task or backlog
// row no longer records its snapshot's state is quarantined here (not
// loaded), as after a plane restart, and kept out of the returned rows;
// `fae quarantine release` repairs those rows from the snapshot.
//
// Every stored workflow revision was checked first (startupRevisions, before
// installDefault; FAE-WF-093). Every revision ever written must stay loadable
// by later binaries, or the binary must refuse to start naming it. One this
// binary cannot read (workflow.Load: not canonical JSON, unknown keys or
// wrong types, an unknown schema version), or cannot read within
// startupReadBound, refuses the start. One it reads but cannot run (the rule
// admission uses, workflow.Parse) refuses the start while an active task is
// pinned to it, naming the workflow, revision, hash, failing rules, tasks and
// the recovery: a start under `serve --quarantine-unrunnable-revisions`
// instead quarantines exactly those tasks here, with the audited alert and
// event each quarantine writes, so the operator can migrate or retire them
// (unrunnableRevisionTasks; quarantineRelease refuses while the revision
// stays unrunnable). An unrunnable revision no active task is pinned to is
// listed and logged, and the start goes on. Nothing is quarantined or loaded
// before the check.
//
// The start then sweeps the scratch killed gate runs left and reports what
// scratch-quarantine holds (sweepGateRunScratch, reportScratchQuarantine).
func (d *Daemon) startupChecks(ctx context.Context, snapshots []controlplane.JobSnapshot, marker stopMarker) ([]controlplane.JobSnapshot, error) {
	report, _ := d.startupReport.Load().(StartupChecks)
	retired, err := d.quarantineDispositions(ctx)
	if err != nil {
		return nil, err
	}
	var kept []controlplane.JobSnapshot
	for _, s := range snapshots {
		// A row an operator retired from quarantine stays retired
		// (loadStoredJobs) while unchanged.
		r, held := retired[s.ID]
		if slices.Contains(report.Plane.Diverged, s.ID) && !(held && r.Version == s.Version && r.Digest == rowDigest(s)) {
			d.quarantine(ctx, s.ID, taskErr("startup-consistency", errors.New("task rows diverged from the durable snapshot")))
			d.quarantineBook.atStart(s.ID, marker)
			report.Plane.Quarantined = append(report.Plane.Quarantined, s.ID)
			continue
		}
		if ref, unrunnable := d.unrunnableRevisionTasks[s.ID]; unrunnable {
			qErr := taskErr("unrunnable-revision", errors.New(unrunnableRevisionQuarantine(ref)))
			if d.restoredUnrunnableQuarantine[s.ID] {
				if d.quarantined == nil {
					d.quarantined = map[string]string{}
				}
				reason := qErr.Error()
				if len(reason) > 1024 {
					reason = reason[:1024]
				}
				d.quarantined[s.ID] = reason
			}
			d.quarantine(ctx, s.ID, qErr)
			d.quarantineBook.atStart(s.ID, marker)
			report.Revisions.Quarantined = append(report.Revisions.Quarantined, s.ID)
			continue
		}
		kept = append(kept, s)
	}
	d.startupReport.Store(report)
	d.sweepGateRunScratch()
	d.reportScratchQuarantine(ctx)
	return kept, nil
}

// startupPlaneConsistency checks the plane's invariants before this start
// writes any event or loads any job: an event clock behind the plane's own
// events refuses the start, naming the damage, since every later event write
// depends on that clock, and so does a check that fails or whose read takes
// longer than startupReadBound.
func (d *Daemon) startupPlaneConsistency(ctx context.Context) (StartupPlaneCheck, error) {
	c, err := d.store.CheckConsistencyBounded(ctx, startupReadBound)
	if err != nil {
		return StartupPlaneCheck{}, fmt.Errorf("startup consistency check of the control plane: %w", err)
	}
	check := StartupPlaneCheck{Consistency: c, Consistent: c.Consistent()}
	d.logLine("plane", "startup consistency: event_clock=%d last_event=%d diverged=%d consistent=%t\n", c.EventClock, c.LastEvent, len(c.Diverged), check.Consistent)
	if c.EventClock < c.LastEvent {
		return check, fmt.Errorf("startup consistency check: the control plane's event clock (%d) is behind its last event (%d), so its events cannot be ordered; the plane is damaged: restore it from a backup", c.EventClock, c.LastEvent)
	}
	return check, nil
}

// startupRevisionCheck reads every stored workflow revision, a page at a
// time with each read bounded, and refuses the start, naming them, when this
// binary cannot read one or more of them, or cannot run one an active task
// is pinned to (unless the start quarantines those tasks, which it marks in
// unrunnableRevisionTasks). It lists the unrunnable ones, with a daemon line
// each. A row retired from quarantine (retired, matching version and digest)
// is not an active task.
func (d *Daemon) startupRevisionCheck(ctx context.Context, snapshots []controlplane.JobSnapshot, retired map[string]RetiredQuarantine) (StartupRevisionCheck, error) {
	check := StartupRevisionCheck{Unloadable: []UnloadableRevisionRef{}}
	var bad, invalid []UnloadableRevisionRef
	latest := map[string]int64{}
	// Revisions are content-addressed: content read and checked once stands
	// for every revision that stores it.
	type verdict struct {
		load, valid error
		refs        []workflow.WorkflowRef
	}
	parsed := map[string]verdict{}
	knownRevs := map[workflowRevisionKey]bool{}
	subRefs := map[workflowRevisionKey][]workflow.WorkflowRef{}
	afterID, afterRevision := "", int64(0)
	for {
		read, cancel := context.WithTimeout(ctx, startupReadBound)
		page, err := d.store.StoredRevisionsAfter(read, afterID, afterRevision)
		cancel()
		if err != nil {
			return check, fmt.Errorf("refusing to start: the stored workflow revisions after %q revision %d could not be listed (%w), so this binary cannot show that every stored revision loads (FAE-WF-093); retry once the control plane answers", afterID, afterRevision, err)
		}
		for _, row := range page {
			knownRevs[workflowRevisionKey{row.WorkflowID, row.Revision}] = true
			if !row.Retired {
				latest[row.WorkflowID] = max(latest[row.WorkflowID], row.Revision)
			}
			v, known := parsed[row.Hash]
			if !known {
				read, cancel := context.WithTimeout(ctx, startupReadBound)
				var raw []byte
				_, raw, err = d.store.GetWorkflow(read, row.WorkflowID, row.Revision)
				unread := err != nil && read.Err() != nil
				cancel()
				if unread {
					return check, fmt.Errorf("refusing to start: stored workflow %s revision %d (%s) could not be read within %s (%v), so this binary cannot show it loads (FAE-WF-093); retry once the control plane answers", row.WorkflowID, row.Revision, row.Hash, startupReadBound, err)
				}
				if v.load = err; err == nil {
					var doc workflow.Document
					if doc, v.load = workflow.Load(raw); v.load == nil {
						v.refs = subWorkflowRefs(doc)
						_, v.valid = d.parseWorkflow(raw)
					}
				}
				parsed[row.Hash] = v
			}
			if len(v.refs) > 0 {
				subRefs[workflowRevisionKey{row.WorkflowID, row.Revision}] = v.refs
			}
			check.Checked++
			ref := UnloadableRevisionRef{WorkflowID: row.WorkflowID, Revision: row.Revision, Hash: row.Hash}
			switch {
			case v.load != nil:
				ref.Error = v.load.Error()
				bad = append(bad, ref)
			case v.valid != nil:
				ref.Error = v.valid.Error()
				invalid = append(invalid, ref)
			}
		}
		if len(page) < controlplane.StoredRevisionPage() {
			break
		}
		last := page[len(page)-1]
		afterID, afterRevision = last.WorkflowID, last.Revision
	}
	snapshotSubRefs := map[string][]workflow.WorkflowRef{}
	for _, s := range snapshots {
		if backlogTerminal(s.State) || domain.State(s.State).Terminal() {
			continue
		}
		var e envelope
		if decodeEnvelope(s.Payload, &e) == nil {
			refs := subWorkflowRefs(e.Job.Document)
			if c := e.Job.Child; c != nil {
				if c.WorkflowID != "" && c.Revision > 0 {
					refs = append(refs, workflow.WorkflowRef{ID: c.WorkflowID, Revision: c.Revision})
				}
				for _, ch := range c.Children {
					if ch.WorkflowID != "" && ch.Revision > 0 {
						refs = append(refs, workflow.WorkflowRef{ID: ch.WorkflowID, Revision: ch.Revision})
					}
				}
			}
			if len(refs) > 0 {
				snapshotSubRefs[s.ID] = refs
			}
		}
	}
	reachesRevision := func(s controlplane.JobSnapshot, target workflowRevisionKey) bool {
		root := workflowRevisionKey{s.WorkflowID, s.WorkflowRevision}
		if root == target {
			return true
		}
		seen := map[workflowRevisionKey]bool{root: true}
		type item struct {
			key   workflowRevisionKey
			depth int
		}
		var queue []item
		enqueue := func(refs []workflow.WorkflowRef, depth int) {
			for _, ref := range refs {
				if ref.ID == "" {
					continue
				}
				rev := int64(ref.Revision)
				if rev <= 0 {
					rev = latest[ref.ID]
				}
				next := workflowRevisionKey{ref.ID, rev}
				if !seen[next] {
					seen[next] = true
					queue = append(queue, item{next, depth})
				}
			}
		}
		enqueue(subRefs[root], 1)
		enqueue(snapshotSubRefs[s.ID], 1)
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur.key == target {
				return true
			}
			if cur.depth < maxSubWorkflowDepth {
				enqueue(subRefs[cur.key], cur.depth+1)
			}
		}
		return false
	}
	uses := func(r *UnloadableRevisionRef) {
		r.Admits = latest[r.WorkflowID] == r.Revision
		target := workflowRevisionKey{r.WorkflowID, r.Revision}
		for _, s := range snapshots {
			if backlogTerminal(s.State) || domain.State(s.State).Terminal() {
				continue
			}
			if !reachesRevision(s, target) {
				continue
			}
			if q, held := retired[s.ID]; held && q.Version == s.Version && q.Digest == rowDigest(s) {
				continue
			}
			r.Tasks = append(r.Tasks, s.ID)
		}
	}
	var unrunnable []UnloadableRevisionRef
	for i := range invalid {
		uses(&invalid[i])
		if len(invalid[i].Tasks) > 0 {
			unrunnable = append(unrunnable, invalid[i])
			continue
		}
		d.logLine("workflow", "stored workflow %s revision %d (%s) loads but cannot be run by this binary (FAE-WF-093): %s; %s; an admission or migration that binds it is refused\n", invalid[i].WorkflowID, invalid[i].Revision, invalid[i].Hash, invalid[i].Error, unloadableRevisionUses(invalid[i]))
	}
	missingSeen := map[workflowRevisionKey]bool{}
	var missingKeys []workflowRevisionKey
	noteMissingRefs := func(refs []workflow.WorkflowRef) {
		for _, ref := range refs {
			if ref.ID == "" {
				continue
			}
			rev := int64(ref.Revision)
			if rev <= 0 {
				rev = latest[ref.ID]
			}
			key := workflowRevisionKey{ref.ID, rev}
			if !knownRevs[key] && !missingSeen[key] {
				missingSeen[key] = true
				missingKeys = append(missingKeys, key)
			}
		}
	}
	for _, refs := range subRefs {
		noteMissingRefs(refs)
	}
	for _, refs := range snapshotSubRefs {
		noteMissingRefs(refs)
	}
	slices.SortFunc(missingKeys, func(a, b workflowRevisionKey) int {
		if a.workflowID != b.workflowID {
			return strings.Compare(a.workflowID, b.workflowID)
		}
		if a.revision < b.revision {
			return -1
		}
		if a.revision > b.revision {
			return 1
		}
		return 0
	})
	for _, key := range missingKeys {
		ref := UnloadableRevisionRef{
			WorkflowID: key.workflowID,
			Revision:   key.revision,
			Hash:       "missing",
			Error:      fmt.Sprintf("referenced sub-workflow %s revision %d is missing from stored workflows", key.workflowID, key.revision),
		}
		uses(&ref)
		if len(ref.Tasks) > 0 {
			invalid = append(invalid, ref)
			unrunnable = append(unrunnable, ref)
		}
	}
	check.Invalid = invalid
	if len(bad) > 0 {
		for i := range bad {
			uses(&bad[i])
			d.reportUnloadableRevision(ctx, bad[i])
		}
		return check, unloadableRevisionsError(bad)
	}
	if len(unrunnable) == 0 {
		return check, nil
	}
	prior := d.priorUnrunnableQuarantines(ctx)
	quarantine := d.config.QuarantineUnrunnableRevisions
	var refused []UnloadableRevisionRef
	for _, r := range unrunnable {
		key := workflowRevisionKey{r.WorkflowID, r.Revision}
		allRestored := true
		for _, id := range r.Tasks {
			if !prior[key][id] {
				allRestored = false
				break
			}
		}
		if !quarantine && !allRestored {
			d.reportUnrunnableRevision(ctx, r, false)
			refused = append(refused, r)
			continue
		}
		if allRestored {
			d.logLine("workflow", "stored workflow %s revision %d (%s) loads but cannot be run by this binary and active tasks are pinned to it, so the start keeps them quarantined (%s) (FAE-WF-093): %s; %s; to recover: %s\n", r.WorkflowID, r.Revision, r.Hash, unrunnableRevisionFlag, r.Error, unloadableRevisionUses(r), unrunnableRevisionRecovery)
		} else {
			d.reportUnrunnableRevision(ctx, r, true)
		}
		for _, id := range r.Tasks {
			if d.unrunnableRevisionTasks == nil {
				d.unrunnableRevisionTasks = map[string]UnloadableRevisionRef{}
			}
			d.unrunnableRevisionTasks[id] = r
			if prior[key][id] {
				if d.restoredUnrunnableQuarantine == nil {
					d.restoredUnrunnableQuarantine = map[string]bool{}
				}
				d.restoredUnrunnableQuarantine[id] = true
			}
		}
	}
	if len(refused) > 0 {
		return check, unrunnableRevisionsError(refused)
	}
	return check, nil
}

func subWorkflowRefs(doc workflow.Document) []workflow.WorkflowRef {
	var refs []workflow.WorkflowRef
	for _, stage := range doc.Stages {
		if stage.Primitive == workflow.SubWorkflow && stage.Workflow != nil && stage.Workflow.ID != "" {
			refs = append(refs, *stage.Workflow)
		}
	}
	return refs
}

// priorUnrunnableQuarantines reads earlier unrunnable-workflow-revision events
// that quarantined tasks under --quarantine-unrunnable-revisions, so a
// subsequent start without the flag keeps those tasks quarantined instead of
// refusing again before the operator migrates or retires them (FAE-WF-093).
func (d *Daemon) priorUnrunnableQuarantines(ctx context.Context) map[workflowRevisionKey]map[string]bool {
	out := map[workflowRevisionKey]map[string]bool{}
	read, cancel := context.WithTimeout(ctx, startupReadBound)
	events, _, err := d.store.EventsOfKindSince(read, "unrunnable-workflow-revision", time.Time{}, 10000)
	cancel()
	if err != nil {
		return out
	}
	for _, ev := range events {
		var rec struct {
			WorkflowID  string   `json:"workflow_id"`
			Revision    int64    `json:"revision"`
			Tasks       []string `json:"tasks"`
			Quarantined bool     `json:"quarantined"`
		}
		if json.Unmarshal(ev.Payload, &rec) != nil || !rec.Quarantined {
			continue
		}
		key := workflowRevisionKey{rec.WorkflowID, rec.Revision}
		if out[key] == nil {
			out[key] = map[string]bool{}
		}
		for _, id := range rec.Tasks {
			out[key][id] = true
		}
	}
	return out
}

// startupRevisionInstalled records the revision installDefault wrote after
// the check: its write validated it (Parse), so it counts as checked, and
// when it replaced the workflow's latest, no listed revision of that
// workflow is the one new admissions bind any more.
func (d *Daemon) startupRevisionInstalled(workflowID string, upgraded bool) {
	d.updateStartupReport(func(r *StartupChecks) {
		r.Revisions.Checked++
		for i := range r.Revisions.Invalid {
			if upgraded && r.Revisions.Invalid[i].WorkflowID == workflowID {
				r.Revisions.Invalid[i].Admits = false
			}
		}
	})
}

// unloadableRevisionUses says which active tasks are pinned to an unloadable
// or unrunnable revision and whether new admissions bind it.
func unloadableRevisionUses(bad UnloadableRevisionRef) string {
	uses := "no active task is pinned to it"
	if len(bad.Tasks) > 0 {
		uses = fmt.Sprintf("active tasks %v are pinned to it", bad.Tasks)
	}
	if bad.Admits {
		uses += "; it is the workflow's latest revision, which new admissions bind"
	}
	return uses
}

// unloadableRevisionRecovery is how the operator recovers from a start
// refused for unloadable revisions.
const unloadableRevisionRecovery = "every stored revision must stay loadable by later binaries, so this binary cannot run this instance: start the binary that wrote the revision again (it loads it, and the pinned tasks keep running on it) and report the error against this binary; if the revision's stored content is damaged rather than newer than this binary, restore the control plane from a backup taken before the damage"

// unloadableRevisionsError is the start's refusal for revisions this binary
// cannot load (FAE-WF-093).
func unloadableRevisionsError(bad []UnloadableRevisionRef) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to start: %d stored workflow revision(s) cannot be loaded by this binary (FAE-WF-093)", len(bad))
	for i, r := range bad {
		if i == startupUnloadableNamed {
			fmt.Fprintf(&b, "; and %d more, each named in the daemon log", len(bad)-i)
			break
		}
		fmt.Fprintf(&b, "; workflow %s revision %d (%s): %s; %s", r.WorkflowID, r.Revision, r.Hash, r.Error, unloadableRevisionUses(r))
	}
	b.WriteString(". To recover: " + unloadableRevisionRecovery)
	return errors.New(b.String())
}

// reportUnloadableRevision writes the daemon line and the audited alert for a
// stored revision this binary cannot load; the start then refuses.
func (d *Daemon) reportUnloadableRevision(ctx context.Context, bad UnloadableRevisionRef) {
	d.logLine("workflow", "stored workflow %s revision %d (%s) cannot be loaded by this binary, so the start refuses (FAE-WF-093): %s; %s; to recover: %s\n", bad.WorkflowID, bad.Revision, bad.Hash, bad.Error, unloadableRevisionUses(bad), unloadableRevisionRecovery)
	body, _ := json.Marshal(bad)
	if preMigrationSchema(ctx) {
		d.appendStartupAlertFile(body)
		return
	}
	if err := d.store.ControllerAction(ctx, controlplane.Audit{Actor: "daemon", Action: "unloadable-workflow-revision", Inputs: body, Outcome: "alert"}, body, true); err != nil {
		d.logLine("workflow", "warning: unloadable revision alert not recorded: %v\n", err)
		d.appendStartupAlertFile(body)
		return
	}
	_ = d.mirrorAlerts(ctx)
}

// unrunnableRevisionRecovery is how the operator recovers from a start
// refused for an unrunnable revision with active tasks pinned to it. It
// needs no older binary.
const unrunnableRevisionRecovery = "this binary never drives a task on a revision it cannot validate, so start it once with `fae serve ... " + unrunnableRevisionFlag + "`: that start proceeds and quarantines exactly these tasks (an audited alert and event each; `fae status` lists them under snapshot.quarantined and startup_checks.revisions.quarantined), and the flag is a no-op once no such task is left; then move each task to a revision this binary validates (`fae migrate --task ID --revision N`), or retire it keeping its history (`fae quarantine retire --task ID --reason TEXT`); `fae quarantine release` refuses while the revision stays unrunnable. Or start the binary that wrote the revision again. Nothing was written to the stored workflows or tasks"

// unrunnableRevisionsError is the start's refusal for revisions this binary
// reads but cannot run while active tasks are pinned to them (FAE-WF-093).
func unrunnableRevisionsError(unrunnable []UnloadableRevisionRef) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to start: %d stored workflow revision(s) cannot be run by this binary and have active tasks pinned to them (FAE-WF-093)", len(unrunnable))
	for i, r := range unrunnable {
		if i == startupUnloadableNamed {
			fmt.Fprintf(&b, "; and %d more, each named in the daemon log", len(unrunnable)-i)
			break
		}
		fmt.Fprintf(&b, "; workflow %s revision %d (%s): %s; %s", r.WorkflowID, r.Revision, r.Hash, r.Error, unloadableRevisionUses(r))
	}
	b.WriteString(". To recover: " + unrunnableRevisionRecovery)
	return errors.New(b.String())
}

// unrunnableRevisionQuarantine is the quarantine reason of a task pinned to
// an unrunnable revision, and what quarantineRelease answers for it. The
// recovery comes before the failing rules, which can run past the 1024
// bytes a quarantine reason keeps.
func unrunnableRevisionQuarantine(ref UnloadableRevisionRef) string {
	return fmt.Sprintf("workflow %s revision %d (%s) cannot be run by this binary (FAE-WF-093); migrate the task to a revision this binary validates (fae migrate) or retire it (fae quarantine retire); release refuses while the revision stays unrunnable; the failing rules: %s", ref.WorkflowID, ref.Revision, ref.Hash, ref.Error)
}

// reportUnrunnableRevision writes the daemon line and the audited alert for a
// stored revision this binary reads but cannot run while active tasks are
// pinned to it: the start then refuses, or quarantines those tasks.
func (d *Daemon) reportUnrunnableRevision(ctx context.Context, ref UnloadableRevisionRef, quarantine bool) {
	outcome := "so the start refuses"
	if quarantine {
		outcome = "so the start quarantines them (" + unrunnableRevisionFlag + ")"
	}
	d.logLine("workflow", "stored workflow %s revision %d (%s) loads but cannot be run by this binary and active tasks are pinned to it, %s (FAE-WF-093): %s; %s; to recover: %s\n", ref.WorkflowID, ref.Revision, ref.Hash, outcome, ref.Error, unloadableRevisionUses(ref), unrunnableRevisionRecovery)
	body, _ := json.Marshal(struct {
		UnloadableRevisionRef
		Quarantined bool `json:"quarantined"`
	}{ref, quarantine})
	if preMigrationSchema(ctx) && !quarantine {
		d.appendStartupAlertFile(body)
		return
	}
	if err := d.store.ControllerAction(ctx, controlplane.Audit{Actor: "daemon", Action: "unrunnable-workflow-revision", Inputs: body, Outcome: "alert"}, body, true); err != nil {
		d.logLine("workflow", "warning: unrunnable revision alert not recorded: %v\n", err)
		d.appendStartupAlertFile(body)
		return
	}
	_ = d.mirrorAlerts(ctx)
}

func (d *Daemon) appendStartupAlertFile(body json.RawMessage) {
	if d.layout.Root == "" {
		return
	}
	now := time.Now().UTC()
	line, err := json.Marshal(alertFileLine{Event: "raised", Severity: "error", Source: "controller", CreatedAt: &now, Detail: body})
	if err != nil {
		return
	}
	f, err := os.OpenFile(d.alertsFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Sync()
	_ = f.Close()
}
