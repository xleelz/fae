package daemon

import "testing"

// FAE-EXT-010: unmapped vendor publish/merge flags fail closed.
func TestImplicitPublicationFlag(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{"autoMergeOnGreen", "true", true}, {"publish-when-ready", "1", true}, {"Auto_Publish", "YES", true}, {"merge_after_checks", "on", true},
		{"autoMergeOnGreen", "false", false}, {"publish_now", "", false}, {"merge_strategy", "squash", false}, {"draft", "true", false}, {"branch", "true", false},
	} {
		if got := ImplicitPublicationFlag(tc.name, tc.value); got != tc.want {
			t.Errorf("%s=%s: %v want %v", tc.name, tc.value, got, tc.want)
		}
	}
}
