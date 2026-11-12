package daemon

// CanReleaseIntoMolecule is the FAE-SCH-007 release rule for a Pending task. A
// task whose molecule already runs is never capped: one molecule holds every
// task of a revision. Otherwise its release starts a molecule, admitted only
// below the instance-wide and the per-workflow running-molecule limits; −1
// means unbounded.
// @ ensures moleculeRunning ==> ok
// @ ensures ok && !moleculeRunning ==> (limit == -1 || running < limit)
// @ ensures ok && !moleculeRunning ==> (workflowLimit == -1 || workflowRunning < workflowLimit)
// @ ensures ok == (moleculeRunning || ((limit == -1 || running < limit) && (workflowLimit == -1 || workflowRunning < workflowLimit)))
func CanReleaseIntoMolecule(moleculeRunning bool, running, limit, workflowRunning, workflowLimit int) (ok bool) {
	if moleculeRunning {
		return true
	}
	return (limit == -1 || running < limit) && (workflowLimit == -1 || workflowRunning < workflowLimit)
}
