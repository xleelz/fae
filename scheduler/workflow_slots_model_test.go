package scheduler

import "testing"

// Exhaustively enumerate three owners acquiring/releasing a unit footprint.
// A successful atomic ledger transaction is one transition. The deliberately
// split control instead caches eligibility outside the commitment transaction.
type workflowSlotModel struct {
	used     int
	phase    [3]uint8
	held     [3]bool
	eligible [3]bool
}

func exploreWorkflowSlotModel(split bool) (states int, violation bool) {
	seen := map[workflowSlotModel]bool{}
	var visit func(workflowSlotModel)
	visit = func(s workflowSlotModel) {
		if seen[s] {
			return
		}
		seen[s] = true
		states++
		sum := 0
		for _, held := range s.held {
			if held {
				sum++
			}
		}
		if s.used < 0 || s.used > 2 || sum != s.used {
			violation = true
			return
		}
		for actor := range s.phase {
			next := s
			switch s.phase[actor] {
			case 0:
				if split {
					next.eligible[actor] = s.used < 2
					next.phase[actor] = 1
				} else if s.used < 2 {
					next.used++
					next.held[actor] = true
					next.phase[actor] = 2
				} else {
					next.phase[actor] = 3
				}
			case 1:
				if s.eligible[actor] {
					next.used++
					next.held[actor] = true
					next.phase[actor] = 2
				} else {
					next.phase[actor] = 3
				}
			case 2:
				next.used--
				next.held[actor] = false
				next.phase[actor] = 3
			default:
				continue
			}
			visit(next)
		}
	}
	visit(workflowSlotModel{})
	return
}
func TestWorkflowSlotAtomicInterleavingModelAndSplitCommitControl(t *testing.T) {
	states, bad := exploreWorkflowSlotModel(false)
	if bad || states < 20 {
		t.Fatalf("atomic commitment model: states%d violation%v", states, bad)
	}
	splitStates, bad := exploreWorkflowSlotModel(true)
	if !bad || splitStates < states {
		t.Fatalf("negative split read/commit control failed to expose overcommit: states%d violation%v", splitStates, bad)
	}
	t.Logf("atomic reachable states%d; split-commit control reachable states%d (overcommit found)", states, splitStates)
}
