package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSeededPatternFixtures(t *testing.T) {
	cases := []struct {
		pattern          string
		minimum, maximum int
	}{
		{`^[A-Z]{3}$`, 3, 3},
		{`^report-[0-9]{2,4}\.yaml$`, 0, 32},
		{`^(ab|cde){2,4}$`, 7, 9},
		{`^[αβ]{2,4}$`, 3, 3},
		{`[0-9]{2}`, 10, 12},
		{`^a*$`, 0, 0},
		{`\b[A-Z]{3}\b`, 3, 3},
		{`\Bfoo\B`, 5, 5},
	}
	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			schema, _ := json.Marshal(map[string]any{"type": "string", "pattern": c.pattern, "minLength": c.minimum, "maxLength": c.maximum})
			for seed := int64(0); seed < 12; seed++ {
				first, err := SchemaValidFixture(schema, seed)
				if err != nil {
					t.Fatalf("seed%d: %v", seed, err)
				}
				if err := ValidateArtifact(schema, first); err != nil {
					t.Fatal(string(first), err)
				}
				second, err := SchemaValidFixture(schema, seed)
				if err != nil || string(first) != string(second) {
					t.Fatalf("nondeterministic seed%d: %s %s %v", seed, first, second, err)
				}
			}
		})
	}
}
func TestPatternFixturesStayBounded(t *testing.T) {
	for _, pattern := range []string{`^a{1000}a{1000}a{1000}a{1000}a{1000}$`, `^a+$`} {
		schema, _ := json.Marshal(map[string]any{"type": "string", "pattern": pattern, "minLength": 5000})
		if _, err := SchemaValidFixture(schema, 1); err == nil {
			t.Fatal("unbounded fixture accepted")
		}
	}
	schema, _ := json.Marshal(map[string]any{"type": "string", "pattern": "^" + strings.Repeat("(a|b)", 100) + "$"})
	if _, err := SchemaValidFixture(schema, 1); err != nil {
		t.Fatal(err)
	}
}
