//go:build !linux

package processowner

import "errors"

func setSubreaper() error { return errors.New("child subreaper unsupported on this platform") }

func directChildren() ([]Process, error) { return censusChildren() }

// DescendantPIDs is unavailable here; callers fall back to a full census.
func DescendantPIDs(int) ([]int, error) { return nil, errors.New("descendant walk unsupported") }
