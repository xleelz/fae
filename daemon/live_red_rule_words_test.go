package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/provider"
)

// liveRedStartsSubtests sees Run selected on the test's own *testing.T
// parameter anywhere in its body, and nothing else.
func TestLiveRedStartsSubtests(t *testing.T) {
	const head = "package sample\n\nimport \"testing\"\n\n"
	for source, want := range map[string]bool{
		"func TestX(t *testing.T) { t.Run(\"a\", func(t *testing.T) {}) }":                            true,
		"func TestX(tt *testing.T) { for range 2 { tt.Run(\"a\", func(*testing.T) {}) } }":            true,
		"func TestX(t *testing.T) { run := t.Run; run(\"a\", func(*testing.T) {}) }":                  true,
		"func TestX(t *testing.T) { if false { t.Run(\"a\", nil) } }":                                 true,
		"func TestX(t *testing.T) { t.Fatal(\"got 0 want 42\") }":                                     false,
		"func TestX(t *testing.T) { helper(t) }\nfunc helper(t *testing.T) { t.Run(\"a\", nil) }":     false,
		"func TestY(t *testing.T) { t.Run(\"a\", nil) }\nfunc TestX(t *testing.T) { t.Fatal(\"x\") }": false,
		"func TestX(t *testing.T) { { t := struct{ Run func() }{}; t.Run() } }":                       false,
		"func TestX(_ *testing.T) {}":       false,
		"func TestX(t *testing.T) { t.Run(": false,
	} {
		if got := liveRedStartsSubtests([]byte(head+source), "TestX"); got != want {
			t.Errorf("%s: subtests %v, want %v", source, got, want)
		}
	}
}

// FAE-WF-043/073 (re-audit 9): the words the reason rule refuses are exactly
// the ones the bridge's output protocol lists (provider.RedReasonTraceWords),
// and the protocol says error alone is refused through runtime error, not as
// a word.
func TestLiveRedTraceWordsAreTheProtocols(t *testing.T) {
	var words []string
	for word := range liveRedTraceWords {
		words = append(words, word)
	}
	stated := strings.Split(provider.RedReasonTraceWords, ", ")
	slices.Sort(words)
	slices.Sort(stated)
	if !slices.Equal(words, stated) {
		t.Fatalf("rule refuses %v, protocol states %v", words, stated)
	}
	if !slices.Equal(liveRedTracePhrases, []string{"runtime error"}) {
		t.Fatalf("phrases %v: the protocol states only runtime error", liveRedTracePhrases)
	}
}
