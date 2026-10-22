package daemon

import "testing"

func TestRestartGuard(t *testing.T) {
	for _, head := range []bool{false, true} {
		for _, tree := range []bool{false, true} {
			if got := CanRestartWorkspace(head, tree); got != (head && tree) {
				t.Fatalf("head=%v tree=%v accepted=%v", head, tree, got)
			}
		}
	}
}
