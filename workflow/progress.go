package workflow

// Progress lets an iteration of a Loop that strictly reduces Measure, a
// count of open blocking findings, not consume an iteration; Streak bounds
// consecutive iterations without such a reduction (FAE-WF-075).
type Progress struct {
	Measure Expr `json:"measure"`
	Streak  int  `json:"streak"`
}

// Measure evaluates a progress count over evidence; ok is false when the
// count is unknown (for example its evidence is missing).
func Measure(x Expr, evidence []Evidence) (float64, bool) {
	e := evaluator{evidence: evidence, ids: map[string]bool{}}
	v := e.eval(x, 0)
	n, ok := number(v.data)
	return n, ok && !v.unknown
}

func validateProgress(d Document, id string, s Stage, bad func(string, string)) {
	if s.Progress == nil {
		return
	}
	p := "stages." + id + ".progress"
	if s.Primitive != Loop {
		bad(p, "progress applies to a Loop")
	}
	if s.Progress.Measure.Op != "count" || ValidateExpr(s.Progress.Measure) != nil {
		bad(p+".measure", "must be a valid count expression")
	}
	selectors(d, s.Progress.Measure, p+".measure", bad)
	if s.Progress.Streak < 1 {
		bad(p+".streak", "a positive no-improvement bound is required")
	}
}
