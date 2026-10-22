package engine

import (
	"errors"
	"strings"
)

// ParseRetirement requires exactly one classified reason. Repository and task
// identity evidence is checked by the daemon before the durable transition.
func ParseRetirement(reason string) (class, reference string, err error) {
	class, reference, ok := strings.Cut(strings.TrimSpace(reason), " ")
	reference = strings.TrimSpace(reference)
	if !ok || reference == "" {
		return "", "", errors.New("retirement requires reason class and reference")
	}
	switch class {
	case "superseded-by", "contained":
		if len(strings.Fields(reference)) != 1 {
			return "", "", errors.New("retirement requires one identity")
		}
	case "obsolete":
	default:
		return "", "", errors.New("unknown retirement reason class")
	}
	return class, reference, nil
}
