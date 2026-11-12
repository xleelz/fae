//go:build !linux && !darwin

package scratchfs

import (
	"errors"
	"os"
)

// openTopDir checks dir is a directory, not a link, then changes and opens
// it by name: elsewhere than Linux and macOS a link swapped in for dir
// between the check and the open is followed.
func openTopDir(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New(dir + " is not a directory")
	}
	if topOpened != nil {
		topOpened(dir)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}
