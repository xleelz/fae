package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
)

// detectLeaks is FAE-HOST-022/-023: a per-command census of the daemon's own
// process tree. observeIdle schedules it: every drive round while busy, and
// while idle at once when the tree's process set changes, else every
// observeIdleCensus. A bound newly exceeded raises one alert; foreign host
// commands beyond a bound are logged and reported, never acted on. The full
// host walk that finds foreign processes runs when hostWide is set; an
// own-tree census reads only the daemon's own tree (a few /proc reads,
// FAE-ARC-002) and keeps the last foreign report.
func (d *Daemon) detectLeaks(ctx context.Context, hostWide bool) error {
	root := os.Getpid()
	var all []processowner.Process
	var err error
	if hostWide {
		all, err = processowner.Census()
	} else {
		var pids []int
		if pids, err = processowner.DescendantPIDs(root); err == nil {
			all, err = processowner.CensusOf(append([]int{root}, pids...))
		}
	}
	if err != nil {
		return err
	}
	d.obs.mu.Lock()
	limits := d.obs.limits
	if limits == (processowner.LeakLimits{}) {
		limits = processowner.DefaultLeakLimits()
	}
	members := d.obs.tree.Members(all, root)
	report := processowner.Analyze(all, members, root, limits, d.runningHostCommands(members, limits)...)
	if !hostWide {
		if d.obs.leaks != nil {
			report.Foreign = d.obs.leaks.Foreign
		}
	}
	d.obs.leaks = &report
	if d.obs.leakKeys == nil {
		d.obs.leakKeys, d.obs.foreign = map[string]bool{}, map[string]bool{}
	}
	var fresh []processowner.Leak
	current := map[string]bool{}
	for _, l := range report.Leaks {
		key := l.Command + "\x00" + l.Reason
		current[key] = true
		if !d.obs.leakKeys[key] {
			fresh = append(fresh, l)
		}
	}
	var foreign []processowner.CommandUsage
	if hostWide {
		seen := map[string]bool{}
		for _, u := range report.Foreign {
			seen[u.Command] = true
			if !d.obs.foreign[u.Command] {
				foreign = append(foreign, u)
			}
		}
		d.obs.foreign = seen
	}
	d.obs.mu.Unlock()
	for _, u := range foreign {
		d.logLine("leaks", "foreign process group beyond bound command=%q processes=%d rss_bytes=%d (reported only)\n", u.Command, u.Processes, u.RSS)
	}
	var raiseErr error
	for _, l := range fresh {
		fields := map[string]any{"leak": l, "limits": limits, "root": root, "processes": report.Processes, "rss_bytes": report.RSS, "defunct": report.Defunct}
		if l.Reason == "defunct" {
			// A defunct leak names the processes that have not collected
			// their children (command, pid, how many, for how many
			// censuses), so the next occurrence can be diagnosed.
			fields["defunct_in_commands"], fields["defunct_parents"] = report.DefunctInCommands, report.DefunctParents
		}
		detail, _ := json.Marshal(fields)
		if _, _, e := d.raiseAlert(ctx, controlplane.Alert{Source: "process-leak", Detail: detail}, false); e != nil {
			raiseErr = e
			delete(current, l.Command+"\x00"+l.Reason) // retry next round
		}
	}
	d.obs.mu.Lock()
	d.obs.leakKeys = current
	d.obs.mu.Unlock()
	return raiseErr
}

// runningHostCommands are the census rows of the brokered host commands
// running now: the live records of the effects registry, matched by exact
// identity. A defunct process inside one of their trees is the command's to
// reap, and processowner.Analyze leaves it to the command for
// processowner.CommandDefunctGrace censuses (two rounds), so a test suite
// whose processes exit a moment before their parents collect them raises no
// process-leak alert, while a command that lets defunct children accumulate
// is alerted after the grace, naming it (FAE-HOST-022). The registry is read
// only when the tree holds more defunct processes than the bound, so a quiet
// round reads nothing more.
func (d *Daemon) runningHostCommands(members []processowner.Process, limits processowner.LeakLimits) []processowner.Process {
	defunct := 0
	for _, p := range members {
		if p.Defunct() {
			defunct++
		}
	}
	if defunct <= limits.Defunct || d.layout.Run == "" {
		return nil
	}
	// A record that cannot be read is skipped: its command's defunct
	// processes then count against the bound, as before.
	records, _ := processowner.Inventory(filepath.Join(d.layout.Run, "effect-processes"))
	var running []processowner.Process
	for _, r := range records {
		for _, p := range members {
			if p.PID == r.PID && p.Start == r.Birth {
				running = append(running, p)
			}
		}
	}
	return running
}

// processReport is the last census for Metrics, largest commands first.
func (d *Daemon) processReport() *processowner.LeakReport {
	d.obs.mu.Lock()
	defer d.obs.mu.Unlock()
	if d.obs.leaks == nil {
		return nil
	}
	r := *d.obs.leaks
	r.Commands = append([]processowner.CommandUsage(nil), r.Commands...)
	sort.SliceStable(r.Commands, func(i, j int) bool { return r.Commands[i].RSS > r.Commands[j].RSS })
	return &r
}
