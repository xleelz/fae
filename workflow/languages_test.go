package workflow

import "testing"

func TestLanguageTestSourcesAndShapes(t *testing.T) {
	for _, tc := range []struct {
		shape, file string
		command     []string
	}{
		{"rust-unit", "tests/answer.rs", []string{"cargo", "test", "--test", "answer"}},
		{"python-pytest", "tests/test_answer.py", []string{"python3", "-m", "pytest", "tests"}},
		{"node-test", "tests/answer.test.ts", []string{"node", "--test", "tests/answer.test.ts"}},
	} {
		if !TestSourceFile(tc.file, tc.shape) || !TestSourceFile(tc.file, "") {
			t.Fatal(tc)
		}
		if !testShapeValid(Stage{Primitive: HostCommand, TestRun: true, TestShape: tc.shape, Command: tc.command}) {
			t.Fatal("shape rejected", tc)
		}
		if testShapeValid(Stage{Primitive: HostCommand, TestRun: true, TestShape: tc.shape, Command: []string{"go", "test", "./..."}}) {
			t.Fatal("wrong runner accepted", tc)
		}
	}
	for _, file := range []string{"answer.py", "src/lib.rs", "answer.ts", "_test.go", ".hidden_test.go"} {
		if TestSourceFile(file, "") {
			t.Fatal("production/hidden file accepted", file)
		}
	}
	for _, tc := range []struct{ file, id, want string }{
		{"test_answer.py", "tests/test_answer.py::TestAnswer::test_answer[one]", "test_answer"},
		{"tests/answer.rs", "checks::answer", "answer"},
		{"answer.test.ts", "answer", "answer"},
	} {
		if got := TestDeclarationName(tc.file, tc.id); got != tc.want {
			t.Fatal(got, tc)
		}
	}
}
