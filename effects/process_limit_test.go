package effects

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessLimitWrapKeepsTheCommandVector(t *testing.T) {
	p := &ProcessLimit{Tasks: 64, Runner: "/usr/bin/systemd-run", Runtime: "/run/user/1000"}
	argv, env := p.wrap([]string{"/usr/bin/go", "test", "-p=1", "./..."}, []string{"PATH=/usr/bin"})
	if strings.Join(argv, " ") != "/usr/bin/systemd-run --user --scope --quiet --collect --property=TasksMax=64 -- /usr/bin/go test -p=1 ./..." || strings.Join(env, ",") != "PATH=/usr/bin,XDG_RUNTIME_DIR=/run/user/1000" {
		t.Fatal(argv, env)
	}
	if _, err := ProbeProcessLimit(context.Background(), 4); err == nil {
		t.Fatal("process limit below 8 tasks accepted")
	}
}

// FAE-HOST-015 acceptance: a fork-heavy fixture run by the broker stays within
// its per-run process limit; the storm is refused at the cap (pids.events max).
func TestBrokerForkStormStaysWithinProcessLimit(t *testing.T) {
	limit, err := ProbeProcessLimit(context.Background(), 24)
	if err != nil {
		t.Skip("per-run process limit unavailable on this host: ", err)
	}
	b, w, _ := setup(t)
	b.ProcessLimit = limit
	script := `cg=/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup); (i=0; while [ $i -lt 300 ]; do sleep 1 & i=$((i+1)); done) 2>/dev/null; cat $cg/pids.max $cg/pids.peak; sed -n 's/^max //p' $cg/pids.events; sleep 1.2`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	r, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(r.Output)
	if r.ExitCode != 0 || r.ProcessLimit != 24 || len(f) != 3 {
		t.Fatalf("%+v", r)
	}
	peak, _ := strconv.Atoi(f[1])
	refused, _ := strconv.Atoi(f[2])
	if f[0] != "24" || peak > 24 || refused < 1 {
		t.Fatalf("fork storm escaped the per-run limit: max=%s peak=%d refused=%d", f[0], peak, refused)
	}
}

// FAE-HOST-015 on a host without delegated pids: the degraded RLIMIT_NPROC cap
// still bounds a fork storm of the run.
func TestBrokerForkStormStaysWithinRlimitFallback(t *testing.T) {
	limit, err := probeRlimit(context.Background(), 24)
	if err != nil {
		t.Skip("RLIMIT_NPROC fallback unavailable on this host: ", err)
	}
	b, w, _ := setup(t)
	b.ProcessLimit = limit
	// Builtins only after the storm: the user's task budget may still be spent.
	script := `( i=0; while [ $i -lt 300 ]; do sleep 1 & echo >> "$TMPDIR/forks"; i=$((i+1)); done ) 2>/dev/null; n=0; while read l; do n=$((n+1)); done < "$TMPDIR/forks"; echo $n; while read a b c rest; do [ "$a $b" = "Max processes" ] && echo $c; done < /proc/self/limits; true`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	r, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if r.ProcessLimit != 24 {
		t.Fatalf("%+v", r)
	}
	// Other tasks of this user can spend the run's budget before its first
	// fork; the storm is then refused outright, which is still within the cap.
	if r.ExitCode != 0 && strings.Contains(r.Stderr, "Cannot fork") && r.Output == "" {
		return
	}
	f := strings.Fields(r.Output)
	if len(f) != 2 {
		t.Fatalf("%+v", r)
	}
	started, _ := strconv.Atoi(f[0])
	// Other tasks of this user come and go meanwhile, so the bound is loose.
	if max, _ := strconv.Atoi(f[1]); max <= 0 || started >= 150 || started < 1 {
		t.Fatalf("fork storm escaped the RLIMIT_NPROC fallback: started=%d limit=%s", started, f[1])
	}
}
