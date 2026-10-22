package engine

import "time"

const DefaultReviewPollSeconds = 30
const DefaultReviewMaxPolls = 120

// parkReviewPoll counts pending observations separately from implementation
// attempts. The durable retry deadline and count survive daemon replacement.
func parkReviewPoll(j *Job, d Dispatch, detail string, events *[]Event) {
	if d.AttemptCharged && j.Attempts > 0 {
		j.Attempts--
	}
	j.ReviewPollCount++
	limit := j.ReviewMaxPolls
	if limit == 0 {
		limit = DefaultReviewMaxPolls
	}
	interval := j.ReviewPollSeconds
	if interval == 0 {
		interval = DefaultReviewPollSeconds
	}
	j.InFlight = nil
	if j.ReviewPollCount >= limit {
		stop(j, "ReviewPollCap: pending review observation budget exhausted", events)
		return
	}
	j.Parked = true
	j.Reason = "ReviewPending"
	j.AdmissionRetryAfter = time.Now().Add(time.Duration(interval) * time.Second)
	event(j, Event{Kind: "review-wait", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: detail}, events)
}
func resetReviewPoll(j *Job) { j.ReviewPollCount = 0; j.AdmissionRetryAfter = time.Time{} }
