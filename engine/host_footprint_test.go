package engine_test

import (
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"testing"
	"time"
)

func TestImpossibleHostFootprintClearsPreviousWorkflowSlotWait(t *testing.T) {
	doc := scenarioLeafDoc()
	job, err := engine.New("host-refused", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	job.Parked = true
	job.Reason = "WorkflowSlot"
	job.SlotWaitReason = "host_capacity"
	job.SlotWaitSince = time.Now().Add(-time.Hour)
	refused, events, err := engine.RefuseHostFootprint(job, "footprint exceeds total host memory")
	if err != nil || refused.State != domain.Escalated || refused.EscalationKind != domain.FootprintExceedsHost || refused.Parked || refused.SlotWaitReason != "" || !refused.SlotWaitSince.IsZero() || len(events) == 0 {
		t.Fatalf("obsolete wait survived host refusal: %+v %v", refused, err)
	}
	if refused.Attempts != job.Attempts || refused.Sequence != job.Sequence {
		t.Fatal("refusal charged work")
	}
}
