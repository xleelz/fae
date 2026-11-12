package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workspace"
)

// DesignDocParams attaches a design-document reference (an http(s) URL or a
// path inside the task target's containment root) to a task or backlog item.
type DesignDocParams struct {
	Task               string `json:"task,omitempty"`
	Item               string `json:"item,omitempty"`
	Reference          string `json:"reference"`
	AuthorizingMessage string `json:"authorizing_message"`
}

// attachDesignDoc appends the reference as hashed operator context, so every
// later brief (never the turn in flight) binds it, like resume-with-context.
func (d *Daemon) attachDesignDoc(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before attaching a design document")
	}
	var p DesignDocParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	id := p.Task + p.Item
	if (p.Task == "") == (p.Item == "") {
		return nil, errors.New("attach-design-doc requires exactly one of task or item")
	}
	if p.Reference == "" || len(p.Reference) > 4096 || strings.IndexFunc(p.Reference, unicode.IsControl) >= 0 {
		return nil, errors.New("reference must contain 1..4096 bytes without control characters")
	}
	s, ok, lookupErr := d.storedJob(ctx, id)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !ok {
		return nil, errors.New("unknown task or item")
	}
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	if !engine.CanChangeTask(e.Job.State.Terminal(), e.Job.InFlight != nil, true) {
		return nil, errors.New("design documents cannot be attached to a terminal task")
	}
	reference, err := d.designReference(ctx, s, e, p.Reference)
	if err != nil {
		return nil, err
	}
	attachment := "design-" + controlplane.Digest([]byte(id + "\x00" + reference))[:24]
	text := "design document " + attachment + ": " + reference
	result := map[string]any{"id": id, "attachment_id": attachment, "reference": reference, "duplicate": false}
	for _, c := range e.Job.ContextEntries {
		if c.Kind == "design-doc" && c.Text == text {
			result["duplicate"], result["version"] = true, s.Version
			return result, nil
		}
	}
	original := s
	e.Job.ContextEntries = append(e.Job.ContextEntries, engine.ContextEntry{Kind: "design-doc", Text: text, SHA256: engine.ContextHash(text)})
	ev := engine.Event{Kind: "design-doc-attached", Stage: string(e.Job.State), Attempt: max(1, e.Job.Attempts), Cause: attachment}
	e.Job.History = append(e.Job.History, ev)
	if s.Payload, err = json.Marshal(e); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(ev)
	s.Events = []controlplane.Event{{Kind: ev.Kind, JobID: s.ID, Payload: payload}}
	saved, err := d.saveOperatorCheckpoint(ctx, audit, original, s)
	if err != nil {
		return nil, err
	}
	d.jobs[id] = saved
	d.notifyEvents()
	d.publishStatus()
	result["version"] = saved.Version
	return result, nil
}

// designReference resolves a path against the task target's containment root
// (its target configuration's workspace, else the instance's bound workspace)
// and refuses escapes, including through symlinks, and missing paths.
func (d *Daemon) designReference(ctx context.Context, s controlplane.JobSnapshot, e envelope, ref string) (string, error) {
	if strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://") {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" || u.User != nil {
			return "", errors.New("design reference URL must be absolute http(s) without credentials")
		}
		return ref, nil
	}
	if strings.Contains(ref, "://") {
		return "", errors.New("design reference must be an http(s) URL or a path inside the containment root")
	}
	root := d.config.Workspace
	if e.Admission.TargetConfig != "" {
		_, raw, err := d.store.GetTarget(ctx, s.TargetID, s.TargetRevision)
		if err != nil {
			return "", err
		}
		target, err := parseTarget(raw)
		if err != nil {
			return "", err
		}
		root = target.Workspace
	}
	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if err := workspace.Contained(root, path); err != nil {
		return "", errors.New("design reference must resolve inside the target containment root: " + err.Error())
	}
	return path, nil
}
