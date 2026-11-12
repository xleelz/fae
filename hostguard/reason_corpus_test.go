package hostguard

import (
	"os"
	"regexp"
	"sort"
	"testing"
	"time"
)

// hostGateReasons is keyed by the exact source literal of every admission and
// hold outcome reason in guard.go admit and hold.go Record. Each case drives
// the gate to that reason; refusals must not be allowed and proceeds must be
// (FAE-HOST-002..-006, -010; FAE-VER-001 firing controls).
var hostGateReasons = map[string]func(t *testing.T) (reason string, allowed bool){
	`"LeaseLedger"`: func(t *testing.T) (string, bool) {
		d := Admit(healthy(), DefaultConfig(), 0, false)
		return d.Reason, d.Allowed
	},
	`"Configuration"`: func(t *testing.T) (string, bool) {
		c := DefaultConfig()
		c.PaneLimit = 0
		d := Admit(healthy(), c, 0, true)
		return d.Reason, d.Allowed
	},
	`"HostHardLimit"`: func(t *testing.T) (string, bool) {
		s := healthy()
		s.Memory.Value = float64(GiB)
		d := Admit(s, DefaultConfig(), 0, true)
		if !d.Hard {
			t.Fatal("hard breaker not hard")
		}
		return d.Reason, d.Allowed
	},
	`"PaneLimit"`: func(t *testing.T) (string, bool) {
		s := healthy()
		s.LivePanes = DefaultConfig().PaneLimit
		d := Admit(s, DefaultConfig(), 0, true)
		return d.Reason, d.Allowed
	},
	`"MemoryReservation"`: func(t *testing.T) (string, bool) {
		d := Admit(healthy(), DefaultConfig(), 30, true)
		return d.Reason, d.Allowed
	},
	`"HostPressure"`: func(t *testing.T) (string, bool) {
		d := Admit(withLoad(DefaultConfig().Load+1), DefaultConfig(), 0, true)
		if d.Reason != "HostPressure" || d.Allowed {
			return d.Reason, d.Allowed
		}
		s := mustHold(t, false).Record(epoch, withLoad(DefaultConfig().Load+1), DefaultConfig())
		return s.Reason, s.Outcome != OutcomeRefused
	},
	`"HostPressureHold"`: func(t *testing.T) (string, bool) {
		h := mustHold(t, false)
		h.Record(epoch, withLoad(DefaultConfig().Load+1), DefaultConfig())
		s := h.Record(epoch.Add(time.Second), calm(), DefaultConfig())
		return s.Reason, s.Outcome != OutcomeWaiting
	},
	`"PressureHoldLifted"`: func(t *testing.T) (string, bool) {
		h := mustHold(t, false)
		h.Record(epoch, withLoad(DefaultConfig().Load+1), DefaultConfig())
		h.Record(epoch.Add(time.Second), calm(), DefaultConfig())
		s := h.Record(epoch.Add(32*time.Second), calm(), DefaultConfig())
		return s.Reason, s.Outcome == OutcomeProceed && s.Lifted
	},
	`"IOPressure"`: func(t *testing.T) (string, bool) {
		s := mustHold(t, true).Record(epoch, withIO(DefaultPolicy().IOGate+1), DefaultConfig())
		return s.Reason, s.Outcome != OutcomeRefused
	},
	`"IOPressureHold"`: func(t *testing.T) (string, bool) {
		s := mustHold(t, true).Record(epoch, calm(), DefaultConfig())
		return s.Reason, s.Outcome != OutcomeWaiting
	},
	`"IOGateClear"`: func(t *testing.T) (string, bool) {
		h := mustHold(t, true)
		h.Record(epoch, calm(), DefaultConfig())
		s := h.Record(epoch.Add(31*time.Second), calm(), DefaultConfig())
		return s.Reason, s.Outcome == OutcomeProceed && s.Lifted
	},
	`"TelemetryGap"`: func(t *testing.T) (string, bool) {
		s := calm()
		s.LoadPerCore.Known = false
		step := mustHold(t, false).Record(epoch, s, DefaultConfig())
		return step.Reason, step.Outcome == OutcomeProceed
	},
}

// proceeds are the reasons that admit; every other reason refuses or holds.
var proceeds = map[string]bool{`"PressureHoldLifted"`: true, `"IOGateClear"`: true, `"TelemetryGap"`: true}

func TestHostGateReasonCorpus(t *testing.T) {
	for key, run := range hostGateReasons {
		reason, allowed := run(t)
		if `"`+reason+`"` != key || allowed != proceeds[key] {
			t.Errorf("%s: got reason %q allowed %v", key, reason, allowed)
		}
	}
	if d := Admit(healthy(), DefaultConfig(), 0, true); !d.Allowed || d.Reason != "" {
		t.Fatal("healthy host refused", d)
	}
}

// The census: the corpus keys are exactly the gates' reason literals.
func TestHostGateReasonCensus(t *testing.T) {
	found := map[string]bool{}
	for file, fn := range map[string]string{"guard.go": "func admit(", "hold.go": "func (h *Hold) Record("} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		start := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(fn)).FindStringIndex(body)
		if start == nil {
			t.Fatal("missing", fn)
		}
		body = body[start[0]:]
		body = body[:regexp.MustCompile(`(?m)^}`).FindStringIndex(body)[1]]
		for _, m := range regexp.MustCompile(`"[A-Z][A-Za-z]+"`).FindAllString(body, -1) {
			found[m] = true
		}
	}
	diff := []string{}
	for k := range found {
		if hostGateReasons[k] == nil {
			diff = append(diff, "missing "+k)
		}
	}
	for k := range hostGateReasons {
		if !found[k] {
			diff = append(diff, "stale "+k)
		}
	}
	sort.Strings(diff)
	if len(diff) != 0 {
		t.Fatal(diff)
	}
}
