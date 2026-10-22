package daemon

import (
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"testing"
)

func TestFleetProvisionToolReservedAlias(t *testing.T) {
	tool := externalTool("http://localhost:9999")
	slash := tool
	slash.Endpoint += "///"
	other := tool
	other.Endpoint = "http://localhost:9998"
	c := LiveConfig{ExternalTools: map[string]external.Tool{"provision": tool, "duplicate": tool, "trailing-slash": slash, "ordinary": other}, ReviewPolicies: map[string]external.ReviewPolicy{"task": {ProvisionTool: "provision"}}}
	for _, alias := range []string{"provision", "duplicate", "trailing-slash"} {
		if !isFleetProvisionTool(c, alias) {
			t.Fatal(alias)
		}
		var dispatch engine.Dispatch
		dispatch.Config.Subject = alias
		if _, _, e := externalIntent(c, domain.Disposition{Mode: domain.Normal, Live: true}, dispatch); e == nil {
			t.Fatal("reserved effect accepted")
		}
	}
	for _, alias := range []string{"ordinary", "missing"} {
		if isFleetProvisionTool(c, alias) {
			t.Fatal(alias)
		}
	}
}
