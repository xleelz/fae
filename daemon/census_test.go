package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"strings"
	"testing"
)

func TestCensusArtifactRefusalControls(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanApplyCensus(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
	a := CensusArtifact{Format: 1, Instance: "test", Rows: []CensusRow{{ID: "t", State: "Escalated", Version: 1, SnapshotDigest: strings.Repeat("a", 64), Classification: "obsolete", Reason: "obsolete ruling", Evidence: "ruling/123"}}}
	a.Digest = CensusDigest(a)
	if err := ValidateCensusArtifact(a); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CensusArtifact){func(a *CensusArtifact) { a.Format = 2 }, func(a *CensusArtifact) { a.Rows = append(a.Rows, a.Rows[0]) }, func(a *CensusArtifact) { a.Rows[0].Evidence = "" }, func(a *CensusArtifact) { a.Rows[0].Classification = "contained" }, func(a *CensusArtifact) { a.Rows[0].State = "Done" }, func(a *CensusArtifact) { a.Rows[0].Version = 0 }, func(a *CensusArtifact) { a.Rows[0].Classification = "live" }} {
		b := a
		b.Rows = append([]CensusRow{}, a.Rows...)
		change(&b)
		b.Digest = CensusDigest(b)
		if ValidateCensusArtifact(b) == nil {
			t.Fatal("invalid artifact accepted", b)
		}
	}
	a.Digest = "bad"
	if ValidateCensusArtifact(a) == nil {
		t.Fatal("invalid checksum")
	}
}
func TestCensusNativeTenAuditedRowsAndStaleRefusal(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	items := []BacklogItemParams{}
	proposals := []CensusProposal{}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("census-%02d", i)
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}})
		proposals = append(proposals, CensusProposal{ID: id, Reason: "obsolete ruling/123", Evidence: "operator ruling/123"})
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range proposals {
		if _, err = call(t, d, "Escalate", TaskControlParams{ID: v.ID, Reason: "review", AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	get := func(p CensusParams) CensusArtifact {
		t.Helper()
		v, e := call(t, d, "Census", p, "")
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(v.Result)
		var a CensusArtifact
		if e = json.Unmarshal(raw, &a); e != nil {
			t.Fatal(e)
		}
		return a
	}
	live := get(CensusParams{})
	for _, r := range live.Rows {
		if r.Classification != "live" || r.LandingEvidence != "" {
			t.Fatal("invented containment", r)
		}
	}
	a := get(CensusParams{Proposals: proposals})
	if len(a.Rows) != 10 {
		t.Fatal(a)
	}
	bad := a
	bad.Rows = append([]CensusRow{}, a.Rows...)
	bad.Rows[0].Version++
	bad.Digest = CensusDigest(bad)
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: bad, RowID: bad.Rows[0].ID, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("stale accepted")
	}
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: a, RowID: "missing", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("missing accepted")
	}

	bad = a
	bad.Rows = append([]CensusRow{}, a.Rows...)
	bad.Rows[0].SnapshotDigest = strings.Repeat("f", 64)
	bad.Digest = CensusDigest(bad)
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: bad, RowID: bad.Rows[0].ID, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("changed payload accepted")
	}
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: a, RowID: a.Rows[0].ID}, ""); err == nil {
		t.Fatal("missing authority accepted")
	}

	invalid := a
	invalid.Rows = append([]CensusRow{}, a.Rows...)
	invalid.Rows[0].Reason = "superseded-by " + invalid.Rows[0].ID
	invalid.Rows[0].Classification = "superseded-by"
	invalid.Digest = CensusDigest(invalid)
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: invalid, RowID: invalid.Rows[0].ID, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("self supersession accepted")
	}
	if _, err = call(t, d, "Census", CensusParams{Proposals: []CensusProposal{{ID: "missing", Reason: "obsolete ruling", Evidence: "ruling"}}}, ""); err == nil {
		t.Fatal("missing proposal accepted")
	}
	for _, r := range a.Rows {
		if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: a, RowID: r.ID, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect census audits", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE action='Retire' AND message_id='message'`).Scan(&count)
	})
	if err != nil || count != 10 {
		t.Fatalf("audits=%d err=%v", count, err)
	}

	for _, r := range a.Rows {
		if d.jobs[r.ID].State != "Retired" || d.jobs[r.ID].Version != r.Version+1 {
			t.Fatal("not exactly once", r.ID)
		}
	}
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: a, RowID: a.Rows[0].ID, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("replay accepted")
	}
	// A later row cannot introduce a supersession cycle through an earlier retirement.
	cycleItems := []BacklogItemParams{}
	for _, id := range []string{"cycle-a", "cycle-b"} {
		cycleItems = append(cycleItems, BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "same acceptance"}})
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: cycleItems, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cycle-a", "cycle-b"} {
		if _, err = call(t, d, "Escalate", TaskControlParams{ID: id, Reason: "review", AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	cycle := get(CensusParams{Proposals: []CensusProposal{{ID: "cycle-a", Reason: "superseded-by cycle-b", Evidence: "operator says acceptance covered"}, {ID: "cycle-b", Reason: "superseded-by cycle-a", Evidence: "operator says acceptance covered"}}})
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: cycle, RowID: "cycle-a", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "CensusApply", CensusApplyParams{Artifact: cycle, RowID: "cycle-b", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("supersession cycle accepted")
	}
	if d.jobs["cycle-b"].State != "Escalated" {
		t.Fatal("cycle failure mutated second row")
	}

}

func TestCensusReadSnapshotIndependent(t *testing.T) {
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{"t": {ID: "t", State: "Escalated", Version: 1}}}
	jobs := d.censusSnapshots()
	if !d.mu.TryLock() {
		t.Fatal("census snapshot retained writer mutex")
	}
	d.jobs["t"] = controlplane.JobSnapshot{ID: "t", State: "Done", Version: 2}
	d.mu.Unlock()
	if jobs["t"].Version != 1 || jobs["t"].State != "Escalated" {
		t.Fatal("mutable census view")
	}
}
