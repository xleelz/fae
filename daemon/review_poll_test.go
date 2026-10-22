package daemon

import (
	"bytes"
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"strings"
	"testing"
	"time"
)

func TestReviewPollingUsesDurableSeparateBudget(t *testing.T) {
	for _, operation := range []string{"review_read", "review_publish", "review_merge"} {
		t.Run(operation, func(t *testing.T) {
			j := externalJob(t, "tree", "base", true)
			j.ReviewStages = map[string]string{"external": operation}
			j.ReviewPollSeconds = 45
			j.ReviewMaxPolls = 3
			originalAttempts := j.Attempts
			for poll := 1; poll <= 3; poll++ {
				dispatch := *j.InFlight
				result := engine.Result{ExecutionKind: "live", TaskID: j.TaskID, DispatchID: dispatch.ID, Tree: j.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now(), Class: "review-wait", Detail: "vendor check still Blocked"}
				var err error
				j, _, err = engine.Complete(j, dispatch.ID, result)
				if err != nil {
					t.Fatal(err)
				}
				if j.Attempts != originalAttempts || j.ReviewPollCount != poll {
					t.Fatal("poll consumed implementation budget", j.Attempts, originalAttempts, j.ReviewPollCount)
				}
				if poll == 3 {
					if j.State != domain.Escalated || !strings.Contains(j.Reason, "ReviewPollCap") {
						t.Fatal(j.State, j.Reason)
					}
					break
				}
				if !j.Parked || j.Reason != "ReviewPending" || time.Until(j.AdmissionRetryAfter) < 40*time.Second {
					t.Fatal("poll interval not applied", j.Parked, j.Reason, j.AdmissionRetryAfter)
				}
				_, early, _, err := engine.Prepare(j)
				if err != nil || early != nil {
					t.Fatal("polled before retry deadline", early, err)
				}
				// The daemon persists the whole job. Simulate replacement using precisely
				// that serialized checkpoint; neither count nor retry deadline may reset.
				deadline := j.AdmissionRetryAfter
				raw, _ := json.Marshal(j)
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				var recovered engine.Job
				if err = decoder.Decode(&recovered); err != nil {
					t.Fatal(err)
				}
				j = recovered
				var replay *engine.Dispatch
				j, replay, err = engine.Recover(j, false, false)
				if err != nil || replay != nil {
					t.Fatal("parked poll replayed on daemon recovery", replay, err)
				}
				if j.ReviewPollCount != poll || j.ReviewMaxPolls != 3 || j.ReviewPollSeconds != 45 || !j.AdmissionRetryAfter.Equal(deadline) {
					t.Fatal("replacement lost polling checkpoint")
				}
				j.AdmissionRetryAfter = time.Now().Add(-time.Second)
				// Even an exhausted implementation-attempt budget cannot block collecting
				// the result of an already admitted external build.
				if poll == 1 {
					j.Attempts = j.Document.Limits.Attempts
					originalAttempts = j.Attempts
				}
				var next *engine.Dispatch
				j, next, _, err = engine.Prepare(j)
				if err != nil || next == nil || next.AttemptCharged {
					t.Fatal("pending poll charged or blocked", next, err, j.State, j.Reason)
				}
			}
		})
	}
}

func TestReviewPollingDefaultsAndConfigurationBounds(t *testing.T) {
	if engine.DefaultReviewPollSeconds != 30 || engine.DefaultReviewMaxPolls != 120 {
		t.Fatal("unexpected operator defaults")
	}
	j := externalJob(t, "tree", "base", true)
	j.ReviewStages = map[string]string{"external": "review_read"}
	dispatch := *j.InFlight
	result := engine.Result{ExecutionKind: "live", TaskID: j.TaskID, DispatchID: dispatch.ID, Tree: j.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now(), Class: "review-wait"}
	j, _, err := engine.Complete(j, dispatch.ID, result)
	if err != nil || j.ReviewPollCount != 1 || time.Until(j.AdmissionRetryAfter) < 25*time.Second {
		t.Fatal(j.ReviewPollCount, j.AdmissionRetryAfter, err)
	}
	for _, invalid := range []engine.Job{{ReviewPollCount: -1}, {ReviewPollSeconds: -1}, {ReviewMaxPolls: -1}, {ReviewPollSeconds: 3601}, {ReviewMaxPolls: 3601}} {
		candidate := j
		candidate.ReviewPollCount = invalid.ReviewPollCount
		candidate.ReviewPollSeconds = invalid.ReviewPollSeconds
		candidate.ReviewMaxPolls = invalid.ReviewMaxPolls
		if err := engine.Validate(candidate); err == nil {
			t.Fatal("accepted invalid polling bounds", invalid)
		}
	}
}

func TestReviewPolicyPollingDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		interval, limit int
		valid           bool
	}{{"defaults", 0, 0, true}, {"explicit", 60, 40, true}, {"negative interval", -1, 10, false}, {"negative budget", 30, -1, false}, {"oversized interval", 3601, 10, false}, {"oversized budget", 30, 3601, false}} {
		t.Run(tc.name, func(t *testing.T) {
			j := externalJob(t, "tree", "base", true)
			c := LiveConfig{ExternalTools: map[string]external.Tool{"fixture": {Name: "review_create"}}, ReviewPolicies: map[string]external.ReviewPolicy{j.TaskID: {PollSeconds: tc.interval, MaxPolls: tc.limit}}}
			e := configureReviewJob(c, &j)
			if (e == nil) != tc.valid {
				t.Fatal(e)
			}
			if tc.valid {
				wantInterval, wantLimit := tc.interval, tc.limit
				if wantInterval == 0 {
					wantInterval = 30
				}
				if wantLimit == 0 {
					wantLimit = 120
				}
				if j.ReviewPollSeconds != wantInterval || j.ReviewMaxPolls != wantLimit {
					t.Fatal(j.ReviewPollSeconds, j.ReviewMaxPolls)
				}
			}
		})
	}
}
