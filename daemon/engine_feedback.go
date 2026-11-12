package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
)

// engineFeedbackRecord points from a failed host-command run to its failure
// excerpt, which is stored by reference (FAE-CP-012). Artifact is the content
// id of the run's artifact: a record left by an interrupted attempt of the
// same dispatch names a different artifact and is never shown for this one.
type engineFeedbackRecord struct {
	Artifact   string               `json:"artifact"`
	Stage      string               `json:"stage"`
	DispatchID string               `json:"dispatch_id"`
	Command    []string             `json:"command"`
	ExitCode   int                  `json:"exit_code"`
	Excerpt    controlplane.BlobRef `json:"excerpt"`
	Truncated  bool                 `json:"excerpt_truncated"`
}

// engineFeedbackKey names a host-command dispatch's feedback marker. The
// run's artifact carries the dispatch's session id, so a later turn finds the
// excerpt from its input artifacts alone.
func engineFeedbackKey(session string) string {
	sum := sha256.Sum256([]byte(session))
	return "engine-feedback/" + hex.EncodeToString(sum[:])
}

// engineFeedbackInt reads an integer artifact field, typed as decoded
// (json.Number from the engine, float64 after a plain round trip).
func engineFeedbackInt(fields map[string]any, name string) (int, bool) {
	switch v := fields[name].(type) {
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// engineFeedbackCount reads a numeric count field from a flat or dotted path in fields.
func engineFeedbackCount(fields map[string]any, path string) (int, bool) {
	if n, ok := engineFeedbackInt(fields, path); ok {
		return n, true
	}
	cur := any(fields)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur, ok = m[part]
		if !ok {
			return 0, false
		}
	}
	return engineFeedbackInt(map[string]any{"v": cur}, "v")
}

// hostRunFailed reports whether a host-command artifact records a failed
// run: a nonzero exit, or counts no test gate accepts (nothing executed, or a
// targeted run that did not match exactly one test). A format run fails on
// new debt. The run-time store and the brief builder use this same test.
func hostRunFailed(stage workflow.Stage, fields map[string]any) bool {
	exit, _ := engineFeedbackInt(fields, "exit_code")
	if stage.Formal != nil {
		passed, ok := fields["passed"].(bool)
		return exit != 0 || !ok || !passed
	}
	if stage.Check != nil && stage.Check.CountField != "" {
		count, ok := engineFeedbackCount(fields, stage.Check.CountField)
		return exit != 0 || !ok || count < 1
	}
	if stage.TestRun {
		executed, _ := engineFeedbackInt(fields, "executed_count")
		matched, _ := engineFeedbackInt(fields, "matched_count")
		return exit != 0 || executed < 1 || stage.TestShape != "" && matched != 1
	}
	debt, _ := engineFeedbackInt(fields, "new_debt")
	return exit != 0 || debt > 0
}

// recordEngineFeedback stores a failed run's bounded excerpt (and that of
// each required check folded into it) before the run's result is recorded,
// so every recorded failing run has one. The excerpt blob is written before
// the marker that names it: a crash between the two leaves only an
// unreferenced blob. Every secret the daemon holds is redacted from it
// before it is stored (FAE-SEC-003; redactedExcerpt).
func (x *liveExecutor) recordEngineFeedback(ctx context.Context, d engine.Dispatch, worktree string, argv []string, run effects.RunEvidence, checks []effects.RunEvidence, output any) error {
	raw, err := json.Marshal(output)
	if err != nil {
		return err
	}
	var fields map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err = dec.Decode(&fields); err != nil {
		return err
	}
	if len(d.Sessions) == 0 || !hostRunFailed(d.Config, fields) {
		return nil
	}
	artifact, err := domain.ContentHash(raw)
	if err != nil {
		return err
	}
	roots := []string{worktree}
	if physical, err := filepath.EvalSymlinks(worktree); err == nil {
		roots = append(roots, physical) // the command shim's view of the worktree
	}
	text, truncated := x.redactedExcerpt(argv, run, checks, roots)
	audit, _ := json.Marshal(map[string]any{"task": d.TaskID, "dispatch": d.ID, "stage": d.Stage, "bytes": len(text), "truncated": truncated})
	ref, err := x.daemon.store.PutBlob(ctx, controlplane.Audit{Actor: "daemon", Action: "store engine feedback excerpt", Outcome: "stored", Inputs: audit}, []byte(text))
	if err != nil {
		return err
	}
	record, _ := json.Marshal(engineFeedbackRecord{Artifact: artifact, Stage: d.Stage, DispatchID: d.ID, Command: argv, ExitCode: run.ExitCode, Excerpt: ref, Truncated: truncated})
	return x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "record engine feedback", Outcome: "stored", Inputs: audit}, engineFeedbackKey(d.Sessions[0].ID), record)
}

// redactedExcerpt is effects.FailureExcerpt of the run with every secret the
// daemon holds replaced (FAE-SEC-003): in the run's and checks' output
// before the excerpt is cut, so a cut never leaves part of a secret, and in
// the excerpt again, for a secret the go test -json events had escaped.
// When the secrets cannot be read the excerpt is withheld, with the reason.
func (x *liveExecutor) redactedExcerpt(argv []string, run effects.RunEvidence, checks []effects.RunEvidence, roots []string) (string, bool) {
	secrets, err := x.daemon.failureSecrets(x.config.Environment)
	if err != nil {
		return withheldExcerpt(err), false
	}
	redacted := make([]effects.RunEvidence, len(checks))
	for i, c := range checks {
		redacted[i] = secrets.redactRun(c)
	}
	text, truncated := effects.FailureExcerpt(argv, secrets.redactRun(run), redacted, roots...)
	text, cut := effects.BoundExcerpt(secrets.redactString(text), effects.ExcerptLimit)
	return text, truncated || cut
}

// engineFeedback is the executor's Feedback hook. It returns one entry per
// failed host-command run among the dispatch's input artifacts, in input
// order, with the excerpt stored when the run failed. A run recorded without
// an excerpt (before excerpts were stored) keeps an empty one; a failed store
// read fails the dispatch before launch. A stored excerpt is redacted again
// before it reaches the brief (FAE-SEC-003), which also covers one stored
// before excerpts were redacted; when the secrets cannot be read it is
// withheld, with the reason.
func (x *liveExecutor) engineFeedback(ctx context.Context, d engine.Dispatch, doc workflow.Document) ([]execution.FeedbackRun, error) {
	var runs []execution.FeedbackRun
	var secrets secretValues
	var secretsErr error
	secretsRead := false
	for _, a := range d.Inputs {
		stage, ok := doc.Stages[a.Stage]
		if !ok || stage.Primitive != workflow.HostCommand || !hostRunFailed(stage, a.Fields) {
			continue
		}
		run := execution.FeedbackRun{Stage: a.Stage, Command: stage.Command}
		if i := strings.LastIndex(a.Session, "/session/"); i >= 0 {
			run.DispatchID = a.Session[:i] // a host command's session is "<dispatch>/session/0"
		}
		run.ExitCode, _ = engineFeedbackInt(a.Fields, "exit_code")
		run.ExecutedCount, _ = engineFeedbackInt(a.Fields, "executed_count")
		run.MatchedCount, _ = engineFeedbackInt(a.Fields, "matched_count")
		raw, err := x.daemon.store.GetMarker(ctx, engineFeedbackKey(a.Session))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			var record engineFeedbackRecord
			if err = json.Unmarshal(raw, &record); err != nil {
				return nil, err
			}
			if record.Artifact == a.ID {
				text, err := x.daemon.store.GetBlob(ctx, record.Excerpt)
				if err != nil {
					return nil, err
				}
				if !secretsRead {
					secrets, secretsErr = x.daemon.failureSecrets(x.config.Environment)
					secretsRead = true
				}
				excerpt := secrets.redactString(string(text))
				if secretsErr != nil {
					excerpt = withheldExcerpt(secretsErr)
				}
				run.DispatchID, run.Command, run.Excerpt, run.ExcerptTruncated = record.DispatchID, record.Command, excerpt, record.Truncated
				if _, ok := a.Fields["exit_code"]; !ok {
					run.ExitCode = record.ExitCode // a format artifact records only its debt
				}
			}
		}
		runs = append(runs, run)
	}
	return runs, nil
}
