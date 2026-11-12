package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lifecyclePreviewLock is the read-only form of lifecycleLock for a scratch
// cleanup preview: it shares an existing lifecycle lock with other previews
// and never creates the lock directory or file (FAE-CTL-043, FAE-CLI-001).
func lifecyclePreviewLock(ctx context.Context, home, name string) (func(), error) {
	if _, err := Paths(home, name); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, ".lifecycle", name+".lock"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) {
		f.Close()
		return nil, errors.New("unsafe lifecycle lock")
	}
	for {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if err != syscall.EAGAIN && err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
