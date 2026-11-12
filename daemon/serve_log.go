package daemon

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
)

// ServeLogEnv names a detached daemon's standard output and error file
// (<home>/<instance>.serve.log); `fae serve` sets it for its detached child.
const ServeLogEnv = "FAE_SERVE_LOG"

// fdRotator rotates the file behind a detached daemon's standard output and
// error at the supervision log's limit, keeping as many older files
// (FAE-OBS-001): the stderr copy of every supervision line would otherwise grow
// without bound. Rotation renames the file and points the descriptors at a
// fresh private one; a child that inherited the old descriptor keeps writing
// to the renamed file.
type fdRotator struct {
	mu          sync.Mutex
	path        string
	target      *os.File
	fds         []int
	size, limit int64
	keep        int
}

// serveLogRotator returns a rotator for this process's standard error when
// ServeLogEnv names the regular private file it writes to, else nil. The
// variable is consumed so the daemon's children do not inherit it.
func serveLogRotator() *fdRotator {
	path := os.Getenv(ServeLogEnv)
	os.Unsetenv(ServeLogEnv)
	if path == "" {
		return nil
	}
	info, err := os.Stderr.Stat()
	named, statErr := os.Stat(path)
	if err != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !os.SameFile(info, named) {
		return nil
	}
	fds := []int{int(os.Stderr.Fd())}
	if out, err := os.Stdout.Stat(); err == nil && os.SameFile(out, named) {
		fds = append(fds, int(os.Stdout.Fd()))
	}
	return &fdRotator{path: path, target: os.Stderr, fds: fds, size: info.Size(), limit: supervisionLogLimit, keep: supervisionLogKeep}
}

func (r *fdRotator) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(b)) > r.limit {
		if err := r.rotate(); err != nil {
			fmt.Fprintf(r.target, "serve log rotation failed: %v\n", err)
		}
	}
	n, err := r.target.Write(b)
	r.size += int64(n)
	return n, err
}

func (r *fdRotator) rotate() error {
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(r.path+"."+strconv.Itoa(i), r.path+"."+strconv.Itoa(i+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, fd := range r.fds {
		if err = syscall.Dup3(int(f.Fd()), fd, 0); err != nil {
			return err
		}
	}
	r.size = 0
	return nil
}
