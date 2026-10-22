package kernel

// CanShedIdle is the actual opt-in idle adapter retirement guard. Protected
// final-judgement/QA sessions and any authoritative active turn are excluded.
// @ ensures ok == (global && preCode && !protected && !active && inflight == 0)
// @ ensures ok ==> global && preCode && !protected && !active && inflight == 0
func CanShedIdle(global, preCode, protected, active bool, inflight int) (ok bool) {
	return global && preCode && !protected && !active && inflight == 0
}
