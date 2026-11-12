package processowner

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func proc(pid, ppid, pgid int, command, state string, rss int64) Process {
	return Process{PID: pid, PPID: ppid, PGID: pgid, SID: pgid, Start: "s" + strconv.Itoa(pid), Command: command, State: state, RSS: rss}
}

func TestAnalyzeLeakBoundaries(t *testing.T) {
	limits := LeakLimits{Processes: 3, RSS: 300, Defunct: 2}
	members := []Process{proc(100, 1, 100, "fae", "S", 10)}
	add := func(command, state string, n int, rss int64) {
		for i := 0; i < n; i++ {
			members = append(members, proc(1000+len(members), 100, 100, command, state, rss))
		}
	}
	add("at-count", "S", 3, 1)
	add("over-count", "S", 4, 1)
	add("at-rss", "S", 2, 150)
	add("over-rss", "R", 2, 151)
	add("zombie", "Z", 2, 0)
	all := append([]Process{}, members...)
	for i := 0; i < 5; i++ {
		all = append(all, proc(5000+i, 1, 5000+i, "foreign", "S", 1))
	}
	all = append(all, proc(6000, 1, 6000, "quiet", "S", 1))
	r := Analyze(all, members, 100, limits)
	if r.Processes != len(members) || r.Defunct != 2 || len(r.Leaks) != 2 {
		t.Fatalf("report %+v", r)
	}
	if r.Leaks[0] != (Leak{Command: "over-count", Reason: "processes", Processes: 4, RSS: 4}) || r.Leaks[1] != (Leak{Command: "over-rss", Reason: "rss", Processes: 2, RSS: 302}) {
		t.Fatalf("leaks %+v", r.Leaks)
	}
	if len(r.Foreign) != 1 || r.Foreign[0].Command != "foreign" || r.Foreign[0].Processes != 5 {
		t.Fatalf("foreign %+v", r.Foreign)
	}
	add("zombie", "Z", 1, 0)
	r = Analyze(all, members, 100, limits)
	if len(r.Leaks) != 3 || r.Leaks[2] != (Leak{Reason: "defunct", Defunct: 3}) {
		t.Fatalf("defunct leak %+v", r.Leaks)
	}
	if DefaultLeakLimits() != (LeakLimits{Processes: 200, RSS: 4 << 30, Defunct: 20}) {
		t.Fatal("Appendix A process-leak defaults")
	}
}

func TestTreeMembershipSurvivesSetsidAndReparenting(t *testing.T) {
	var tree Tree
	census := []Process{
		proc(100, 1, 100, "fae", "S", 1),
		proc(101, 100, 101, "dolt", "S", 1),     // owned group leader
		proc(102, 101, 102, "detached", "S", 1), // setsid descendant
		proc(103, 1, 101, "orphan", "S", 1),     // reparented, still in an owned group
		proc(104, 1, 100, "tee", "S", 1),        // shares the daemon's own group only
		proc(105, 1, 105, "foreign", "S", 1),
	}
	got := map[int]bool{}
	for _, p := range tree.Members(census, 100) {
		got[p.PID] = true
	}
	if len(got) != 4 || !got[100] || !got[101] || !got[102] || !got[103] {
		t.Fatalf("members %v", got)
	}
	census[2].PPID = 1 // the setsid descendant's parent exited
	reused := proc(106, 1, 106, "detached", "S", 1)
	reused.Start = "other"
	census = append(census[:3], census[4:]...)
	census = append(census, reused)
	got = map[int]bool{}
	for _, p := range tree.Members(census, 100) {
		got[p.PID] = true
	}
	if !got[102] || got[106] || got[104] {
		t.Fatalf("remembered members %v", got)
	}
	census[2].Start = "recycled" // pid 102 now names another process
	got = map[int]bool{}
	for _, p := range tree.Members(census, 100) {
		got[p.PID] = true
	}
	if got[102] {
		t.Fatal("recycled pid stayed a member")
	}
}

func TestCensusParsers(t *testing.T) {
	p, e := parseStat("4242 (my (odd) cmd) S 1 4242 4242 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 98765 1000 3 18446744073709551615", 4096)
	if e != nil || p != (Process{PID: 4242, PPID: 1, PGID: 4242, SID: 4242, Start: "98765", Command: "my (odd) cmd", State: "S", RSS: 3 * 4096}) {
		t.Fatalf("stat %+v %v", p, e)
	}
	if _, e = parseStat("4242 (cmd) S 1", 4096); e == nil {
		t.Fatal("short stat accepted")
	}
	rows, e := parsePS([]byte("  1     0     1 Ss    1024 Wed Oct 22 06:00:52 2026 /sbin/launchd\n 77     1    77 Z+       0 Wed Oct 22 06:01:00 2026 /usr/bin/Google Chrome Helper\n"))
	if e != nil || len(rows) != 2 || rows[1] != (Process{PID: 77, PPID: 1, PGID: 77, Start: "Wed Oct 22 06:01:00 2026", Command: "Google Chrome Helper", State: "Z+", RSS: 0}) || rows[0].RSS != 1024<<10 || !rows[1].Defunct() {
		t.Fatalf("ps %+v %v", rows, e)
	}
	if _, e = parsePS([]byte("1 0 1 S\n")); e == nil {
		t.Fatal("short ps row accepted")
	}
}

// spawn starts a fixture shell in its own group; cleanup kills only that group
// and any remembered detached descendant whose exact identity still matches.
func spawn(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}
func killExact(t *testing.T, p Process) {
	t.Helper()
	t.Cleanup(func() {
		if b, e := birth(p.PID); e == nil && b == p.Start {
			_ = syscall.Kill(p.PID, syscall.SIGKILL)
		}
	})
}
func members(t *testing.T, tree *Tree, root int, want func([]Process) bool) []Process {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		all, e := Census()
		if e != nil {
			t.Fatal(e)
		}
		got := tree.Members(all, root)
		if want(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("census of %d: %+v", root, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func count(ps []Process, command string, defunct bool) (n int, other []Process) {
	for _, p := range ps {
		if p.Command == command && p.Defunct() == defunct {
			n++
			other = append(other, p)
		}
	}
	return n, other
}

func TestCensusFixtureChainRaisesLeakIncludingSetsid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("setsid fixture is Linux-only")
	}
	cmd := spawn(t, "setsid sleep 30 & sleep 30 & sleep 30 & wait")
	var tree Tree
	got := members(t, &tree, cmd.Process.Pid, func(ps []Process) bool { n, _ := count(ps, "sleep", false); return n == 3 })
	_, sleeps := count(got, "sleep", false)
	detached := 0
	for _, p := range sleeps {
		killExact(t, p)
		if p.SID == p.PID {
			detached++
		}
	}
	if detached != 1 {
		t.Fatalf("setsid descendant not in the tree: %+v", sleeps)
	}
	all, _ := Census()
	r := Analyze(all, got, cmd.Process.Pid, LeakLimits{Processes: 2, RSS: 4 << 30, Defunct: 20})
	if len(r.Leaks) != 1 || r.Leaks[0].Command != "sleep" || r.Leaks[0].Reason != "processes" || r.Leaks[0].Processes != 3 || r.Leaks[0].RSS <= 0 {
		t.Fatalf("leak %+v", r)
	}
}

func TestCensusRemembersDetachedDescendantAfterParentExit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("setsid fixture is Linux-only")
	}
	cmd := spawn(t, "setsid sleep 30 & sleep 1")
	root := cmd.Process.Pid
	var tree Tree
	got := members(t, &tree, root, func(ps []Process) bool { n, _ := count(ps, "sleep", false); return n == 2 })
	var detached Process
	for _, p := range got {
		if p.Command == "sleep" && p.SID == p.PID {
			detached = p
		}
	}
	if detached.PID == 0 {
		t.Fatalf("no detached sleep in %+v", got)
	}
	killExact(t, detached)
	if e := cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	got = members(t, &tree, root, func(ps []Process) bool {
		for _, p := range ps {
			if p.PID == detached.PID && p.PPID != root {
				return true
			}
		}
		return false
	})
	var fresh Tree
	for _, p := range members(t, &fresh, root, func([]Process) bool { return true }) {
		if p.PID == detached.PID {
			t.Fatal("a never-observed reparented process cannot be claimed")
		}
	}
}

func TestCensusCountsDefunctChildren(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie command naming is checked on Linux")
	}
	cmd := spawn(t, "sleep 0 & sleep 0 & exec sleep 30")
	var tree Tree
	got := members(t, &tree, cmd.Process.Pid, func(ps []Process) bool { n, _ := count(ps, "sleep", true); return n == 2 })
	all, _ := Census()
	r := Analyze(all, got, cmd.Process.Pid, LeakLimits{Processes: 200, RSS: 4 << 30, Defunct: 1})
	if r.Defunct != 2 || len(r.Leaks) != 1 || r.Leaks[0] != (Leak{Reason: "defunct", Defunct: 2}) {
		t.Fatalf("defunct %+v", r)
	}
	for _, c := range r.Commands {
		if c.Command == "sleep" && (c.Processes != 3 || c.Defunct != 2) {
			t.Fatalf("usage %+v", c)
		}
	}
	if strings.Contains(strings.Join(func() []string {
		var s []string
		for _, l := range r.Leaks {
			s = append(s, l.Reason)
		}
		return s
	}(), ","), "processes") {
		t.Fatal("defunct children counted as a process leak")
	}
}

// FAE-HOST-022: Analyze holds a running command's defunct processes (its
// descendants and its group's members, the command matched by exact
// identity) to the command, not to the tree's defunct bound, and counts them
// in DefunctInCommands, up to limits.Defunct of them; the rest count against
// the bound. They still count towards their command name's process bound. A defunct process outside every running command, one under
// a command whose identity no longer matches, and one in the root's own
// group count as before.
func TestAnalyzeLeavesRunningCommandsDefunctProcessesToThem(t *testing.T) {
	limits := LeakLimits{Processes: 4, RSS: 1 << 30, Defunct: 2}
	members := []Process{proc(100, 1, 100, "fae", "S", 1), proc(200, 100, 200, "go", "S", 1), proc(201, 200, 200, "daemon.test", "S", 1)}
	for i := 0; i < 5; i++ {
		members = append(members, proc(300+i, 201, 200, "sh", "Z", 0)) // defunct under the running command
	}
	members = append(members, proc(400, 100, 200, "orphan", "Z", 0)) // reparented, still in the command's group
	command := members[1]
	r := Analyze(members, members, 100, limits, command)
	if r.Defunct != 6 || r.DefunctInCommands != 2 {
		t.Fatalf("report %+v", r)
	}
	if len(r.Leaks) != 2 || r.Leaks[0] != (Leak{Command: "sh", Reason: "processes", Processes: 5}) || r.Leaks[1] != (Leak{Reason: "defunct", Defunct: 4}) {
		t.Fatalf("a running command's defunct processes: leaks %+v, want the per-name process bound and those beyond the capped grace", r.Leaks)
	}
	// Without the running command (it exited, or its identity changed), the
	// same defunct processes are a defunct leak.
	stale := command
	stale.Start = "other"
	for _, running := range [][]Process{nil, {stale}, {members[0]}} {
		r = Analyze(members, members, 100, limits, running...)
		if r.DefunctInCommands != 0 || len(r.Leaks) != 2 || r.Leaks[1] != (Leak{Reason: "defunct", Defunct: 6}) {
			t.Fatalf("running %+v: report %+v", running, r)
		}
	}
	// Defunct processes outside the command are held to the bound.
	for i := 0; i < 3; i++ {
		members = append(members, proc(500+i, 100, 100, "stray", "Z", 0))
	}
	r = Analyze(members, members, 100, limits, command)
	if r.DefunctInCommands != 2 || r.Leaks[len(r.Leaks)-1] != (Leak{Reason: "defunct", Defunct: 7}) {
		t.Fatalf("defunct processes outside the command: %+v", r)
	}
}

// FAE-HOST-022: a running command's defunct processes are left to it only
// for CommandDefunctGrace censuses. The Tree counts the consecutive censuses
// that saw each member defunct (DefunctRounds); once a defunct process has
// been seen for that many, it counts against the defunct bound although its
// command still runs, and the report names the parent that has not
// collected it (command, pid, how many, for how many rounds). Before, every
// defunct process inside a running command was exempt for as long as the
// command ran, so a command holding 150 unreaped children raised nothing.
func TestAnalyzeCountsARunningCommandsDefunctProcessesAfterTheGrace(t *testing.T) {
	limits := LeakLimits{Processes: 100, RSS: 1 << 30, Defunct: 5}
	all := []Process{proc(100, 1, 100, "fae", "S", 1), proc(200, 100, 200, "go", "S", 1), proc(201, 200, 200, "daemon.test", "S", 1)}
	for i := 0; i < 5; i++ {
		all = append(all, proc(300+i, 201, 200, "sh", "Z", 0)) // defunct under the running command
	}
	all = append(all, proc(400, 100, 100, "stray", "Z", 0)) // defunct outside it, counted from the first census
	command := all[1]
	var tree Tree
	for round := 0; round < CommandDefunctGrace+1; round++ {
		members := tree.Members(all, 100)
		for _, p := range members {
			if want := round; p.Defunct() && p.DefunctRounds != want || !p.Defunct() && p.DefunctRounds != 0 {
				t.Fatalf("round %d: %+v", round, p)
			}
		}
		r := Analyze(all, members, 100, limits, command)
		var leak Leak
		for _, l := range r.Leaks {
			if l.Reason == "defunct" {
				leak = l
			}
		}
		parents := map[int]DefunctParent{}
		for _, p := range r.DefunctParents {
			parents[p.PID] = p
		}
		if round < CommandDefunctGrace {
			if r.DefunctInCommands != 5 || leak != (Leak{}) || parents[201] != (DefunctParent{Command: "daemon.test", PID: 201, Defunct: 5, Graced: 5, Rounds: round, Running: true}) {
				t.Fatalf("round %d, within the grace: %+v", round, r)
			}
		} else if r.DefunctInCommands != 0 || leak != (Leak{Reason: "defunct", Defunct: 6}) || parents[201] != (DefunctParent{Command: "daemon.test", PID: 201, Defunct: 5, Rounds: round, Running: true}) {
			t.Fatalf("round %d, after the grace: %+v", round, r)
		}
		if r.Defunct != 6 || parents[100] != (DefunctParent{Command: "fae", PID: 100, Defunct: 1, Rounds: round}) || r.DefunctParents[0].PID != 201 {
			t.Fatalf("round %d parents: %+v", round, r.DefunctParents)
		}
	}
	// A defunct process collected and replaced by another with the same pid
	// (another start identity) starts its grace again; one that stops being a
	// member is forgotten.
	all[3].Start = "reborn"
	all = all[:len(all)-1]
	members := tree.Members(all, 100)
	for _, p := range members {
		if p.Defunct() && p.PID == 300 && p.DefunctRounds != 0 || p.Defunct() && p.PID == 301 && p.DefunctRounds != CommandDefunctGrace+1 {
			t.Fatalf("%+v", p)
		}
	}
	if len(tree.defunctRounds) != 5 {
		t.Fatalf("forgotten members kept: %v", tree.defunctRounds)
	}
}

// FAE-HOST-022: the grace a running command's defunct processes get is
// capped at the defunct bound. A command that leaves more children defunct
// than limits.Defunct within CommandDefunctGrace has only the first
// limits.Defunct of them left to it (in member order); the rest count
// against the bound at once, so the census raises a defunct leak. Before,
// every one of them was graced, and a command could hold up to the
// per-name process bound of zombies without an alert.
func TestAnalyzeDefunctGraceCapsAtDefunctLimit(t *testing.T) {
	members := []Process{proc(100, 1, 100, "fae", "S", 1), proc(200, 100, 200, "go", "S", 1)}
	for i := 0; i < 45; i++ {
		members = append(members, proc(300+i, 200, 200, "sh", "Z", 0)) // first-round defunct under the running command
	}
	command := members[1]
	r := Analyze(members, members, 100, DefaultLeakLimits(), command)
	if r.Defunct != 45 {
		t.Fatalf("defunct %d, want 45", r.Defunct)
	}
	if r.DefunctInCommands != 20 {
		t.Fatalf("defunct_in_commands %d, want 20", r.DefunctInCommands)
	}
	found := false
	for _, l := range r.Leaks {
		if l == (Leak{Reason: "defunct", Defunct: 25}) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no defunct leak of 25: leaks %+v", r.Leaks)
	}
}

// FAE-HOST-022, the probe's shape with real processes: a brokered command
// (its own group leader) that never collects its children holds them
// defunct. Within CommandDefunctGrace censuses the census leaves them to the
// command, up to limits.Defunct of them; from the next one they are a
// defunct leak that names the command as their parent. A command whose children are collected within the grace
// (the shell waits for them once told to) raises nothing.
func TestCensusRaisesALeaderHoldingUnreapedChildrenAfterTheGrace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie fixtures are Linux-only")
	}
	root := os.Getpid()
	var baseline Tree
	unrelated := 0
	for _, p := range members(t, &baseline, root, func([]Process) bool { return true }) {
		if p.Defunct() {
			unrelated++
		}
	}
	limits := LeakLimits{Processes: 1000, RSS: 64 << 30, Defunct: unrelated + 20}
	children := limits.Defunct + 5
	collectingChildren := 10
	start := func(script string) (*exec.Cmd, io.WriteCloser) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdin, e := cmd.StdinPipe()
		if e != nil {
			t.Fatal(e)
		}
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { stdin.Close(); _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
		return cmd, stdin
	}
	spawnChildren := func(n int) string { return "for i in $(seq " + strconv.Itoa(n) + "); do /bin/true & done; " }
	leaking, _ := start(spawnChildren(children) + "exec /bin/sleep 60")
	collecting, told := start(spawnChildren(collectingChildren) + "read line; wait; exec /bin/sleep 60")
	defunctUnder := func(ps []Process, pid int) int {
		n := 0
		for _, p := range ps {
			if p.PPID == pid && p.Defunct() {
				n++
			}
		}
		return n
	}
	// Waiting polls the census with a throwaway tree, so the rounds counted
	// below are exactly the censuses this test takes.
	await := func(want func([]Process) bool) {
		var scratch Tree
		members(t, &scratch, root, want)
	}
	var tree Tree
	census := func() ([]Process, []Process) {
		all, e := Census()
		if e != nil {
			t.Fatal(e)
		}
		return all, tree.Members(all, root)
	}
	running := func(ps []Process) (out []Process) {
		for _, p := range ps {
			if p.PID == leaking.Process.Pid || p.PID == collecting.Process.Pid {
				out = append(out, p)
			}
		}
		return out
	}
	defunctLeak := func(r LeakReport) (Leak, bool) {
		for _, l := range r.Leaks {
			if l.Reason == "defunct" {
				return l, true
			}
		}
		return Leak{}, false
	}
	await(func(ps []Process) bool {
		return defunctUnder(ps, leaking.Process.Pid) == children && defunctUnder(ps, collecting.Process.Pid) == collectingChildren
	})
	// The first census: both commands' children are within the grace, which
	// covers limits.Defunct of them; the rest stay within the bound.
	all, got := census()
	r := Analyze(all, got, root, limits, running(got)...)
	if leak, found := defunctLeak(r); found || r.DefunctInCommands != limits.Defunct {
		t.Fatalf("first census: leak %+v report %+v", leak, r)
	}
	// Told to, the collecting command waits for its children before the next census.
	if _, e := io.WriteString(told, "go\n"); e != nil {
		t.Fatal(e)
	}
	await(func(ps []Process) bool { return defunctUnder(ps, collecting.Process.Pid) == 0 })
	for round := 1; round <= CommandDefunctGrace; round++ {
		all, got = census()
		r = Analyze(all, got, root, limits, running(got)...)
		leak, found := defunctLeak(r)
		if round < CommandDefunctGrace {
			if found || r.DefunctInCommands != limits.Defunct {
				t.Fatalf("census %d, within the grace: leak %+v report %+v", round+1, leak, r)
			}
			continue
		}
		if !found || leak.Defunct < children || r.DefunctInCommands != 0 {
			t.Fatalf("census %d, after the grace: a leader holding %d unreaped children raised %+v (defunct_in_commands %d)", round+1, children, leak, r.DefunctInCommands)
		}
		var parent DefunctParent
		for _, p := range r.DefunctParents {
			if p.PID == leaking.Process.Pid {
				parent = p
			}
		}
		if parent.Command != "sleep" || parent.Defunct != children || parent.Graced != 0 || parent.Rounds != round || !parent.Running {
			t.Fatalf("the leak does not name the leader: %+v in %+v", parent, r.DefunctParents)
		}
		for _, p := range r.DefunctParents {
			if p.PID == collecting.Process.Pid {
				t.Fatalf("a command that collected its children is named: %+v", p)
			}
		}
	}
}
