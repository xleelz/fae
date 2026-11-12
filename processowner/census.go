package processowner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Process is one row of a read-only host process census. Start is the kernel
// start identity (Linux boot ID and start ticks; ps lstart elsewhere).
type Process struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	PGID    int    `json:"pgid"`
	SID     int    `json:"sid,omitempty"`
	Start   string `json:"start"`
	Command string `json:"command"`
	State   string `json:"state"`
	RSS     int64  `json:"rss_bytes"`
	// DefunctRounds is how many earlier censuses of the same Tree already
	// saw this process defunct (0 the first time); Tree.Members sets it.
	DefunctRounds int `json:"defunct_rounds,omitempty"`
}

func (p Process) Defunct() bool { return strings.HasPrefix(p.State, "Z") }

// Census reads every process on the host and signals none. Linux walks /proc
// (a process that exits mid-walk is skipped); other systems fall back to ps.
func Census() ([]Process, error) {
	if runtime.GOOS != "linux" {
		out, e := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,stat=,rss=,lstart=,comm=").Output()
		if e != nil {
			return nil, e
		}
		return parsePS(out)
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return nil, e
	}
	entries, e := os.ReadDir("/proc")
	if e != nil {
		return nil, e
	}
	page := int64(os.Getpagesize())
	out := make([]Process, 0, len(entries))
	for _, entry := range entries {
		if _, e := strconv.Atoi(entry.Name()); e != nil {
			continue
		}
		raw, e := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if e != nil {
			continue
		}
		p, e := parseStat(string(raw), page)
		if e != nil {
			return nil, e
		}
		p.Start = strings.TrimSpace(string(boot)) + ":" + p.Start
		out = append(out, p)
	}
	return out, nil
}

// CensusOf reads only the given processes, skipping any that exited: on Linux
// a cheap read of the instance's own tree for the per-round leak census
// (FAE-HOST-022, FAE-ARC-002). Other systems fall back to a full census.
func CensusOf(pids []int) ([]Process, error) {
	if runtime.GOOS != "linux" {
		all, e := Census()
		if e != nil {
			return nil, e
		}
		want := map[int]bool{}
		for _, pid := range pids {
			want[pid] = true
		}
		out := all[:0]
		for _, p := range all {
			if want[p.PID] {
				out = append(out, p)
			}
		}
		return out, nil
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return nil, e
	}
	page := int64(os.Getpagesize())
	out := make([]Process, 0, len(pids))
	for _, pid := range pids {
		raw, e := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if e != nil {
			continue
		}
		p, e := parseStat(string(raw), page)
		if e != nil {
			return nil, e
		}
		p.Start = strings.TrimSpace(string(boot)) + ":" + p.Start
		out = append(out, p)
	}
	return out, nil
}

func parseStat(raw string, page int64) (Process, error) {
	open, end := strings.IndexByte(raw, '('), strings.LastIndexByte(raw, ')')
	if open < 1 || end < open {
		return Process{}, errors.New("invalid proc stat")
	}
	f := strings.Fields(raw[end+1:])
	if len(f) < 22 {
		return Process{}, errors.New("short proc stat")
	}
	var n [5]int64
	for i, s := range []string{strings.TrimSpace(raw[:open]), f[1], f[2], f[3], f[21]} {
		v, e := strconv.ParseInt(s, 10, 64)
		if e != nil {
			return Process{}, fmt.Errorf("invalid proc stat field: %w", e)
		}
		n[i] = v
	}
	return Process{PID: int(n[0]), PPID: int(n[1]), PGID: int(n[2]), SID: int(n[3]), Start: f[19], Command: raw[open+1 : end], State: f[0], RSS: n[4] * page}, nil
}

// parsePS reads `ps -axo pid=,ppid=,pgid=,stat=,rss=,lstart=,comm=` (rss in
// KiB, a five-word lstart, a command that may contain spaces).
func parsePS(out []byte) ([]Process, error) {
	var rows []Process
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 11 {
			return nil, errors.New("unreadable process census")
		}
		var n [4]int64
		for i, s := range []string{f[0], f[1], f[2], f[4]} {
			v, e := strconv.ParseInt(s, 10, 64)
			if e != nil {
				return nil, errors.New("unreadable process census")
			}
			n[i] = v
		}
		rows = append(rows, Process{PID: int(n[0]), PPID: int(n[1]), PGID: int(n[2]), Start: strings.Join(f[5:10], " "), Command: filepath.Base(strings.Join(f[10:], " ")), State: f[3], RSS: n[3] << 10})
	}
	return rows, nil
}

// Tree is an instance process tree across censuses. A member is the root, a
// descendant of a member, a process in a group led by a member (other than
// the root's own group, which a shell pipeline may share), or a process seen
// as a member before whose exact identity is unchanged: a descendant that
// called setsid stays owned after its parent exits. A process reparented
// before any census observed it cannot be attributed. The tree also counts,
// per member, the consecutive censuses that saw it defunct (DefunctRounds),
// which Analyze's grace for a running command's defunct processes uses.
type Tree struct {
	known         map[string]bool
	defunctRounds map[string]int
}

func (t *Tree) Members(all []Process, root int) []Process {
	key := func(p Process) string { return strconv.Itoa(p.PID) + "/" + p.Start }
	rootGroup := -1
	for _, p := range all {
		if p.PID == root {
			rootGroup = p.PGID
		}
	}
	member := map[int]bool{root: true}
	groups := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for _, p := range all {
			if member[p.PID] {
				if p.PID == p.PGID && p.PGID != rootGroup && !groups[p.PGID] {
					groups[p.PGID], changed = true, true
				}
				continue
			}
			if member[p.PPID] || (groups[p.PGID] && p.PGID != rootGroup) || t.known[key(p)] {
				member[p.PID], changed = true, true
			}
		}
	}
	out := []Process{}
	known := map[string]bool{}
	defunctRounds := map[string]int{}
	for _, p := range all {
		if member[p.PID] {
			if p.Defunct() {
				p.DefunctRounds = t.defunctRounds[key(p)]
				defunctRounds[key(p)] = p.DefunctRounds + 1
			}
			out = append(out, p)
			known[key(p)] = true
		}
	}
	t.known, t.defunctRounds = known, defunctRounds
	return out
}

// LeakLimits are the FAE-HOST-022 bounds: per command name, a process count
// and summed resident memory; across the tree, defunct processes. A value at a
// bound does not alert; one beyond it does.
type LeakLimits struct {
	Processes int   `json:"processes"`
	RSS       int64 `json:"rss_bytes"`
	Defunct   int   `json:"defunct"`
}

func DefaultLeakLimits() LeakLimits { return LeakLimits{Processes: 200, RSS: 4 << 30, Defunct: 20} }

type CommandUsage struct {
	Command   string `json:"command"`
	Processes int    `json:"processes"`
	RSS       int64  `json:"rss_bytes"`
	Defunct   int    `json:"defunct"`
}

// Leak names one exceeded bound; a defunct leak covers the whole tree.
type Leak struct {
	Command   string `json:"command,omitempty"`
	Reason    string `json:"reason"`
	Processes int    `json:"processes,omitempty"`
	RSS       int64  `json:"rss_bytes,omitempty"`
	Defunct   int    `json:"defunct,omitempty"`
}

// DefunctParent is one process that has defunct children in the tree: the
// one that has not collected them, so a defunct leak can be traced to it.
type DefunctParent struct {
	// Command is the parent's command name ("" when it is not in the census).
	Command string `json:"command"`
	PID     int    `json:"pid"`
	// Defunct is how many of its children are defunct; Graced how many of
	// them are still left to it (inside a running brokered command's tree,
	// seen defunct for fewer than CommandDefunctGrace censuses, and within
	// the report's cap of limits.Defunct graced processes).
	Defunct int `json:"defunct"`
	Graced  int `json:"graced,omitempty"`
	// Rounds is how many earlier censuses already saw the oldest of them
	// defunct (Process.DefunctRounds).
	Rounds int `json:"rounds"`
	// Running reports a parent inside a running brokered command's tree.
	Running bool `json:"running,omitempty"`
}

// CommandDefunctGrace is how many consecutive censuses a defunct process
// inside a running brokered command's tree is left to that command before
// it counts against the defunct bound: the census that first sees it defunct
// and the next one. A command that collects its children collects them well
// within that; one that lets them accumulate is a leak like any other.
const CommandDefunctGrace = 2

// defunctParentsListed bounds the parents a report names, largest first.
const defunctParentsListed = 32

type LeakReport struct {
	Root      int   `json:"root"`
	Processes int   `json:"processes"`
	RSS       int64 `json:"rss_bytes"`
	Defunct   int   `json:"defunct"`
	// DefunctInCommands are the defunct processes (of Defunct) inside a
	// running brokered command's tree that are still within
	// CommandDefunctGrace: that command gets that long to reap them, so they
	// are not held against the bound. At most limits.Defunct of them are
	// graced (when it is positive), the first in member order; the rest
	// count against the bound. Older ones count whether or not their
	// command is still running.
	DefunctInCommands int `json:"defunct_in_commands,omitempty"`
	// DefunctParents names the processes whose children are defunct, most
	// defunct children first (at most defunctParentsListed), so an alert
	// says which command is not collecting them.
	DefunctParents []DefunctParent `json:"defunct_parents,omitempty"`
	Commands       []CommandUsage  `json:"commands"`
	Leaks          []Leak          `json:"leaks"`
	// Foreign lists host commands outside the tree beyond a bound. They are
	// reported only; Fae never acts on a foreign process.
	Foreign []CommandUsage `json:"foreign"`
	Limits  LeakLimits     `json:"limits"`
}

func usage(ps []Process) []CommandUsage {
	by := map[string]*CommandUsage{}
	for _, p := range ps {
		u := by[p.Command]
		if u == nil {
			u = &CommandUsage{Command: p.Command}
			by[p.Command] = u
		}
		u.Processes++
		u.RSS += p.RSS
		if p.Defunct() {
			u.Defunct++
		}
	}
	out := make([]CommandUsage, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Command < out[j].Command })
	return out
}

// Analyze sums the tree per command name and lists every exceeded bound.
// running are the processes of the brokered commands running now, each the
// leader of its own process group (as StartRole starts them), matched by
// exact identity. A defunct process in a running command's tree (a
// descendant of it, or a member of its group) is that command's to collect,
// so for its first CommandDefunctGrace censuses (Process.DefunctRounds, from
// the Tree the members came from) it is counted in DefunctInCommands and not
// held against the defunct bound; it still counts towards its command name's
// process bound. The grace covers at most limits.Defunct defunct processes
// (when it is positive), taken in member order; any more count against the
// bound at once, so a command cannot hide a defunct leak inside the grace.
// One the command has left defunct for longer counts against the bound like
// any other, as does one whose command has exited, until it is reaped.
// DefunctParents names the processes that have not collected their defunct
// children, so a defunct leak says which command they belong to. A member
// slice not produced by Tree.Members has every DefunctRounds zero, so every
// defunct process inside a running command is within the grace.
func Analyze(all, members []Process, root int, limits LeakLimits, running ...Process) LeakReport {
	r := LeakReport{Root: root, Commands: usage(members), Leaks: []Leak{}, Foreign: []CommandUsage{}, Limits: limits}
	inside := map[int]bool{}
	for _, p := range members {
		inside[p.PID] = true
	}
	for _, u := range r.Commands {
		r.Processes += u.Processes
		r.RSS += u.RSS
		r.Defunct += u.Defunct
		if u.Processes > limits.Processes {
			r.Leaks = append(r.Leaks, Leak{Command: u.Command, Reason: "processes", Processes: u.Processes, RSS: u.RSS})
		}
		if u.RSS > limits.RSS {
			r.Leaks = append(r.Leaks, Leak{Command: u.Command, Reason: "rss", Processes: u.Processes, RSS: u.RSS})
		}
	}
	if r.Defunct > 0 {
		var owned map[int]bool
		if len(running) > 0 {
			owned = commandTrees(members, root, running)
		}
		parents := map[int]*DefunctParent{}
		for _, p := range members {
			if !p.Defunct() {
				continue
			}
			graced := owned[p.PID] && p.DefunctRounds < CommandDefunctGrace && (limits.Defunct <= 0 || r.DefunctInCommands < limits.Defunct)
			if graced {
				r.DefunctInCommands++
			}
			parent := parents[p.PPID]
			if parent == nil {
				parent = &DefunctParent{PID: p.PPID, Running: owned[p.PPID]}
				parents[p.PPID] = parent
			}
			parent.Defunct++
			if graced {
				parent.Graced++
			}
			parent.Rounds = max(parent.Rounds, p.DefunctRounds)
		}
		for _, p := range all {
			if parent := parents[p.PID]; parent != nil {
				parent.Command = p.Command
			}
		}
		for _, parent := range parents {
			r.DefunctParents = append(r.DefunctParents, *parent)
		}
		sort.Slice(r.DefunctParents, func(i, j int) bool {
			a, b := r.DefunctParents[i], r.DefunctParents[j]
			return a.Defunct > b.Defunct || a.Defunct == b.Defunct && a.PID < b.PID
		})
		if len(r.DefunctParents) > defunctParentsListed {
			r.DefunctParents = r.DefunctParents[:defunctParentsListed]
		}
	}
	if counted := r.Defunct - r.DefunctInCommands; counted > limits.Defunct {
		r.Leaks = append(r.Leaks, Leak{Reason: "defunct", Defunct: counted})
	}
	var foreign []Process
	for _, p := range all {
		if !inside[p.PID] {
			foreign = append(foreign, p)
		}
	}
	for _, u := range usage(foreign) {
		if u.Processes > limits.Processes || u.RSS > limits.RSS {
			r.Foreign = append(r.Foreign, u)
		}
	}
	return r
}

// commandTrees are the members inside a running command's tree: the command
// itself, its descendants, and members of the process group it leads. A
// command is in the tree only by its exact identity (PID and start), and
// never when it is the root or leads the root's own group.
func commandTrees(members []Process, root int, running []Process) map[int]bool {
	rootGroup := -1
	for _, p := range members {
		if p.PID == root {
			rootGroup = p.PGID
		}
	}
	owned, groups := map[int]bool{}, map[int]bool{}
	for _, c := range running {
		for _, p := range members {
			if p.PID == c.PID && p.Start == c.Start && p.PID != root && p.PID == p.PGID && p.PGID != rootGroup {
				owned[p.PID], groups[p.PGID] = true, true
			}
		}
	}
	for changed := len(owned) > 0; changed; {
		changed = false
		for _, p := range members {
			if !owned[p.PID] && p.PID != root && (owned[p.PPID] || groups[p.PGID]) {
				owned[p.PID], changed = true, true
			}
		}
	}
	return owned
}
