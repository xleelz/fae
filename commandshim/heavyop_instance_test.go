package commandshim

import (
	"strings"
	"testing"
	"time"
)

// FAE-OBS-001: heavy-op run's lines name HeavyOp.Instance (the CLI's
// --instance) unless an alert spool in an instance's layout names another;
// with neither, "unknown".
func TestHeavyOpLinesNameTheInstance(t *testing.T) {
	for _, c := range []struct {
		h    HeavyOp
		want string
	}{
		{HeavyOp{Instance: "inst-b"}, "inst-b"},
		{HeavyOp{Instance: "inst-b", AlertSpool: "/home/u/.fae/instances/inst-c/run/host-alerts"}, "inst-c"},
		{HeavyOp{Instance: "inst-b", AlertSpool: "/elsewhere/spool"}, "inst-b"},
		{HeavyOp{}, "unknown"},
	} {
		line := HostCommandLogLine(heavyOpInstance(c.h), "heavy-op", "text")
		fields := strings.Fields(line)
		if _, err := time.Parse(time.RFC3339Nano, fields[0]); err != nil || len(fields) != 4 || fields[1] != "instance="+c.want || fields[2] != "component=heavy-op" || fields[3] != "text" {
			t.Errorf("%+v: %q %v", c.h, line, err)
		}
	}
}
