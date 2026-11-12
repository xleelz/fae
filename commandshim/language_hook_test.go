package commandshim

import "testing"

func TestInterpreterToolCommandsNeedHostCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{{"node", []string{"--test", "x.test.js"}, true}, {"node", []string{"--require", "boot.js", "--test"}, true}, {"node", []string{"provider.js", "--test"}, false}, {"node", []string{"-e", "console.log('--test')"}, false}, {"node", []string{"--", "--test"}, false}, {"python3", []string{"-m", "compileall", "."}, true}, {"python3.12", []string{"-Bmpy_compile", "a.py"}, true}, {"python3", []string{"-X", "dev", "-m", "py_compile", "a.py"}, true}, {"python3", []string{"provider.py", "-m", "compileall"}, false}, {"python3", []string{"-c", "import compileall"}, false}} {
		if got := InterpreterToolCommand(tc.name, tc.args); got != tc.want {
			t.Fatalf("%s %v: %t", tc.name, tc.args, got)
		}
	}
}
