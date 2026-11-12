package commandshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func pythonPolicy(t *testing.T) Policy {
	t.Helper()
	root := t.TempDir()
	return Policy{Worktree: root, CWD: root, CodeRole: true, LeaseHeld: true}
}
func TestPythonScopedPolicyAndParallelism(t *testing.T) {
	p := pythonPolicy(t)
	for _, argv := range [][]string{{"pytest", "-q", "tests/test_x.py::TestX::test_one[case]"}, {"python3", "-m", "pytest", "tests"}, {"python3", "-B", "-m", "compileall", "pkg"}, {"python", "-m", "py_compile", "pkg/a.py"}, {"ruff", "check", "pkg"}, {"ruff", "format", "--check", "pkg"}} {
		first, err := PreparePython(p, argv)
		if err != nil {
			t.Fatal(argv, err)
		}
		second, err := PreparePython(p, first)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatal("policy not idempotent", argv, first, second, err)
		}
	}
	for _, argv := range [][]string{{"python3", "-c", "print(1)"}, {"python3", "script.py"}, {"python3", "-m", "pip", "install", "x"}, {"pytest", "-n4", "tests"}, {"pytest", "--junitxml=/tmp/out.xml", "tests"}, {"pytest", "-k", "name", "tests"}, {"pytest", "-p", "plugin", "tests"}, {"pytest", "../outside"}, {"pytest", "/tmp"}, {"pytest", "@paths"}, {"python3", "-m", "compileall"}, {"python3", "-m", "compileall", "-j0", "pkg"}, {"python3", "-m", "compileall", "-i", "/tmp/files"}, {"ruff", "check", "--fix", "pkg"}, {"ruff", "format", "--config", "/tmp/other", "pkg"}} {
		if _, err := PreparePython(p, argv); err == nil {
			t.Fatal("unsafe vector accepted", argv)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(p.Worktree, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := PreparePython(p, []string{"pytest", "escape/test_x.py::test_one"}); err == nil {
		t.Fatal("symlink escape accepted")
	}
	p.LeaseHeld = false
	if _, err := PreparePython(p, []string{"pytest", "tests"}); err == nil {
		t.Fatal("test without lease")
	}
}
func TestPythonEnvironmentClearsOverridesAndClampsThreads(t *testing.T) {
	env, err := PythonEnvironment([]string{"python3", "-m", "pytest", "tests"}, []string{"PATH=/usr/bin", "PYTHONPATH=/foreign", "PYTEST_ADDOPTS=-n8", "PYTEST_PLUGINS=external", "RUFF_CACHE_DIR=/foreign", "RAYON_NUM_THREADS=99"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"/foreign", "-n8", "external", "=99"} {
		if strings.Contains(joined, bad) {
			t.Fatal(env)
		}
	}
	for _, key := range []string{"PYTEST_DISABLE_PLUGIN_AUTOLOAD=1", "PYTEST_ADDOPTS=", "RAYON_NUM_THREADS=1", "OMP_NUM_THREADS=1"} {
		if !strings.Contains(joined, key) {
			t.Fatal(env)
		}
	}
	untouched := []string{"PYTHONPATH=/legacy"}
	got, err := PythonEnvironment([]string{"go", "test", "."}, untouched, "")
	if err != nil || !reflect.DeepEqual(got, untouched) {
		t.Fatal(got, err)
	}
}
func TestPythonCompileallNative(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	p := pythonPolicy(t)
	file := filepath.Join(p.Worktree, "module.py")
	if err = os.WriteFile(file, []byte("def answer():\n    return 42\n"), 0600); err != nil {
		t.Fatal(err)
	}
	argv, err := PreparePython(p, []string{python, "-m", "compileall", "module.py"})
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	env, err := PythonEnvironment(argv, os.Environ(), cache)
	if err != nil {
		t.Fatal(err)
	}
	run := func() ([]byte, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = p.CWD
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	if out, err := run(); err != nil {
		t.Fatal(string(out), err)
	}
	if _, err = os.Stat(filepath.Join(p.Worktree, "__pycache__")); !os.IsNotExist(err) {
		t.Fatal("bytecode escaped private cache", err)
	}
	if err = os.WriteFile(file, []byte("def invalid(:\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err == nil || !strings.Contains(string(out), "SyntaxError") {
		t.Fatal("invalid Python compiled", string(out), err)
	}
}
