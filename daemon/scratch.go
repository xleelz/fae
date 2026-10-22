package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type ScratchIdentity struct {
	Device        uint64        `json:"device"`
	Inode         uint64        `json:"inode"`
	ID            string        `json:"id"`
	Instance      string        `json:"instance"`
	Root          string        `json:"root"`
	Workspace     string        `json:"workspace"`
	Created       time.Time     `json:"created"`
	MessageMaxAge time.Duration `json:"message_max_age"`
}

func lifecycleLock(ctx context.Context, home, name string) (func(), error) {
	if _, err := Paths(home, name); err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".lifecycle")
	if err := mkdirPrivate(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
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
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
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
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func scratchIdentity(l Layout) (ScratchIdentity, error) {
	var id ScratchIdentity
	if err := checkPrivateRegular(filepath.Join(l.Root, "scratch.json")); err != nil {
		return id, err
	}
	raw, err := os.ReadFile(filepath.Join(l.Root, "scratch.json"))
	if err != nil {
		return id, err
	}
	if err = json.Unmarshal(raw, &id); err != nil {
		return id, err
	}
	if len(id.ID) != 32 || id.Root != l.Root || id.Instance != filepath.Base(l.Root) || id.Workspace == "" {
		return id, errors.New("invalid scratch identity")
	}
	if _, err = hex.DecodeString(id.ID); err != nil {
		return id, err
	}
	info, err := os.Lstat(l.Root)
	if err != nil {
		return id, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(st.Dev) != id.Device || uint64(st.Ino) != id.Inode || st.Uid != uint32(os.Getuid()) {
		return id, errors.New("scratch root identity changed")
	}
	if real, err := filepath.EvalSymlinks(l.Root); err != nil || real != l.Root {
		return id, errors.New("nonphysical scratch root")
	}
	var binding struct {
		Workspace string `json:"workspace"`
	}
	raw, err = os.ReadFile(l.Binding)
	if os.IsNotExist(err) {
		return id, nil
	}
	if err != nil {
		return id, err
	}
	if err = json.Unmarshal(raw, &binding); err != nil || binding.Workspace != id.Workspace {
		return id, errors.New("scratch workspace binding mismatch")
	}
	return id, nil
}
func checkPrivateRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("unowned or unsafe private file %s", path)
	}
	return nil
}
func prepareScratch(c Config, l Layout, fresh bool) error {
	// A pending removal is never reopened by a daemon. The controller owns retry.
	raw, err := os.ReadFile(scratchJournalPath(c.Root, c.Instance))
	if err == nil {
		var j ScratchCleanupResult
		if json.Unmarshal(raw, &j) != nil {
			return errors.New("invalid scratch cleanup journal")
		}
		if !j.Completed {
			return errors.New("scratch cleanup pending; retry cleanup before startup")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	marker := filepath.Join(l.Root, "scratch.json")
	if _, err = os.Lstat(marker); err == nil {
		id, e := scratchIdentity(l)
		if e != nil {
			return e
		}
		if id.Workspace != c.Workspace {
			return errors.New("scratch workspace mismatch")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if !c.Scratch {
		return nil
	}
	if !fresh {
		return errors.New("cannot mark existing instance scratch")
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return err
	}
	id := ScratchIdentity{ID: hex.EncodeToString(random), Instance: c.Instance, Root: l.Root, Workspace: c.Workspace, Created: time.Now().UTC(), MessageMaxAge: c.MessageMaxAge}
	info, err := os.Lstat(l.Root)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("scratch root identity unavailable")
	}
	id.Device = uint64(st.Dev)
	id.Inode = uint64(st.Ino)
	data, _ := json.Marshal(id)
	return writePrivate(marker, data)
}
func scratchJournalPath(home, name string) string {
	return filepath.Join(home, ".scratch-archive", name+".json")
}
