package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

type outputOwner struct {
	Task      string   `json:"task"`
	OutputDir string   `json:"output_dir"`
	Ancestors []string `json:"ancestors"`
	Parent    string   `json:"parent_task,omitempty"`
}
type outputOwnership struct {
	Version int           `json:"version"`
	Owners  []outputOwner `json:"owners"`
}

func (d *Daemon) outputOwnershipPath() string {
	return filepath.Join(d.layout.Run, "output-owners.json")
}

// The same lock is held shared by collectors through verification and applying
// their writes. A newly committed child cannot race a parent's collection.
func (d *Daemon) lockOutputOwnership(ctx context.Context) (func(), error) {
	f, err := os.OpenFile(d.outputOwnershipPath()+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func ownershipRows(rows []controlplane.JobSnapshot) ([]outputOwner, error) {
	out := []outputOwner{}
	for _, row := range rows {
		var e struct {
			Job struct {
				TaskID    string   `json:"task_id"`
				OutputDir string   `json:"output_dir"`
				Ancestors []string `json:"ancestors"`
				Parent    *struct {
					Task string `json:"task"`
				} `json:"parent"`
			} `json:"job"`
		}
		if err := json.Unmarshal(row.Payload, &e); err != nil {
			return nil, err
		}
		if e.Job.OutputDir == "" {
			continue
		}
		if e.Job.TaskID != row.ID {
			return nil, fmt.Errorf("output owner identity mismatch")
		}
		owner := outputOwner{Task: row.ID, OutputDir: e.Job.OutputDir, Ancestors: e.Job.Ancestors}
		if e.Job.Parent != nil {
			owner.Parent = e.Job.Parent.Task
		}
		out = append(out, owner)
	}
	return out, nil
}
func (d *Daemon) writeOutputOwnership(reg outputOwnership) (err error) {
	reg.Version = 1
	sort.Slice(reg.Owners, func(i, j int) bool { return reg.Owners[i].Task < reg.Owners[j].Task })
	raw, err := json.Marshal(reg)
	if err != nil {
		return err
	}
	path := d.outputOwnershipPath()
	f, err := os.CreateTemp(d.layout.Run, ".output-owners-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	dir, e := os.Open(d.layout.Run)
	if e != nil {
		return e
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// publishOutputOwners runs only after a committed admission, under the registry
// writer lock. Refused candidates never acquire an ownership entry.
func (d *Daemon) publishOutputOwners(rows []controlplane.JobSnapshot) (err error) {
	defer func() {
		if err != nil {
			_ = os.Remove(d.outputOwnershipPath())
		}
	}()
	raw, err := os.ReadFile(d.outputOwnershipPath())
	if os.IsNotExist(err) {
		readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, e := d.store.ListJobs(readCtx, 100000)
		if e != nil {
			return e
		}
		owners, e := ownershipRows(rows)
		if e != nil {
			return e
		}
		return d.writeOutputOwnership(outputOwnership{Version: 1, Owners: owners})
	}
	if err != nil {
		return err
	}
	var reg outputOwnership
	if err = json.Unmarshal(raw, &reg); err != nil {
		return err
	}
	if reg.Version != 1 {
		return fmt.Errorf("invalid output ownership registry version")
	}
	owners, err := ownershipRows(rows)
	if err != nil {
		return err
	}
	byTask := map[string]outputOwner{}
	for _, owner := range reg.Owners {
		byTask[owner.Task] = owner
	}
	for _, owner := range owners {
		byTask[owner.Task] = owner
	}
	reg.Owners = reg.Owners[:0]
	for _, owner := range byTask {
		reg.Owners = append(reg.Owners, owner)
	}
	return d.writeOutputOwnership(reg)
}
func (d *Daemon) rebuildOutputOwnership(ctx context.Context) error {
	unlock, err := d.lockOutputOwnership(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	rows, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		return err
	}
	owners, err := ownershipRows(rows)
	if err != nil {
		return err
	}
	return d.writeOutputOwnership(outputOwnership{Version: 1, Owners: owners})
}
