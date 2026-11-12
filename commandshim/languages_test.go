package commandshim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLanguageWholeSuiteAndSelectedWeights(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want int
	}{
		{[]string{"cargo", "test", "--tests"}, 2},
		{[]string{"cargo", "test", "answer", "--", "--exact"}, 1},
		{[]string{"pytest", "tests"}, 2},
		{[]string{"pytest", "tests/test_answer.py::test_answer"}, 1},
		{[]string{"node", "--test", "answer.test.ts"}, 2},
		{[]string{"node", "--test", "--test-name-pattern=^answer$", "answer.test.ts"}, 1},
	} {
		if got := ShimWeight(tc.argv); got != tc.want {
			t.Fatal(tc.argv, got, tc.want)
		}
	}
}

func TestAccountedLanguagePreparationRetainsPhysicalScope(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "test_answer.py")
	if err := os.WriteFile(file, []byte("def test_answer(): pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := Policy{Worktree: dir, CWD: dir}
	if _, err := p.PrepareAccounted([]string{"pytest", "test_answer.py"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareAccounted([]string{"pytest", "../outside.py"}, nil); err == nil {
		t.Fatal("outside target admitted")
	}
}
