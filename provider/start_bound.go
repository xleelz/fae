package provider

import (
	"os"
	"sync"
	"time"

	"github.com/xleelz/fae/processowner"
)

// DefaultStartTimeout is Appendix A's agent-start bound per attempt
// (FAE-RT-004): how long a provider gets to acknowledge the prompt. A
// Config.StartTimeout of zero uses it.
const DefaultStartTimeout = 180 * time.Second

// ProviderWaitDelay bounds how long the bridge waits, once the provider
// exited or was stopped, for its event and error streams to close: a
// descendant that escaped the provider's tree (stopProviderTree kills only
// what one census found under it) and holds them open cannot keep the
// bridge, and the turn, waiting past it. The turn is then decided from the
// provider's own exit, as it would be with the streams closed.
const ProviderWaitDelay = 10 * time.Second

// providerAcknowledgement watches the provider's event stream (stdout) for its
// acknowledgement of the prompt: the first byte it writes. Claude Code's
// stream-json and Codex's --json both start with the session the CLI opens
// (system/init, thread.started), before the model is asked anything, so any
// output acknowledges the prompt, and a provider that wrote nothing never
// took the turn.
type providerAcknowledgement struct {
	mu      sync.Mutex
	wrote   bool
	expired bool
}

func (a *providerAcknowledgement) Write(p []byte) (int, error) {
	if len(p) > 0 {
		a.mu.Lock()
		a.wrote = true
		a.mu.Unlock()
	}
	return len(p), nil
}

// acknowledged reports whether the provider wrote anything to its event
// stream.
func (a *providerAcknowledgement) acknowledged() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wrote
}

// expire is the start bound firing. It reports true when the provider has not
// acknowledged the prompt by then, and records that, so the bound's decision
// is taken once: output the provider writes while it is being stopped does
// not turn the stop into a failure of a provider that acknowledged.
func (a *providerAcknowledgement) expire() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.wrote {
		a.expired = true
	}
	return a.expired
}

// stoppedUnacknowledged reports whether the start bound stopped the provider
// before it acknowledged the prompt.
func (a *providerAcknowledgement) stoppedUnacknowledged() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expired
}

// stopProviderTree stops a provider the bridge gives up on (the start bound,
// the bridge's timeout or its cancellation): it kills the provider and every
// descendant that one process census, taken before the first signal, finds
// under it (the Codex npm launcher, for one, runs the CLI as its child). A
// descendant is signalled through a process handle (a pidfd on Linux) only
// while its start identity still matches the census, so a recycled PID is
// never signalled. A process that left the tree before the census stays in
// the bridge's process group, which the runtime retires when the bridge
// exits. Only the provider's own kill is reported: its exit status decides
// the turn.
func stopProviderTree(provider *os.Process) error {
	all, censusErr := processowner.Census()
	err := provider.Kill()
	if censusErr != nil {
		return err
	}
	children := map[int][]processowner.Process{}
	for _, p := range all {
		children[p.PPID] = append(children[p.PPID], p)
	}
	seen := map[int]bool{provider.Pid: true}
	queue := []int{provider.Pid}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if seen[child.PID] {
				continue
			}
			seen[child.PID] = true
			queue = append(queue, child.PID)
			handle, e := os.FindProcess(child.PID)
			if e != nil {
				continue
			}
			if now, e := processowner.CensusOf([]int{child.PID}); e == nil && len(now) == 1 && now[0].Start == child.Start {
				_ = handle.Kill()
			}
			_ = handle.Release()
		}
	}
	return err
}

// providerAnswered reports whether the provider left an answer file (Codex's
// --output-last-message; the Claude bridge writes its answer only later, from
// the stream). A file the bridge cannot check counts as an answer, so it never
// proves a non-execution.
func providerAnswered(answer string) bool {
	_, e := os.Lstat(answer)
	return !os.IsNotExist(e)
}
