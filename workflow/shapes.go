package workflow

import (
	"path"
	"strings"
)

// TestShapes are the declared test-shape forms (FAE-WF-036): a unit test, an
// integration-target test, a dedicated worktree script and a declared runner.
var TestShapes = []string{"go-unit", "go-integration", "script", "runner", "rust-unit", "python-pytest", "node-test"}

// testShapeValid checks a stage's declared shape against its command form.
func testShapeValid(s Stage) bool {
	if s.Primitive != HostCommand || !s.TestRun || !contains(TestShapes, s.TestShape) || len(s.Command) == 0 {
		return false
	}
	program := s.Command[0]
	switch s.TestShape {
	case "go-unit", "go-integration":
		return len(s.Command) > 2 && path.Base(program) == "go" && s.Command[1] == "test"
	case "rust-unit":
		return len(s.Command) > 1 && path.Base(program) == "cargo" && s.Command[1] == "test"
	case "python-pytest":
		return path.Base(program) == "pytest" || (path.Base(program) == "python" || path.Base(program) == "python3") && len(s.Command) > 2 && s.Command[1] == "-m" && s.Command[2] == "pytest"
	case "node-test":
		return path.Base(program) == "node" && len(s.Command) > 1 && s.Command[1] == "--test"
	case "script":
		rest, local := strings.CutPrefix(program, "./")
		return local && CheckArg("relative-path", rest) == nil
	}
	return path.Base(program) != "go" && !strings.Contains(program, "/")
}
