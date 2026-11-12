package daemon

import "strings"

// ImplicitPublicationFlag reports a review-creation argument that would
// publish or merge as a side effect (FAE-EXT-010): a pin whose name mentions
// publish or merge (case and separators ignored) and whose value is a boolean
// other than false. It fails closed for vendor flags no argument key maps.
func ImplicitPublicationFlag(name, value string) bool {
	n := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(name))
	if !strings.Contains(n, "publish") && !strings.Contains(n, "merge") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "0", "no", "off":
		return false
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
