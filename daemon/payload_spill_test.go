package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// FAE-CP-012 acceptance: a 1 MiB remediation set (blocking spec-review
// findings bound into the spec's rework) goes through the drive: the row holds
// a content reference, the artifact round-trips, and the drive continues.
func TestMegabyteRemediationThroughDrive(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	findings := []map[string]string{}
	for i := 0; i < 64; i++ {
		findings = append(findings, map[string]string{"finding": strings.Repeat("remediate this exact clause; ", 600), "severity": "blocking"})
	}
	reject, _ := json.Marshal(map[string]any{"approved": false, "findings": findings})
	approve := script["spec-review"][0].Reviews[1]
	script["spec-review"] = []engine.ScriptStep{{Reviews: []json.RawMessage{reject, approve}}, {Reviews: []json.RawMessage{approve, approve}}}
	script["spec"] = append(script["spec"], script["spec"][0])
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "remediation", Tree: "tree", AuthorizingMessage: "message"}, "remediation"); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitState(t, d, "remediation", "Done")
	var e envelope
	if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	if e.Job.Counts["spec"] != 2 {
		t.Fatalf("spec visited %d times; the remediation was not reworked", e.Job.Counts["spec"])
	}
	large := ""
	for _, a := range e.Job.Evidence {
		raw, _ := json.Marshal(a.Fields)
		if a.Stage == "spec-review" && len(raw) > 1<<20 {
			large = a.ID
		}
	}
	if large == "" {
		t.Fatal("no 1 MiB remediation artifact recorded")
	}
	content, err := d.store.ArtifactContent(context.Background(), "remediation", large)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) < 1<<20 || controlplane.Digest(content) != large {
		t.Fatalf("artifact did not round-trip: %d bytes", len(content))
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM artifacts WHERE task_id='remediation' AND artifact_id='`+large+`' AND content_digest='`+large+`'`); n != 1 {
		t.Fatal("artifact row holds no content reference", n)
	}
}
