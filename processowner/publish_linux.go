package processowner

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames old to new only while new does not exist.
func renameNoReplace(old, new string) error {
	if e := unix.Renameat2(unix.AT_FDCWD, old, unix.AT_FDCWD, new, unix.RENAME_NOREPLACE); e != nil {
		return &os.LinkError{Op: "renameat2", Old: old, New: new, Err: e}
	}
	return nil
}
