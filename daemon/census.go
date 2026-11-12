package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

// CensusProposal records operator judgment; semantic acceptance coverage is never inferred.
type CensusProposal struct {
	ID       string `json:"id"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"`
}
type CensusParams struct {
	IncludeBlocked bool             `json:"include_blocked,omitempty"`
	Proposals      []CensusProposal `json:"proposals,omitempty"`
}
type CensusRow struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	Version         int64  `json:"version"`
	SnapshotDigest  string `json:"snapshot_digest"`
	BranchTip       string `json:"branch_tip"`
	BranchError     string `json:"branch_error,omitempty"`
	LandingEvidence string `json:"landing_evidence"`
	Classification  string `json:"classification"`
	Reason          string `json:"reason"`
	Evidence        string `json:"evidence"`
}
type CensusArtifact struct {
	Format   int         `json:"format"`
	Instance string      `json:"instance"`
	Rows     []CensusRow `json:"rows"`
	Digest   string      `json:"digest"`
}
type CensusApplyParams struct {
	Artifact           CensusArtifact `json:"artifact"`
	RowID              string         `json:"row_id"`
	AuthorizingMessage string         `json:"authorizing_message"`
}

func CensusDigest(a CensusArtifact) string {
	a.Digest = ""
	raw, _ := json.Marshal(a)
	return controlplane.Digest(raw)
}
func ValidateCensusArtifact(a CensusArtifact) error {
	if a.Format != 1 || a.Instance == "" || len(a.Rows) > 1000 || a.Digest != CensusDigest(a) {
		return errors.New("invalid census artifact format, bound or digest")
	}
	seen := map[string]bool{}
	for _, row := range a.Rows {
		if len(row.Evidence) > 4096 || len(row.Reason) > 4096 {
			return errors.New("census evidence or reason exceeds bound")
		}
		if row.ID == "" || seen[row.ID] || row.Version < 1 || len(row.SnapshotDigest) != 64 {
			return errors.New("invalid or duplicate census row")
		}
		seen[row.ID] = true
		if row.State != "Escalated" && row.State != "Blocked" {
			return errors.New("invalid census task state")
		}
		if row.Classification == "live" {
			if row.Reason != "" {
				return errors.New("live census row has retirement reason")
			}
			continue
		}
		class, _, err := engine.ParseRetirement(row.Reason)
		if err != nil || class != row.Classification || strings.TrimSpace(row.Evidence) == "" {
			return errors.New("invalid census classification or evidence")
		}
	}
	return nil
}
func (d *Daemon) census(ctx context.Context, raw json.RawMessage) (any, error) {
	var p CensusParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	proposals := map[string]CensusProposal{}
	for _, v := range p.Proposals {
		if v.ID == "" || proposals[v.ID].ID != "" {
			return nil, errors.New("duplicate or empty census proposal")
		}
		proposals[v.ID] = v
	}
	// The drive's tasks as last published: a census never waits for d.mu
	// (FAE-ROB-020). Apply re-checks each row's version and digest.
	published, err := d.publishedTasks()
	if err != nil {
		return nil, err
	}
	jobs := published.driveJobs
	a := CensusArtifact{Format: 1, Instance: d.config.Instance, Rows: []CensusRow{}}
	ids := []string{}
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := jobs[id]
		if s.State != "Escalated" && !(p.IncludeBlocked && s.State == "Blocked") {
			continue
		}
		var e envelope
		if err := decodeEnvelope(s.Payload, &e); err != nil {
			return nil, err
		}
		row := CensusRow{ID: id, State: s.State, Version: s.Version, SnapshotDigest: controlplane.Digest(s.Payload), Classification: "live"}
		if d.live != nil {
			w := d.live.manager.Binding(e.Admission.RunID, id)
			tip, err := effectGit(ctx, w, "rev-parse", "HEAD")
			if err != nil {
				row.BranchError = err.Error()
			} else {
				row.BranchTip = tip
			}
		}
		row.LandingEvidence = ownLandingSHA(e)
		proposal, has := proposals[id]
		if has {
			delete(proposals, id)
			if strings.TrimSpace(proposal.Evidence) == "" {
				return nil, fmt.Errorf("%s requires explicit coverage or ruling evidence", id)
			}
			if err := d.validateCensusReason(ctx, published, e, proposal.Reason); err != nil {
				return nil, err
			}
			class, _, _ := engine.ParseRetirement(proposal.Reason)
			row.Classification, row.Reason, row.Evidence = class, proposal.Reason, proposal.Evidence
		} else if row.LandingEvidence != "" && d.validateRetirement(ctx, e, "contained "+row.LandingEvidence) == nil {
			row.Classification, row.Reason, row.Evidence = "contained", "contained "+row.LandingEvidence, "task-owned validated landing artifact"
		}
		a.Rows = append(a.Rows, row)
	}
	if len(proposals) > 0 {
		return nil, errors.New("proposal does not identify an included Escalated or Blocked task")
	}
	a.Digest = CensusDigest(a)
	if err := ValidateCensusArtifact(a); err != nil {
		return nil, err
	}
	return a, nil
}

// Apply one reviewed row per RPC. The CLI paces requests without blocking the drive.
func (d *Daemon) censusApply(ctx context.Context, raw json.RawMessage) (any, error) {
	var p CensusApplyParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if err := ValidateCensusArtifact(p.Artifact); err != nil {
		return nil, err
	}
	if p.Artifact.Instance != d.config.Instance {
		return nil, errors.New("census instance mismatch")
	}
	for _, row := range p.Artifact.Rows {
		if row.ID == p.RowID {
			if row.Classification == "live" {
				return nil, errors.New("live census row cannot retire")
			}
			params := TaskControlParams{ID: row.ID, Reason: row.Reason, AuthorizingMessage: p.AuthorizingMessage, ExpectedVersion: row.Version, ExpectedDigest: row.SnapshotDigest, CensusDigest: p.Artifact.Digest, CensusEvidence: row.Evidence}
			b, _ := json.Marshal(params)
			return d.handleDirect(ctx, api.Request{Version: api.Version, Method: "Retire", Params: b})
		}
	}
	return nil, errors.New("selected census row missing")
}

// Read classification uses the captured snapshot; apply validates the current graph.
func (d *Daemon) validateCensusReason(ctx context.Context, published statusSnapshot, e envelope, reason string) error {
	class, reference, err := engine.ParseRetirement(reason)
	if err != nil {
		return err
	}
	if class == "superseded-by" {
		if _, ok := published.driveJobs[reference]; !ok {
			if _, stored, err := published.publishedJob(ctx, d, reference); err != nil {
				return err
			} else if stored {
				return nil
			}
			return errors.New("successor must identify another existing task")
		}
		if reference == e.Job.TaskID {
			return errors.New("successor must identify another existing task")
		}
		return nil
	}
	return d.validateRetirement(ctx, e, reason)
}
