package workflow

import (
	"path"
	"strings"
)

// TestSourceFile identifies conventional test files for automatically derived
// RED scopes. Other layouts can use an operator-pinned test_file.
func TestSourceFile(name, shape string) bool {
	base := path.Base(name)
	if base == "" || base[0] == '.' || base[0] == '_' {
		return false
	}
	switch shape {
	case "python-pytest":
		return strings.HasSuffix(base, ".py") && (strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py"))
	case "rust-unit":
		return strings.HasSuffix(base, ".rs") && (strings.HasPrefix(name, "tests/") || strings.Contains(name, "/tests/") || strings.HasSuffix(base, "_test.rs"))
	case "node-test":
		for _, suffix := range []string{".test.ts", ".spec.ts", ".test.js", ".spec.js", ".test.mjs", ".spec.mjs", ".test.cts", ".test.mts"} {
			if strings.HasSuffix(base, suffix) {
				return true
			}
		}
		return false
	case "":
		return TestSourceFile(name, "go-unit") || TestSourceFile(name, "python-pytest") || TestSourceFile(name, "rust-unit") || TestSourceFile(name, "node-test")
	default:
		return strings.HasSuffix(base, "_test.go") && len(base) > len("_test.go")
	}
}

// TestDeclarationName removes runner qualification from a frozen source name;
// the runner still receives and verifies the complete original identifier.
func TestDeclarationName(file, id string) string {
	if strings.HasSuffix(file, ".py") || strings.HasSuffix(file, ".rs") {
		parts := strings.Split(id, "::")
		id = parts[len(parts)-1]
		if strings.HasSuffix(file, ".py") {
			id, _, _ = strings.Cut(id, "[")
		}
	}
	return id
}
