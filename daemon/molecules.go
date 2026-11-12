package daemon

import (
	"sort"

	"github.com/xleelz/fae/controlplane"
)

// validMoleculeLimit accepts −1 (unbounded, the Appendix A default) or a
// positive maximum of running molecules.
func validMoleculeLimit(n int) bool { return n == -1 || (n >= 1 && n <= 1000000) }

// runningMolecules maps each workflow to its running molecules: those with a
// task released into execution, in a stage or Blocked. A Pending task waits
// for release and an Escalated one for the operator; neither runs its
// molecule, and a molecule with only terminal tasks has ended (FAE-SCH-007).
func runningMolecules(jobs map[string]controlplane.JobSnapshot) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, j := range jobs {
		if j.State == "Pending" || j.State == "Escalated" || backlogTerminal(j.State) {
			continue
		}
		if out[j.WorkflowID] == nil {
			out[j.WorkflowID] = map[string]bool{}
		}
		out[j.WorkflowID][moleculeKey(j)] = true
	}
	return out
}

// tighter is the smaller limit, −1 meaning unbounded.
func tighter(a, b int) int {
	if a == -1 || b != -1 && b < a {
		return b
	}
	return a
}

// releasesMolecule reports whether a Pending job may leave Pending under the
// running-molecule limits: the instance-wide limit, and the tighter of the
// operator's per-workflow limit and the one its revision's content declares
// (FAE-WF-001). It costs nothing while every limit is −1.
func (d *Daemon) releasesMolecule(j controlplane.JobSnapshot, declared int) bool {
	limit, workflowLimit := d.config.MaxMolecules, tighter(d.config.MaxWorkflowMolecules, declared)
	if limit == -1 && workflowLimit == -1 {
		return true
	}
	running := runningMolecules(d.jobs)
	total := 0
	for _, molecules := range running {
		total += len(molecules)
	}
	return CanReleaseIntoMolecule(running[j.WorkflowID][moleculeKey(j)], total, limit, len(running[j.WorkflowID]), workflowLimit)
}

// moleculeStatus reports running molecules by workflow against the limits.
func (d *Daemon) moleculeStatus() map[string]any {
	running := runningMolecules(d.jobs)
	byWorkflow := map[string]int{}
	total := 0
	ids := []string{}
	for workflow, molecules := range running {
		byWorkflow[workflow] = len(molecules)
		total += len(molecules)
		for id := range molecules {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	// Status stays small: the Topology read pages every running molecule.
	return map[string]any{"running": total, "running_by_workflow": byWorkflow, "running_molecules": ids[:min(len(ids), 100)], "limit": d.config.MaxMolecules, "workflow_limit": d.config.MaxWorkflowMolecules}
}
