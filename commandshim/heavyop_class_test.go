package commandshim

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
)

// FAE-HOST-002, -005, FAE-SCH-011: a controller- or operator-class heavy-op
// run under soft pressure waits for the host until its maximum wait, as
// landing and gate run do, and runs once the hold lifts; only a role-class
// run backs off at most the policy's re-checks and then refuses. Before,
// every class refused at the role bound (about 30 s under the default
// policy), before a hold could ever lift.
func TestHeavyOpControllerClassWaitsPastTheRoleBound(t *testing.T) {
	loaded, _ := healthyHost(context.Background())
	loaded.LoadPerCore.Value = 4.5
	policy := hostguard.DefaultPolicy()
	held := policy.Checks + 5 // readings under pressure: past the role bound
	for _, c := range []struct {
		class string
		code  int
	}{{"controller", 0}, {"operator", 0}, {"role", HeavyOpRefused}} {
		reads := 0
		observe := func(ctx context.Context) (hostguard.Sample, error) {
			reads++
			if reads <= held {
				return loaded, nil
			}
			return healthyHost(ctx)
		}
		var stderr bytes.Buffer
		clock := &heavyOpClock{now: time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)}
		code, err := RunHeavyOp(context.Background(), HeavyOp{Ledger: heavyLedger(t, 2), Weight: 1, Class: c.class, Argv: []string{"/bin/true"}, Observe: observe, Clock: clock, Policy: policy, Stderr: &stderr})
		if code != c.code || (code == 0) != (err == nil) {
			t.Errorf("%s class under %d held readings: exit %d %v\n%s", c.class, held, code, err, stderr.String())
		}
		if c.class == "role" && reads != policy.Checks+1 {
			t.Errorf("role class read the host %d times, want the first and %d re-checks", reads, policy.Checks)
		}
		if c.class != "role" && reads <= held {
			t.Errorf("%s class ran after %d readings, before the hold lifted", c.class, reads)
		}
	}
}
