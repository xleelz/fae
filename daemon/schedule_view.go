package daemon

import (
	"sort"
	"strconv"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
)

// scheduleView is the drive's per-round view of the durable jobs that live
// admission cannot read under the drive mutex (FAE-SCH-005, -006, -008).
type scheduleView struct {
	treeRoot    map[string]string
	treeLimit   map[string]int
	treeRoleCap map[string]int
	// priority breaks role- and heavy-queue ties between equal waits.
	priority map[string]int
	// molecule is each task's bound molecule identity, one per (workflow id,
	// revision); its tasks share the molecule's task-bound pools.
	molecule map[string]string
	// revision is each task's pinned workflow revision.
	revision map[string]int
	// admitted is each task's admission time, zero when not recorded.
	admitted map[string]time.Time
	// cutOffByQuota marks tasks whose in-flight turn was cut off by a usage
	// limit (FAE-RT-018) so they are re-dispatched first at reset time T.
	cutOffByQuota map[string]bool
	// live lists the workflow-global roles of live molecules: those with a
	// non-terminal task, exactly the molecules whose ended_at is unset.
	live []scheduler.RoleRevision
}

// moleculeKey is the bound molecule, or the revision for a job bound before
// molecules were recorded (the plane binds it at its next checkpoint).
func moleculeKey(s controlplane.JobSnapshot) string {
	if s.MoleculeID != "" {
		return s.MoleculeID
	}
	return "revision\x00" + strconv.FormatInt(s.WorkflowRevision, 10)
}

func newScheduleView(jobs map[string]controlplane.JobSnapshot) (*scheduleView, error) {
	v := &scheduleView{treeRoot: map[string]string{}, treeLimit: map[string]int{}, treeRoleCap: map[string]int{}, priority: map[string]int{}, molecule: map[string]string{}, revision: map[string]int{}, admitted: map[string]time.Time{}, cutOffByQuota: map[string]bool{}}
	seen := map[scheduler.RoleRevision]bool{}
	for id, s := range jobs {
		var e envelope
		if err := decodeEnvelope(s.Payload, &e); err != nil {
			return nil, err
		}

		v.treeRoot[id] = engine.RootTask(e.Job)
		if engine.RootTask(e.Job) == id {
			v.treeLimit[id] = e.Job.MaxSessions
		}
		v.priority[id], v.admitted[id], v.cutOffByQuota[id] = e.Admission.Priority, e.AdmittedAt, e.Job.CutOffByQuota
		v.molecule[id], v.revision[id] = moleculeKey(s), e.Job.Revision
		if e.Job.State.Terminal() {
			continue
		}
		for name, role := range e.Job.Document.Roles {
			if role.CapScope == "tree" {
				key := engine.RootTask(e.Job) + "\x00" + name
				v.treeRoleCap[key] = smallerCap(v.treeRoleCap[key], role.Cap)
			}
		}
		for name, role := range e.Job.Document.Roles {
			r := scheduler.RoleRevision{Workflow: e.Job.Document.ID, Molecule: v.molecule[id], Role: name, Revision: e.Job.Revision, Cap: role.Cap, Global: true, Live: true}
			if role.Scope == "workflow-global" && (role.Cap > 0 || role.Cap == -1) && !seen[r] {
				seen[r] = true
				v.live = append(v.live, r)
			}
		}
	}
	sort.Slice(v.live, func(i, j int) bool {
		a, b := v.live[i], v.live[j]
		if a.Workflow != b.Workflow {
			return a.Workflow < b.Workflow
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		if a.Revision != b.Revision {
			return a.Revision < b.Revision
		}
		return a.Molecule < b.Molecule
	})
	return v, nil
}

// globalCap is the FAE-SCH-006 cap of a workflow-global role: that of the
// newest revision with a non-terminal task, else the caller's own.
func (v *scheduleView) globalCap(workflowID, role, molecule string, revision, own int) (int, error) {
	if molecule == "" {
		molecule = "unscheduled"
	}
	_, c, err := scheduler.RolePool(scheduler.RoleRevision{Workflow: workflowID, Molecule: molecule, Role: role, Revision: revision, Cap: own, Global: true, Live: true}, v.live)
	return c, err
}

// roleCapChoice reports the FAE-SCH-006 choice for one workflow-global role:
// the revision whose cap the shared pool uses and every live revision's cap.
type roleCapChoice struct {
	Workflow string         `json:"workflow"`
	Role     string         `json:"role"`
	Revision int            `json:"revision"`
	Cap      int            `json:"cap"`
	Live     map[string]int `json:"live_revision_caps"`
}

func (v *scheduleView) roleCaps() []roleCapChoice {
	out := []roleCapChoice{}
	for _, r := range v.live {
		if n := len(out); n == 0 || out[n-1].Workflow != r.Workflow || out[n-1].Role != r.Role {
			out = append(out, roleCapChoice{Workflow: r.Workflow, Role: r.Role, Live: map[string]int{}})
		}
		c := &out[len(out)-1]
		c.Live[strconv.Itoa(r.Revision)] = r.Cap
		if r.Revision >= c.Revision {
			c.Revision, c.Cap = r.Revision, r.Cap
		}
	}
	return out
}
