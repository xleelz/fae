package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

// heavyOpObserve and heavyOpPolicy replace host sampling and the pressure
// policy in tests; nil and the zero policy select the host and the defaults.
var (
	heavyOpObserve func(context.Context) (hostguard.Sample, error)
	heavyOpPolicy  hostguard.Policy
)

// heavyOpDefaults: the broker's maximum lease wait is an Appendix A
// configurable default (1800 s, FAE-SCH-011.5).
var heavyOpDefaults = map[string]valueRange{"max-wait": positiveDuration}

// heavyOpCommand is the hidden broker verb shims call:
// fae heavy-op run --weight N --class role|controller|operator [--ledger DIR] -- COMMAND...
// fae heavy-op run --shim-policy FILE -- SHIM ARGS... (role PATH wrappers, FAE-HOST-011)
// It exits with the command's own code, 75 when the lease or host guard refuses,
// 77 when the role shim policy refuses, or 78 when Local mode refuses a git
// push or remote change (FAE-MODE-002).
func heavyOpCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	if len(args) == 0 || args[0] != "run" {
		return usage("usage: fae heavy-op run (--weight N --class role|controller|operator [--ledger DIR] | --shim-policy FILE) [--max-wait D] -- COMMAND...")
	}
	// Common flags (--instance, --home, --json) are accepted for uniformity; the
	// lease is host-scoped, so only the ledger selects state.
	f, opts := flags("heavy-op run", c)
	h := commandshim.HeavyOp{Observe: heavyOpObserve, Policy: heavyOpPolicy, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	ledger := os.Getenv("FAE_HOST_LEDGER")
	if ledger == "" {
		ledger, _ = scheduler.DefaultHostLedger()
	}
	f.IntVar(&h.Weight, "weight", 1, "heavy-op weight units")
	f.StringVar(&h.Class, "class", "role", "priority class: role, controller or operator")
	f.StringVar(&h.Ledger, "ledger", ledger, "absolute host heavy-op ledger directory (default $FAE_HOST_LEDGER, else the host-stable ~/.fae/host-ledger)")
	f.DurationVar(&h.MaxWait, "max-wait", 1800*time.Second, "maximum lease wait before refusal (positive duration)")
	policy := f.String("shim-policy", "", "role shim policy file; weight, class and ledger come from the policy")
	fallBack := configurableFlags(f, commonDefaults, heavyOpDefaults)
	if err := f.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return &helpRequest{f}
	} else if err != nil {
		return usage("heavy-op run: %v", err)
	}
	if h.Argv = f.Args(); len(h.Argv) == 0 {
		return usage("heavy-op run requires a command after --")
	}
	fallBack()
	h.Instance = opts.instance // a shim policy's spool names its own
	explicit := false
	f.Visit(func(fl *flag.Flag) {
		explicit = explicit || fl.Name == "weight" || fl.Name == "class" || fl.Name == "ledger"
	})
	if *policy != "" && explicit {
		return usage("heavy-op run: --shim-policy derives --weight, --class and --ledger")
	}
	// An explicit run spools a hard refusal to its instance when one exists;
	// a shim policy names its owning daemon's spool itself.
	if layout, e := daemon.Paths(opts.home, opts.instance); e == nil {
		h.AlertSpool = instanceSpool(layout)
	}
	run := commandshim.RunHeavyOp
	if *policy != "" {
		run = func(ctx context.Context, h commandshim.HeavyOp) (int, error) {
			return commandshim.RunShim(ctx, *policy, h)
		}
	}
	code, err := run(ctx, h)
	if err != nil {
		return &commandError{code, err}
	}
	if code != 0 {
		return &commandError{code, errors.New("")}
	}
	return nil
}

// heavyOpHook is the provider pre-tool hook, fae heavy-op hook --shim-dir DIR:
// it reads one hook request on standard input and prints a deny decision for
// a shim bypass (FAE-HOST-011). It exits 0 with its decision on standard
// output and fails closed otherwise: a usage error, an unreadable request, a
// missing $HOME and an unwritten or discarded decision all exit 2, which a
// provider pre-tool hook treats as a block. Exit 1 would let the provider run
// the tool, so no hook error maps to it; dispatch routes the hook before its
// own $HOME check for the same reason.
func heavyOpHook(args []string, c common, homeErr error, out io.Writer) error {
	// A write to a pipe whose reader has gone would kill the hook with
	// SIGPIPE (status 141), neither 0 nor 2. With the signal delivered to a
	// channel the write fails with EPIPE instead, and the hook exits 2. It
	// stays registered until the process exits, so that main's report of the
	// failure on a closed standard error cannot kill it either.
	hookSIGPIPE.Do(func() { signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE) })
	err := preToolHook(args, c, homeErr, out)
	var help *helpRequest
	if err == nil || errors.As(err, &help) || exitCode(err) == 2 {
		return err
	}
	return blocked("%v", err)
}

var hookSIGPIPE sync.Once

// blocked is a hook failure: exit 2, which blocks the tool call.
func blocked(format string, args ...any) error {
	return &commandError{2, fmt.Errorf("heavy-op hook: tool call blocked: "+format, args...)}
}

func preToolHook(args []string, c common, homeErr error, out io.Writer) error {
	f, _ := flags("heavy-op hook", c)
	dir := f.String("shim-dir", "", "the session's absolute command-shim directory")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return &helpRequest{f}
	} else if err != nil {
		return usage("heavy-op hook: %v", err)
	}
	if !filepath.IsAbs(*dir) || f.NArg() != 0 {
		return usage("usage: fae heavy-op hook --shim-dir DIR < request.json")
	}
	if homeErr != nil {
		return blocked("the session has no home directory: %v", homeErr)
	}
	if discards(out) {
		return blocked("standard output is closed or discarded, so no decision would reach the provider")
	}
	decision, err := commandshim.PreToolDecision(os.Stdin, *dir)
	if err != nil {
		return blocked("unreadable pre-tool request: %v", err)
	}
	if _, err = out.Write(decision); err != nil {
		return blocked("decision not written: %v", err)
	}
	return nil
}

// discards reports whether out is a file that drops what is written to it:
// /dev/null, where the Go runtime reopens a standard output that was closed
// at exec, or one it cannot even stat.
func discards(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return true
	}
	null, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(info, null)
}
