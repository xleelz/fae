package daemon

import (
	"encoding/json"
	"os"
	"testing"
)

// FAE-CLI-001: redundant scratch archive cleanup runs through the serving
// daemon; preview needs no citation, apply needs an authorizing message.
func TestScratchArchiveCleanupThroughDaemon(t *testing.T) {
	cfg, ctx := fixture(t)
	p, _ := scratchArchiveFixture(t)
	cfg.Root = p.Home
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	params := ScratchArchiveRPCParams{ScratchInstance: p.Instance, Source: p.Source, Survivor: p.Survivor}
	reply, err := call(t, d, "ScratchArchiveCleanup", params, "")
	if err != nil {
		t.Fatal(err)
	}
	var preview ScratchArchiveCleanupResult
	if err = json.Unmarshal(reply.Result, &preview); err != nil || preview.PlanDigest == "" || preview.Completed {
		t.Fatal(string(reply.Result), err)
	}
	params.Apply, params.PlanDigest = true, preview.PlanDigest
	if _, err = call(t, d, "ScratchArchiveCleanup", params, ""); err == nil {
		t.Fatal("uncited apply accepted")
	}
	if _, err = os.Stat(p.Source); err != nil {
		t.Fatal("refused apply removed the source", err)
	}
	if _, err = call(t, d, "ScratchArchiveCleanup", ScratchArchiveRPCParams{ScratchInstance: cfg.Instance, Source: p.Source, Survivor: p.Survivor}, ""); err == nil {
		t.Fatal("serving instance accepted as the scratch instance")
	}
	authorize(t, d)
	params.AuthorizingMessage = "message"
	if reply, err = call(t, d, "ScratchArchiveCleanup", params, ""); err != nil {
		t.Fatal(err)
	}
	var applied ScratchArchiveCleanupResult
	if err = json.Unmarshal(reply.Result, &applied); err != nil || !applied.Completed {
		t.Fatal(string(reply.Result), err)
	}
	if _, err = os.Stat(p.Source); !os.IsNotExist(err) {
		t.Fatal("redundant source survived", err)
	}
	if _, err = os.Stat(p.Survivor); err != nil {
		t.Fatal("canonical archive removed", err)
	}
}
