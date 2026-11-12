package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/scratchfs"
)

// removeHostScratch removes one leftover scratch tree; tests replace it to
// make a removal fail.
var removeHostScratch = scratchfs.RemoveAll

// hostScratchQuarantine is where the start sweep moves a scratch tree it
// cannot remove: under the instance root, outside the worktrees directory
// (where an unknown entry would refuse scratch cleanup), on the same file
// system.
func (d *Daemon) hostScratchQuarantine() string {
	return filepath.Join(d.layout.Root, "scratch-quarantine")
}

// sweepCommandScratch removes the brokered-command scratch (command-tmp-*) and
// policy directories (command-policy-*) a killed daemon left beside its
// worktrees, and the gate commands' TMPDIR a killed landing left in its
// scratch (landings/*/tmp): test scratch is removed on every exit path,
// including SIGKILL (FAE-HOST-020). It runs at start, after orphan
// termination and lease recovery, and only when no host-command lease proof
// remains unretired, so no surviving command can still be using one.
//
// A tree is removed even when a test left read-only directories in it (Go's
// module cache under its own GOPATH, a sealed or unsealed GOROOT mirror;
// scratchfs.RemoveAll). One that still cannot be removed is moved into
// scratch-quarantine under the instance root and logged, and one that cannot
// be moved either is logged and left: a leftover scratch tree never stops the
// daemon starting. Only reading the lease proofs or a bad pattern fails.
func (d *Daemon) sweepCommandScratch(journal *scheduler.Journal) error {
	if journal != nil {
		entries, err := journal.Entries()
		if err != nil {
			return err
		}
		for _, p := range entries {
			if p.Registry == filepath.Join(d.layout.Run, "effect-processes") {
				d.logLine("scratch", "command scratch sweep deferred: host command session=%q retirement unproven\n", p.Session)
				return nil
			}
		}
	}
	removed := 0
	for _, pattern := range []string{filepath.Join(d.layout.Worktrees, "command-tmp-*"), filepath.Join(d.layout.Worktrees, "command-policy-*"), filepath.Join(d.layout.Root, "landings", "*", "tmp")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, path := range matches {
			if info, err := os.Lstat(path); err != nil || !info.IsDir() {
				continue
			}
			// A policy directory may hold a sealed GOROOT mirror.
			if err := removeHostScratch(path); err != nil {
				d.quarantineHostScratch(path, err)
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		d.logLine("scratch", "removed %d brokered command scratch directories left by an earlier daemon\n", removed)
	}
	return nil
}

// quarantineHostScratch moves a scratch tree the sweep could not remove out
// of the sweep's way and logs where it went, or logs that it stays.
func (d *Daemon) quarantineHostScratch(path string, removeErr error) {
	moved, err := scratchfs.Quarantine(path, d.hostScratchQuarantine())
	if err != nil {
		d.logLine("scratch", "scratch %s left in place: not removed (%v) and not quarantined (%v); remove it by hand\n", path, removeErr, err)
		return
	}
	d.logLine("scratch", "scratch %s not removed (%v); quarantined as %s; remove it by hand\n", path, removeErr, moved)
}

// logScratchLeft logs a brokered run's scratch or policy directory
// (effects.Broker.ScratchLeft), or a landing's gate TMPDIR
// (controller.LandRequest.TempLeft), that could not be removed; the next
// start's sweep removes or quarantines it.
func (d *Daemon) logScratchLeft(dir string, err error) {
	d.logLine("scratch", "scratch %s not removed: %v; the next start sweeps it\n", dir, err)
}

// gateRunOwnerSuffix names the owner record `gate run` writes beside its
// scratch directory: gate-runs/<id> is owned by gate-runs/<id>.owner.
const gateRunOwnerSuffix = ".owner"

// gateRunOwner is a gate run's owner record: the process that runs it.
type gateRunOwner struct {
	PID   int    `json:"pid"`
	Birth string `json:"birth"`
}

// GateRunScratch creates `fae gate run`'s scratch directory under the
// instance root, <root>/gate-runs/<id>, after first publishing an owner
// record beside it naming this process, so the start sweep removes a scratch
// only once the gate run that made it is proven dead (FAE-HOST-020). remove
// removes the scratch, with any read-only tree a test left in it, and then
// the record; a record whose scratch is gone is removed too.
func GateRunScratch(root, id string) (dir string, remove func() error, err error) {
	parent := filepath.Join(root, "gate-runs")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return "", nil, err
	}
	dir = filepath.Join(parent, id)
	birth, err := processowner.Birth(os.Getpid())
	if err != nil {
		return "", nil, fmt.Errorf("gate run owner identity: %w", err)
	}
	record, _ := json.Marshal(gateRunOwner{PID: os.Getpid(), Birth: birth})
	if err = writePrivate(dir+gateRunOwnerSuffix, record); err != nil {
		return "", nil, err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		_ = os.Remove(dir + gateRunOwnerSuffix)
		return "", nil, err
	}
	return dir, func() error {
		if err := scratchfs.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.Remove(dir + gateRunOwnerSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}, nil
}

// sweepGateRunScratch removes the scratch a killed `fae gate run` left under
// gate-runs (FAE-HOST-020). A gate run can run beside the daemon, so a
// scratch is removed only when its owner record proves the gate run dead,
// and, on Linux, no process still runs with it as TMPDIR or GOTMPDIR (a
// command the killed gate run started runs in its own process group and can
// outlive it). A scratch with no owner record (an older binary's gate run)
// is left and logged. One that cannot be removed is quarantined, as the
// brokered-command sweep does; an owner record left without its scratch is
// removed once its owner is dead.
func (d *Daemon) sweepGateRunScratch() {
	parent := filepath.Join(d.layout.Root, "gate-runs")
	entries, err := os.ReadDir(parent)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			d.logLine("scratch", "gate run scratch not swept: %v\n", err)
		}
		return
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(parent, name)
		if id, record := strings.CutSuffix(name, gateRunOwnerSuffix); record {
			if _, err := os.Lstat(filepath.Join(parent, id)); errors.Is(err, fs.ErrNotExist) && gateRunOwnerDead(path) {
				_ = os.Remove(path)
			}
			continue
		}
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Lstat(path + gateRunOwnerSuffix); err != nil {
			d.logLine("scratch", "gate run scratch %s left: it has no owner record, so its gate run cannot be proven dead; remove it by hand once no gate run uses it\n", path)
			continue
		}
		if !gateRunOwnerDead(path + gateRunOwnerSuffix) {
			continue // its gate run is running (or unproven): it removes its own scratch
		}
		if holder := scratchHolder(path); holder != 0 {
			d.logLine("scratch", "gate run scratch %s kept: its gate run is dead, but process %d still runs with it as its temporary directory\n", path, holder)
			continue
		}
		if err := removeHostScratch(path); err != nil {
			d.quarantineHostScratch(path, err)
		} else {
			removed++
		}
		_ = os.Remove(path + gateRunOwnerSuffix)
	}
	if removed > 0 {
		d.logLine("scratch", "removed %d gate run scratch directories left by killed gate runs\n", removed)
	}
}

// gateRunOwnerDead reports a readable owner record whose process is proven
// dead (processowner.ProvedDead); an unreadable record is not proof.
func gateRunOwnerDead(record string) bool {
	raw, err := os.ReadFile(record)
	if err != nil {
		return false
	}
	var owner gateRunOwner
	if json.Unmarshal(raw, &owner) != nil {
		return false
	}
	return processowner.ProvedDead(owner.PID, owner.Birth)
}

// scratchHolder returns a live process whose environment names dir as its
// TMPDIR or GOTMPDIR (0 when none). It reads /proc, so it finds one only on
// Linux, and only among processes whose environment this user can read: a
// gate run's commands run as the operator.
func scratchHolder(dir string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	want := [][]byte{[]byte("TMPDIR=" + dir), []byte("GOTMPDIR=" + dir)}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, variable := range bytes.Split(raw, []byte{0}) {
			for _, w := range want {
				if bytes.Equal(variable, w) {
					return pid
				}
			}
		}
	}
	return 0
}

// ScratchQuarantineReport is what <instance>/scratch-quarantine holds: the
// scratch trees a start sweep could neither remove nor leave in place (status
// startup_checks.scratch_quarantine, measured at start). Nothing removes
// them; the operator does.
type ScratchQuarantineReport struct {
	Path    string `json:"path"`
	Entries int    `json:"entries"`
	// Bytes is the regular files' apparent size; a lower bound when
	// Incomplete names what could not be measured.
	Bytes int64 `json:"bytes"`
	// Names are the first scratchQuarantineNamed entries, in name order.
	Names      []string `json:"names,omitempty"`
	Incomplete string   `json:"incomplete,omitempty"`
}

// scratchQuarantineNamed bounds the names the report and its alert list;
// scratchQuarantineWalk bounds the entries the measurement visits.
const (
	scratchQuarantineNamed = 20
	scratchQuarantineWalk  = 200000
)

// measureScratchQuarantine reports what scratch-quarantine holds, visiting at
// most scratchQuarantineWalk entries without following any link.
func (d *Daemon) measureScratchQuarantine() ScratchQuarantineReport {
	r := ScratchQuarantineReport{Path: d.hostScratchQuarantine()}
	entries, err := os.ReadDir(r.Path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.Incomplete = err.Error()
		}
		return r
	}
	r.Entries = len(entries)
	for _, e := range entries[:min(len(entries), scratchQuarantineNamed)] {
		r.Names = append(r.Names, e.Name())
	}
	visited, unreadable := 0, 0
	walkErr := filepath.WalkDir(r.Path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			unreadable++
			return nil
		}
		if visited++; visited > scratchQuarantineWalk {
			return fs.SkipAll
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				r.Bytes += info.Size()
			}
		}
		return nil
	})
	switch {
	case walkErr != nil:
		r.Incomplete = walkErr.Error()
	case visited > scratchQuarantineWalk:
		r.Incomplete = fmt.Sprintf("measured the first %d entries only", scratchQuarantineWalk)
	case unreadable > 0:
		r.Incomplete = fmt.Sprintf("%d directories could not be read", unreadable)
	}
	return r
}

// reportScratchQuarantine records what scratch-quarantine holds in status
// and, when it holds anything, raises an audited warning alert naming it, so
// it cannot grow unseen (FAE-HOST-020). The alert is raised once per distinct
// content (deduplicated on its detail): a start that finds the same trees
// again raises no second alert, and one that finds more raises a new one.
func (d *Daemon) reportScratchQuarantine(ctx context.Context) {
	r := d.measureScratchQuarantine()
	d.updateStartupReport(func(s *StartupChecks) { s.ScratchQuarantine = &r })
	if r.Entries == 0 {
		return
	}
	d.logLine("scratch", "scratch-quarantine %s holds %d trees (%d bytes) the start sweep could not remove; inspect and remove them by hand (chmod -R u+w, then rm -rf)\n", r.Path, r.Entries, r.Bytes)
	detail, _ := json.Marshal(r)
	_, _, _ = d.raiseAlert(ctx, controlplane.Alert{Severity: "warning", Source: "scratch-quarantine", Detail: detail}, true)
}
