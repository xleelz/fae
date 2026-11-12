package scratchfs

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openTopDir opens dir itself without following a link, makes it 0700
// through that descriptor and returns an os.Root opened through /dev/fd,
// which names the open directory itself, not dir's path. A directory that
// cannot be opened for reading first has its mode changed by name without
// following a link (a link swapped in has only its own mode changed), and is
// then opened again, refusing a link (FAE-HOST-020).
func openTopDir(dir string) (*os.Root, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_DIRECTORY | unix.O_CLOEXEC
	fd, err := unix.Open(dir, flags, 0)
	if errors.Is(err, unix.EACCES) {
		if err = unix.Fchmodat(unix.AT_FDCWD, dir, 0700, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			fd, err = unix.Open(dir, flags, 0)
		}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(fd)
	if topOpened != nil {
		topOpened(dir)
	}
	if err = unix.Fchmod(fd, 0700); err != nil {
		return nil, &os.PathError{Op: "chmod", Path: dir, Err: err}
	}
	return os.OpenRoot("/dev/fd/" + strconv.Itoa(fd))
}
