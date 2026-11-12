package scratchfs

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openTopDir opens dir itself without following a link (O_PATH needs no
// permission on dir, so a mode-0000 directory opens), makes it 0700 through
// that descriptor and returns an os.Root opened through the descriptor's
// /proc link, which names the open directory itself, not dir's path. A link
// swapped in for dir after the open is never used (FAE-HOST-020).
func openTopDir(dir string) (*os.Root, error) {
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(fd)
	if topOpened != nil {
		topOpened(dir)
	}
	handle := "/proc/self/fd/" + strconv.Itoa(fd)
	// fchmodat2 (Linux 6.6) changes the descriptor's own inode; before it,
	// chmod through the /proc link does, as glibc's fchmodat does.
	if err = unix.Fchmodat(fd, "", 0700, unix.AT_EMPTY_PATH); err != nil {
		if err = unix.Chmod(handle, 0700); err != nil {
			return nil, &os.PathError{Op: "chmod", Path: dir, Err: err}
		}
	}
	return os.OpenRoot(handle)
}
