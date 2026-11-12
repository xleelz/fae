package commandshim

import "strings"

// InterpreterToolCommand recognizes interpreter-native build/test entrypoints
// that have no global PATH wrapper. Ordinary interpreter/provider boots pass.
func InterpreterToolCommand(name string, args []string) bool {
	if name == "node" || name == "nodejs" {
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "--" || !strings.HasPrefix(a, "-") {
				return false
			}
			if a == "--test" || strings.HasPrefix(a, "--test=") {
				return true
			}
			switch a {
			case "-e", "--eval", "-p", "--print":
				return false
			case "-r", "--require", "--import", "--loader", "--experimental-loader":
				i++
			}
		}
		return false
	}
	if !pythonInterpreter(name) {
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return false
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'c':
				return false
			case 'W', 'X':
				if j+1 == len(a) {
					i++
				}
				j = len(a)
			case 'm':
				module := a[j+1:]
				if module == "" && i+1 < len(args) {
					module = args[i+1]
				}
				return module == "compileall" || module == "py_compile"
			}
		}
	}
	return false
}
