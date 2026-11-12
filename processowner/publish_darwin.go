package processowner

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames old to new only while new does not exist.
func renameNoReplace(old, new string) error {
	if e := unix.RenamexNp(old, new, unix.RENAME_EXCL); e != nil {
		return &os.LinkError{Op: "renamex_np", Old: old, New: new, Err: e}
	}
	return nil
}
