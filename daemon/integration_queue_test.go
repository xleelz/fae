package daemon

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

func TestIntegrationSegmentOfDefault(t *testing.T) {
	got := integrationSegment(defaults.Workflow())
	want := map[string]bool{"merge-route": true, "post-merge-qa": true, "post-merge-gate": true, "commit": true, "integrate": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// FAE-VCS-008: one task at a time holds the integration window, from its
// claimed Merge dispatch until it lands or leaves; escalated and
// operator-held tasks never hold it, a host-deferred holder keeps it.
func TestIntegrationQueueSerialisesTheWindow(t *testing.T) {
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	put := func(id, state string, mutate func(*engine.Job)) {
		j, err := engine.NewLive(id, 1, defaults.Workflow(), "tree")
		if err != nil {
			t.Fatal(err)
		}
		j.State = domain.State(state)
		if mutate != nil {
			mutate(&j)
		}
		raw, _ := json.Marshal(envelope{Job: j})
		d.jobs[id] = controlplane.JobSnapshot{ID: id, State: state, Payload: raw}
	}
	q := &integrationQueue{}
	claim := func(task, dispatch, want string) {
		t.Helper()
		if got, err := q.claim(d, task, dispatch); err != nil || got != want {
			t.Fatalf("%s claim: holder %q want %q (%v)", task, got, want, err)
		}
	}
	put("a", "post-merge-qa", nil)
	claim("b", "b/1", "a")
	put("a", "post-merge-qa", func(j *engine.Job) { j.Parked, j.Reason = true, "infrastructure-deferred" })
	claim("b", "b/1", "a")
	put("a", "commit", func(j *engine.Job) { j.Parked, j.Reason = true, "UncertainDispatch" })
	claim("b", "b/1", "")
	// b's claimed Merge dispatch holds the window until it persists a stage.
	put("b", "merge", func(j *engine.Job) { j.InFlight = &engine.Dispatch{ID: "b/1"} })
	claim("c", "c/1", "b")
	put("b", "implement", nil)
	claim("c", "c/1", "")
	put("c", "integrate", nil)
	put("b", "Escalated", nil)
	claim("b", "b/2", "c")
	put("c", "Done", nil)
	claim("b", "b/2", "")
}
