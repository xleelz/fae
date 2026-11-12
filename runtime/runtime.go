// Package runtime contains provider-neutral asynchronous turn contracts.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/domain"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Turn struct{ ID, Task, Role, Worktree, Brief string }
type Outcome struct {
	TurnID string
	Output json.RawMessage
	Class  string
	Err    error
}
type Adapter interface {
	Dispatch(context.Context, Turn) (<-chan Outcome, error)
}
type Script struct {
	ProviderStats *domain.ProviderSessionStats
	Output        json.RawMessage
	Delay         time.Duration
	Class         string
}

// SessionAdapter is the FAE-RT-001 runtime adapter contract that the scripted,
// direct and multiplexer adapters share (one conformance suite):
//   - Run launches a session for the launch's binding in its worktree with its
//     tool profile and environment policy, delivers the brief, observes
//     readiness, activity (stall) and completion, and collects the handoff
//     file (never a terminal scrape), a bounded output tail and the session's
//     own log directory; the session's whole process tree is retired before
//     Run returns (Collected.ProcessesRetired).
//   - Live lists live sessions with their owners and readiness.
//   - Retire stops one live session and its whole process tree.
type SessionAdapter interface {
	Run(context.Context, Launch) (Collected, error)
	Live() []LiveSession
	Retire(context.Context, string) error
}

// LiveSession is one live session as Live reports it.
type LiveSession struct {
	ID      string    `json:"id"`
	Task    string    `json:"task"`
	Role    string    `json:"role"`
	Owner   string    `json:"owner"`
	PID     int       `json:"pid,omitempty"`
	Ready   bool      `json:"ready"`
	Started time.Time `json:"started"`
}

// ErrUnknownSession is a Retire of a session that is not live.
var ErrUnknownSession = errors.New("session is not live")

type liveSession struct {
	pid        int
	task, role string
	started    time.Time
	ready      bool
	cancel     context.CancelFunc
	done       chan struct{}
}

func listLive(live map[string]*liveSession, owner string) []LiveSession {
	out := make([]LiveSession, 0, len(live))
	for id, s := range live {
		out = append(out, LiveSession{ID: id, Task: s.task, Role: s.role, Owner: owner, PID: s.pid, Ready: s.ready, Started: s.started})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// retireLive cancels a live session's Run and waits until Run has retired it.
func retireLive(ctx context.Context, s *liveSession) error {
	if s == nil {
		return ErrUnknownSession
	}
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Scripted is the scripted adapter: each role's queued Script is a turn's
// handoff. Dispatch serves engine turns; Run, Live and Retire implement the
// shared SessionAdapter contract (FAE-RT-001) with Root holding session log
// directories and Owner naming the instance.
type Scripted struct {
	mu        sync.Mutex
	Scripts   map[string][]Script
	Root      string
	Owner     string
	TailBytes int
	seen      map[string]bool
	live      map[string]*liveSession
}

func (s *Scripted) Live() []LiveSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return listLive(s.live, s.Owner)
}
func (s *Scripted) Retire(ctx context.Context, session string) error {
	s.mu.Lock()
	l := s.live[session]
	s.mu.Unlock()
	return retireLive(ctx, l)
}

// Run is the scripted session: identity and mode are required, a session
// identity runs once, the script's class is the turn's typed failure and its
// output must be a JSON handoff.
func (s *Scripted) Run(ctx context.Context, l Launch) (result Collected, err error) {
	result.ProcessesRetired = true
	fail := func(class string, e error) (Collected, error) {
		return result, &TurnError{class, "", l.Role, l.DispatchID, e}
	}
	if l.DispatchID == "" || l.SessionID == "" || l.TaskID == "" || l.Role == "" || (l.Mode != "local" && l.Mode != "normal") {
		return fail(LaunchRefused, errors.New("complete identity and mode required"))
	}
	s.mu.Lock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.live == nil {
		s.live = map[string]*liveSession{}
	}
	if s.seen[l.SessionID] {
		s.mu.Unlock()
		return fail("uncertain-dispatch", errors.New("session already launched"))
	}
	s.seen[l.SessionID] = true
	queue := s.Scripts[l.Role]
	if len(queue) == 0 {
		s.mu.Unlock()
		result.NonExecution = true
		return fail("startup-not-executed", fmt.Errorf("no scripted output for %s", l.Role))
	}
	step := queue[0]
	result.ProviderStats = step.ProviderStats
	s.Scripts[l.Role] = queue[1:]
	dir, e := os.MkdirTemp(s.Root, "session-")
	if e != nil {
		s.mu.Unlock()
		result.NonExecution = true
		return fail(LaunchRefused, e)
	}
	result.LogDirectory = dir
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	session := &liveSession{task: l.TaskID, role: l.Role, started: time.Now(), ready: true, cancel: cancel, done: make(chan struct{})}
	s.live[l.SessionID] = session
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.live, l.SessionID)
		s.mu.Unlock()
		close(session.done)
	}()
	tail := "scripted " + l.Role + " turn " + l.DispatchID
	if s.TailBytes > 0 && len(tail) > s.TailBytes {
		tail = tail[len(tail)-s.TailBytes:]
	}
	result.Tail = tail
	timer := time.NewTimer(step.Delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fail("interrupted", ctx.Err())
	case <-timer.C:
	}
	if step.Class != "" {
		return fail(step.Class, errors.New("scripted failure"))
	}
	if !json.Valid(step.Output) {
		return fail("malformed-output", errors.New("handoff is not valid JSON"))
	}
	result.Output, result.ProducedAt = append(json.RawMessage(nil), step.Output...), time.Now()
	return result, nil
}

func (s *Scripted) Dispatch(ctx context.Context, t Turn) (<-chan Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" || t.Task == "" || t.Role == "" {
		return nil, errors.New("turn identity required")
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[t.ID] {
		return nil, errors.New("uncertain dispatch: turn already dispatched")
	}
	queue := s.Scripts[t.Role]
	if len(queue) == 0 {
		return nil, fmt.Errorf("no scripted output for %s", t.Role)
	}
	step := queue[0]
	step.Output = append(json.RawMessage(nil), step.Output...)
	s.Scripts[t.Role] = queue[1:]
	s.seen[t.ID] = true
	ch := make(chan Outcome, 1)
	go func() {
		defer close(ch)
		timer := time.NewTimer(step.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			ch <- Outcome{TurnID: t.ID, Class: "interrupted", Err: ctx.Err()}
		case <-timer.C:
			o := Outcome{TurnID: t.ID, Output: step.Output, Class: step.Class}
			if !json.Valid(o.Output) {
				o.Class = "malformed-output"
				o.Err = errors.New("handoff is not valid JSON")
			}
			ch <- o
		}
	}()
	return ch, nil
}

type EnvironmentDelta struct{ Removed, Set []string }

// Sanitize removes instance selection and permit variables; explicit values win.
func Sanitize(environ []string, explicit map[string]string, prefixes []string) ([]string, EnvironmentDelta, error) {
	values := map[string]string{}
	delta := EnvironmentDelta{}
	prefixes = append([]string{"FAE_"}, prefixes...)
	for _, prefix := range prefixes {
		if prefix == "" {
			return nil, delta, errors.New("empty runtime prefix")
		}
	}
	removed := map[string]bool{}
	for _, item := range environ {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" || strings.ContainsRune(item, 0) {
			return nil, delta, errors.New("invalid environment entry")
		}
		drop := false
		for _, p := range prefixes {
			if p == "" {
				return nil, delta, errors.New("empty runtime prefix")
			}
			if strings.HasPrefix(k, p) {
				drop = true
			}
		}
		if drop {
			removed[k] = true
		} else {
			values[k] = v
		}
	}
	for k, v := range explicit {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return nil, delta, fmt.Errorf("invalid explicit environment key %q", k)
		}
		values[k] = v
		delta.Set = append(delta.Set, k)
	}
	for k := range removed {
		delta.Removed = append(delta.Removed, k)
	}
	sort.Strings(delta.Removed)
	sort.Strings(delta.Set)
	result := make([]string, 0, len(values))
	for k, v := range values {
		result = append(result, k+"="+v)
	}
	sort.Strings(result)
	return result, delta, nil
}

// CheckCommand authorizes an exact vector. It never invokes a shell or interprets arguments.
func CheckCommand(args []string, allowed [][]string) error {
	if len(args) == 0 {
		return errors.New("empty command")
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return errors.New("NUL argument")
		}
	}
	for _, shape := range allowed {
		if len(args) != len(shape) {
			continue
		}
		match := true
		for i := range args {
			if args[i] != shape[i] {
				match = false
				break
			}
		}
		if match {
			return nil
		}
	}
	return errors.New("command shape not allowed")
}
