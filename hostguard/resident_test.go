package hostguard

import "testing"

func TestResidentTurnDoesNotReserveAnotherPane(t *testing.T) {
	c := DefaultConfig()
	s := Sample{Memory: Reading{float64(c.Soft), true}, Swap: Reading{1, true}, LivePanes: c.PaneLimit}
	if d := Admit(s, c, 0, true); d.Allowed || d.Reason != "PaneLimit" {
		t.Fatal(d)
	}
	if d := AdmitResidentTurn(s, c, true); !d.Allowed {
		t.Fatal(d)
	}
	if d := AdmitResidentTurn(s, c, false); d.Allowed || d.Reason != "LeaseLedger" {
		t.Fatal(d)
	}
	s.LoadPerCore = Reading{c.Load + 1, true}
	if d := AdmitResidentTurn(s, c, true); d.Allowed || d.Reason != "HostPressure" {
		t.Fatal(d)
	}
	s.Memory = Reading{float64(c.Hard - 1), true}
	if d := AdmitResidentTurn(s, c, true); d.Allowed || !d.Hard {
		t.Fatal(d)
	}
}
