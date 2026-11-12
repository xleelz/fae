package daemon

import (
	"strings"
	"testing"
)

func nonLiteralLanguageRedIdentity() error {
	_, _, _, err := liveRedIdentity(map[string]any{"test_id": "selected.*", "test_shape": "rust-unit", "expected_failure": "actual defect", "declared_files": []any{"tests/selected.rs"}}, []string{"tests/selected.rs"}, true)
	return err
}

func TestLiveDerivedLanguageRedRefusesNonliteralIdentifier(t *testing.T) {
	// Census marker: "RedReport test_id %q is not a literal test identifier", id
	err := nonLiteralLanguageRedIdentity()
	if !isScopeRefusal(err) || !strings.Contains(err.Error(), "is not a literal test identifier") {
		t.Fatalf("want typed literal identifier refusal, got %v", err)
	}
	for _, id := range []string{"tests::selected", "suite/selected"} {
		_, got, _, err := liveRedIdentity(map[string]any{"test_id": id, "test_shape": "rust-unit", "expected_failure": "actual defect", "declared_files": []any{"tests/selected.rs"}}, []string{"tests/selected.rs"}, true)
		if err != nil || got != id {
			t.Fatalf("literal %q: %q %v", id, got, err)
		}
	}
}
