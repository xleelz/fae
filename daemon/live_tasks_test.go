package daemon

import "testing"

func TestLiveTaskOverridesArePinnedAndIsolated(t *testing.T) {
	a := LiveTaskConfig{Files: []string{"a.go", "a_test.go"}, TestFile: "a_test.go", TestID: "TestA", ExpectedFailure: "a is zero"}
	b := LiveTaskConfig{Files: []string{"b.go", "b_test.go"}, TestFile: "b_test.go", TestID: "TestB", ExpectedFailure: "b is zero"}
	tasks := map[string]LiveTaskConfig{"a": a, "b": b}
	if err := validateLiveTasks(tasks); err != nil {
		t.Fatal(err)
	}
	x := &liveExecutor{config: LiveConfig{Tasks: tasks, TestFile: "default_test.go"}}
	first, second := x.forTask("a"), x.forTask("b")
	if first.config.TestID != "TestA" || second.config.TestID != "TestB" || x.config.TestFile != "default_test.go" || x.forTask("unknown") != x {
		t.Fatal("scope leaked")
	}
	first.config.Files[0] = "changed.go"
	if tasks["a"].Files[0] != "a.go" {
		t.Fatal("mutated pinned map")
	}
	for _, name := range []string{"../outside", "/absolute", ".git/config", ".", ""} {
		bad := a
		bad.Files = []string{name, "a_test.go"}
		if validateLiveTasks(map[string]LiveTaskConfig{"a": bad}) == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, bad := range []LiveTaskConfig{{Files: []string{"a.go"}, TestFile: "a_test.go", TestID: "TestA", ExpectedFailure: "a"}, {Files: a.Files, TestFile: a.TestFile, TestID: a.TestID}} {
		if validateLiveTasks(map[string]LiveTaskConfig{"a": bad}) == nil {
			t.Fatal("incomplete RED binding accepted")
		}
	}
}
