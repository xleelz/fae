package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// FAE-WF-043: the expected-failure rule the daemon holds a derived RedReport
// to (daemon liveRedReasonVacuous) is stated in the output protocol, which
// the kernel contract of every bridge turn carries, before the turn runs:
// what the fragment is matched against, what it must say, what is refused and
// what a refusal costs. Its refused words are RedReasonTraceWords, the list
// the daemon's rule is held to (daemon TestLiveRedTraceWordsAreTheProtocols).
// Codex and Claude get the same protocol, so the Direct adapter, the
// one-shot herdr-core pane and resident workers, which all run this bridge, send
// it. It also states the RED's shape the gate needs: one top-level test
// without subtests (a RedReport whose test calls t.Run is refused), a
// failure the test reports itself (a test whose binary ends under it does
// not count), and a hang only as a last resort, which fails when go test's
// own -timeout (effects.brokerGoTestAlarm) fires. Its examples are Go's own
// messages.
func TestOutputProtocolStatesRedReasonRule(t *testing.T) {
	rule := []string{
		"The RED is one top-level test function without t.Run subtests: the RED gate needs exactly one executed test, and each subtest counts as one, so a RedReport whose test calls t.Run is refused.",
		"The test must report its own failure (t.Error, t.Fatal or a panic in the test's own goroutine): a test whose test binary ends under it, by os.Exit or log.Fatal, a panic in another goroutine or a signal, reports no result and does not count as executed.",
		"A RED should fail, not hang: a hanging test fails only when go test's own -timeout, half the test stage's deadline, fires",
		"expected_failure is matched literally against that test's own output, without the goroutine stacks in it and without go test's === RUN and --- FAIL: lines",
		"must say why the test fails",
		"(got 0 want 42, expected error)", "(assignment to entry in nil map)", "for a test that hangs, the timeout (test timed out)",
		"Quote the failure itself: text the test logs before it fails would match a failure for any other reason too.",
		"Unless the operator pinned the task's RED identity, a RedReport is refused, costing an attempt",
		"the test's name or part of it, the test file's name, a file:line position or part of one (go:9)",
		"go test's framing (=== RUN, --- FAIL:, a duration, exit status N)",
		"a panic's generic text (runtime error, goroutine N, stack frames and source positions)",
		"these words: " + RedReasonTraceWords + ",",
		"got, want, expected, actual",
		"or numbers alone (a number counts beside a word or a comparison, as in 0 != 42)",
		"Any other word makes it a reason: expected error is one, error alone is not.",
	}
	for _, stale := range []string{"nil map write", "source paths", "up to the goroutine dump", ", error,"} {
		if strings.Contains(outputProtocolRules, stale) {
			t.Errorf("protocol still says %q", stale)
		}
	}
	run := func(name string, fixture func() (Config, string, string)) {
		t.Run(name, func(t *testing.T) {
			c, config, logs := fixture()
			if e := Run(context.Background(), c, config); e != nil {
				t.Fatal(e)
			}
			var contract struct {
				Protocol string `json:"output_protocol"`
			}
			if e := json.Unmarshal(promptBlocks(t, []byte(providerFile(t, logs, "prompt.json")))["kernel_contract"], &contract); e != nil {
				t.Fatal(e)
			}
			for _, part := range rule {
				if !strings.Contains(contract.Protocol, part) {
					t.Errorf("protocol lacks %q", part)
				}
			}
		})
	}
	run("codex", func() (Config, string, string) {
		c, config, l := turnFixture(t, "test-first", nil, acceptedNoEdits)
		return c, config, l.LogDirectory
	})
	run("claude", func() (Config, string, string) {
		c, config, l := claudeFixture(t, "")
		return c, config, l.LogDirectory
	})
}
