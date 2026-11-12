package workflow

func mistakenItemComparison(a, b Expr) bool {
	scope := func(x Expr) (int, bool) {
		if x.Op == "item" {
			return x.ItemDepth, true
		}
		if x.Op == "outer" {
			return 1, true
		}
		return 0, false
	}
	aDepth, aItem := scope(a)
	bDepth, bItem := scope(b)
	return aItem && bItem && aDepth == bDepth && ((len(a.Path) == 0 && len(b.Path) > 0) || (len(b.Path) == 0 && len(a.Path) > 0))
}
