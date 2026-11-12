package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// prophetObservation is one health observation routeProphet recorded.
type prophetObservation struct {
	model   string
	attempt int
	outcome providerhealth.Outcome
}

// FAE-RT-004, FAE-RT-009: a Prophet turn whose provider never acknowledged
// the prompt follows the workflow's start rule. The same route is relaunched,
// charging nothing, up to the agent-start bound (3 starts); the start that
// exhausts it charges the binding Unavailable once, and the turn fails over
// within the budget. A provider that acknowledges on a later start is
// charged as usual. A start whose retirement is unconfirmed ends the turn
// with no relaunch and no charge, and a failed exhaustion charge ends it with
// no failover.
func TestProphetChargesAProviderThatNeverAcknowledged(t *testing.T) {
	notStarted := &agentruntime.TurnError{Class: agentruntime.ProviderNotStarted, Role: "prophet", Cause: &agentruntime.BridgeRefusal{Class: agentruntime.ProviderNotStarted, Detail: "provider exited before acknowledging the prompt"}}
	for _, tc := range []struct {
		name            string
		silentStarts    int // the first route's starts that never acknowledge
		budget          int
		retired         bool
		observeError    bool
		success         bool
		launches        []string
		observations    []prophetObservation
		errorMentionsNA bool
	}{
		{name: "exhausted, then failover", silentStarts: 3, retired: true, success: true,
			launches:     []string{"first", "first", "first", "second"},
			observations: []prophetObservation{{"first", 2, providerhealth.Unavailable}, {"second", 3, providerhealth.OK}}},
		{name: "acknowledged on the third start", silentStarts: 2, retired: true, success: true,
			launches:     []string{"first", "first", "first"},
			observations: []prophetObservation{{"first", 2, providerhealth.OK}}},
		{name: "exhausted with failover disabled", silentStarts: 3, budget: -1, retired: true,
			launches:     []string{"first", "first", "first"},
			observations: []prophetObservation{{"first", 2, providerhealth.Unavailable}}, errorMentionsNA: true},
		{name: "retirement unconfirmed", silentStarts: 3,
			launches: []string{"first"}, errorMentionsNA: true},
		{name: "exhaustion charge fails", silentStarts: 3, retired: true, observeError: true,
			launches:     []string{"first", "first", "first"},
			observations: []prophetObservation{{"first", 2, providerhealth.Unavailable}}, errorMentionsNA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var launches []string
			var observations []prophetObservation
			starts := 0
			c := Config{ProphetRoutes: []workflow.Binding{{Provider: "p", Model: "first"}, {Provider: "p", Model: "second"}}, ProphetMaxFailovers: tc.budget}
			raw, err := routeProphet(context.Background(), c, func(context.Context) ([]providerhealth.Health, error) { return nil, nil }, func(_ context.Context, route workflow.Binding, attempt int) prophetAttempt {
				launches = append(launches, route.Model)
				if route.Model == "first" {
					starts++
					if starts <= tc.silentStarts {
						return prophetAttempt{collected: agentruntime.Collected{ProcessesRetired: tc.retired}, cleanupOK: true, err: notStarted}
					}
				}
				return prophetAttempt{collected: agentruntime.Collected{Output: validProphetReply, ProcessesRetired: true}, cleanupOK: true}
			}, func(_ context.Context, route workflow.Binding, attempt int, o providerhealth.Outcome) error {
				observations = append(observations, prophetObservation{route.Model, attempt, o})
				if tc.observeError {
					return errors.New("commit unknown")
				}
				return nil
			})
			if (err == nil) != tc.success || fmt.Sprint(launches) != fmt.Sprint(tc.launches) || fmt.Sprint(observations) != fmt.Sprint(tc.observations) {
				t.Fatalf("err=%v raw=%s launches %v observations %v; want launches %v observations %v", err, raw, launches, observations, tc.launches, tc.observations)
			}
			if tc.success && !json.Valid(raw) {
				t.Fatalf("reply %s", raw)
			}
			var refusal *agentruntime.BridgeRefusal
			if tc.errorMentionsNA && (!errors.As(err, &refusal) || refusal.Class != agentruntime.ProviderNotStarted) {
				t.Fatalf("error %v does not carry the start failure", err)
			}
		})
	}
}
