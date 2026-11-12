package commandshim

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FAE-HOST-015: every probe is a name go test runs as a test function: Test
// alone or Test and a suffix whose first rune is not lower case.
func TestGoTestNameProbesAreTestFunctionNames(t *testing.T) {
	for _, name := range goTestNameProbes {
		suffix, ok := strings.CutPrefix(name, "Test")
		if r, _ := utf8.DecodeRuneInString(suffix); !ok || (suffix != "" && unicode.IsLower(r)) {
			t.Errorf("%q is not a test function name", name)
		}
	}
}
