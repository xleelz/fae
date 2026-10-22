// Package cron implements anchored interval recurrence, independently of provider loops.
package cron

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const DefaultCatchUp = 10
const MaxCatchUp = 100
const maxUnix = int64(253402300799) // last second representable by RFC3339
var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)

type Template struct {
	TargetConfig   string `json:"target_config,omitempty"`
	TargetRevision int64  `json:"target_revision,omitempty"`
	WorkflowID     string `json:"workflow_id"`
	Revision       int64  `json:"revision"`
	Title          string `json:"title,omitempty"`
	Content        string `json:"content,omitempty"`
	Tree           string `json:"tree,omitempty"`
	Priority       int    `json:"priority,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	Mode           string `json:"mode,omitempty"`
}
type Spec struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Anchor          time.Time `json:"anchor"`
	IntervalSeconds int64     `json:"interval_seconds"`
	End             time.Time `json:"end,omitempty"`
	CatchUpLimit    int       `json:"catch_up_limit,omitempty"`
	Template        Template  `json:"template"`
}
type Schedule struct {
	MessageID string `json:"authorizing_message"`
	Spec      Spec   `json:"spec"`
	State     string `json:"state"`
	Next      int64  `json:"next_occurrence"`
	Admitted  int64  `json:"admitted"`
	Skipped   int64  `json:"skipped"`
	LastTask  string `json:"last_task,omitempty"`
	Error     string `json:"error,omitempty"`
}

func New(spec Spec) (Schedule, error) {
	if spec.CatchUpLimit == 0 {
		spec.CatchUpLimit = DefaultCatchUp
	}
	if spec.Template.Priority == 0 {
		spec.Template.Priority = 10
	}
	if spec.Template.Mode == "" {
		spec.Template.Mode = "Local"
	}
	spec.Anchor = spec.Anchor.UTC()
	if !spec.End.IsZero() {
		spec.End = spec.End.UTC()
	}
	s := Schedule{Spec: spec, State: "active"}
	return s, s.Validate()
}
func (s Schedule) Validate() error {
	p := s.Spec
	if !identifier.MatchString(p.ID) || p.Kind != "cron" || p.Anchor.IsZero() || p.Anchor.Unix() < 0 || p.Anchor.Unix() > maxUnix || p.IntervalSeconds < 1 || p.IntervalSeconds > 31536000 || p.CatchUpLimit < 1 || p.CatchUpLimit > MaxCatchUp || p.Template.WorkflowID == "" || len(p.Template.WorkflowID) > 255 || len(p.Template.RunID) > 255 || len(p.Template.Tree) > 128 || p.Template.Revision < 1 || p.Template.TargetRevision < 0 || (p.Template.TargetConfig == "" && p.Template.TargetRevision != 0) || (p.Template.TargetConfig != "" && p.Template.TargetRevision < 1) || p.Template.Priority < 10 || p.Template.Mode != "Local" || len(p.Template.Content) > 32768 || len(p.Template.Title) > 4096 {
		return errors.New("invalid cron identity, anchor, interval, bound, or pinned template")
	}
	if !p.End.IsZero() && (!p.End.After(p.Anchor) || p.End.Unix() > maxUnix) {
		return errors.New("cron end must be after anchor and within RFC3339 range")
	}
	if s.Next < 0 || s.Admitted < 0 || s.Skipped < 0 || s.Admitted > s.Next || s.Skipped != s.Next-s.Admitted {
		return errors.New("invalid cron cursor accounting")
	}
	switch s.State {
	case "active", "paused", "retired", "completed":
	default:
		return errors.New("invalid cron lifecycle")
	}
	return nil
}
func (s Schedule) At(index int64) (time.Time, bool) {
	if index < 0 || index > (maxUnix-s.Spec.Anchor.Unix())/s.Spec.IntervalSeconds {
		return time.Time{}, false
	}
	return time.Unix(s.Spec.Anchor.Unix()+index*s.Spec.IntervalSeconds, int64(s.Spec.Anchor.Nanosecond())).UTC(), true
}
func (s Schedule) latest(now time.Time) (int64, bool) {
	if now.Before(s.Spec.Anchor) {
		return 0, false
	}
	seconds := now.Unix() - s.Spec.Anchor.Unix()
	if now.Nanosecond() < s.Spec.Anchor.Nanosecond() {
		seconds--
	}
	return seconds / s.Spec.IntervalSeconds, true
}

// Due returns a bounded suffix of missed occurrences. Older occurrences are
// intentionally skipped and must be durably audited before these are admitted.
func (s Schedule) Due(now time.Time) (skip int64, indices []int64) {
	next, valid := s.At(s.Next)
	withinEnd := s.Spec.End.IsZero() || valid && next.Before(s.Spec.End)
	if !CanAdmit(s.State == "active", valid && !now.Before(next), withinEnd, s.Spec.Template.Revision > 0) {
		return 0, nil
	}
	if !s.Spec.End.IsZero() && !now.Before(s.Spec.End) {
		now = s.Spec.End.Add(-time.Nanosecond)
	}
	last, ok := s.latest(now)
	if !ok || last < s.Next {
		return 0, nil
	}
	start := s.Next
	if last-start+1 > int64(s.Spec.CatchUpLimit) {
		start = last - int64(s.Spec.CatchUpLimit) + 1
		skip = start - s.Next
	}
	for i := start; i <= last; i++ {
		indices = append(indices, i)
	}
	return
}
func (s *Schedule) Bound() {
	if s.State != "active" {
		return
	}
	next, ok := s.At(s.Next)
	if !ok || !s.Spec.End.IsZero() && !next.Before(s.Spec.End) {
		s.State = "completed"
	}
}
func (s Schedule) TaskID(index int64) string {
	sum := sha256.Sum256([]byte(s.Spec.ID))
	return fmt.Sprintf("cron-%x-%d", sum[:12], index)
}
