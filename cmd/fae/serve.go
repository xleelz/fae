package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/ui"
)

// startDaemon starts a foreground serve's daemon; tests substitute it to see
// the configuration serve resolved.
var startDaemon = daemon.Start

// serveDefaults are the serve flags that carry a configurable default
// (Appendix A, and FAE-CLI-010's adapter, disposition and UI bind), with the
// range each accepts. An adapter, disposition or bind falls back only when
// empty: any other value is checked as before, so an unknown adapter or mode
// stays a usage error and a non-loopback bind a refusal (FAE-API-030).
// --ui-port has no default in the spec, so an out-of-range port stays a usage
// error; the pane-limit floor (FAE-SCH-012) is a refusal by the daemon, not a
// range.
var serveDefaults = map[string]valueRange{
	"adapter":                nonEmpty,
	"default-mode":           nonEmpty,
	"ui-bind":                nonEmpty,
	"run-id":                 {"a nonempty run ID", func(v any) bool { return v.(string) != "" }},
	"dolt-port":              {"1024..65535, or 0 to derive it from the data directory", func(v any) bool { p := v.(int); return p == 0 || p >= 1024 && p <= 65535 }},
	"dolt-stop-wait":         positiveDuration,
	"event-retention-age":    positiveDuration,
	"event-retention-count":  positiveCount,
	"message-max-age":        positiveDuration,
	"max-tasks-in-flight":    unboundedLimit,
	"recursion-ceiling":      {"1..64", func(v any) bool { n := v.(int); return n >= 1 && n <= 64 }},
	"max-molecules":          unboundedLimit,
	"max-workflow-molecules": unboundedLimit,
	"max-panes":              zeroKeepsLive("pane limit, else 60"),
	"max-heavy-weight":       zeroKeepsLive("heavy capacity, else 2"),
	"rearm-budget":           positiveCount,
	"rearm-window":           positiveDuration,
	"inline-limit":           {fmt.Sprintf("%d..%d bytes", controlplane.MinInlineLimit, controlplane.DefaultInlineLimit), func(v any) bool { return controlplane.ValidInlineLimit(v.(int)) }},
	"prophet-max-failovers":  {"-1 (none), 0 (one) or 1..7", func(v any) bool { n := v.(int); return n >= -1 && n <= 7 }},
}

// applyServeLimits lets serve flags override the live configuration; heavy
// capacity defaults to Appendix A's 2 weight units.
func applyServeLimits(live *daemon.LiveConfig, maxPanes, maxHeavy int) {
	if maxPanes > 0 {
		live.PaneLimit = maxPanes
	}
	if maxHeavy > 0 {
		live.HeavyCapacity = maxHeavy
	} else if live.HeavyCapacity == 0 {
		live.HeavyCapacity = 2
	}
}

// addDoltRestartAttemptsFlag registers --dolt-restart-attempts on fae serve
// (FAE-CP-006: default 0 uses daemon.Config's default of 3).
func addDoltRestartAttemptsFlag(f *flag.FlagSet) *int {
	return f.Int("dolt-restart-attempts", 0, "bounded Dolt restarts per outage (0 uses the default of 3)")
}

func init() {
	for i := range commandList {
		if commandList[i][0] == "serve" && !strings.Contains(commandList[i][1], "--dolt-restart-attempts") {
			commandList[i][1] = strings.Replace(commandList[i][1], "[--dolt-stop-wait D]", "[--dolt-stop-wait D] [--dolt-restart-attempts N] [--tui] [--tui-bin PATH] [--tui-allow-writes] [--tui-refresh D] [--workflow-md PATH] [--workflow PATH] [--symphony-http-addr ADDR]", 1)
		}
	}
}

// validateDoltRestartAttempts rejects a negative --dolt-restart-attempts with usage exit 2.
func validateDoltRestartAttempts(n int) error {
	if n < 0 {
		return usage("--dolt-restart-attempts must be nonnegative (0 uses the default of 3)")
	}
	return nil
}

// recordedServeArgs is serveArgs without the one-shot flags.
func recordedServeArgs(serveArgs []string) []string {
	return daemon.RelaunchArgs(serveArgs)
}

// recordServe keeps the serve arguments in the private instance root so that
// run --start can restart the same daemon (FAE-CLI-003), without the
// one-shot flags.
func recordServe(c *common, serveArgs []string) error {
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(recordedServeArgs(serveArgs))
	tmp := filepath.Join(layout.Root, ".serve-args.json.tmp")
	if err = os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(layout.Root, "serve-args.json"))
}

// startRecorded restarts a stopped instance from its recorded serve arguments,
// only from the bound workspace.
func startRecorded(ctx context.Context, c *common) error {
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		return err
	}
	var binding struct {
		Workspace string `json:"workspace"`
	}
	raw, err := os.ReadFile(layout.Binding)
	if err != nil {
		return unreachable(fmt.Errorf("no bound workspace to start from: %w", err))
	}
	if err = json.Unmarshal(raw, &binding); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	bound, e1 := filepath.EvalSymlinks(binding.Workspace)
	here, e2 := filepath.EvalSymlinks(cwd)
	if e1 != nil || e2 != nil || bound != here {
		return fmt.Errorf("run --start must be invoked from the bound workspace %s", binding.Workspace)
	}
	var args []string
	if raw, err = os.ReadFile(filepath.Join(layout.Root, "serve-args.json")); err != nil {
		return unreachable(fmt.Errorf("no recorded serve invocation; start with fae serve: %w", err))
	}
	if err = json.Unmarshal(raw, &args); err != nil {
		return err
	}
	return detachServe(ctx, c, args, io.Discard)
}

// serveReadyWait bounds how long a detaching serve waits for the start line.
var serveReadyWait = 3 * time.Minute

// detachServe starts the same serve invocation with --foreground in its own
// session, waits for its start line and exits, leaving the daemon running.
// The daemon's output goes to HOME/INSTANCE.serve.log; a launcher relaunch
// reuses the child's own arguments, which already select --foreground.
func detachServe(ctx context.Context, c *common, serveArgs []string, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(c.home, 0700); err != nil {
		return err
	}
	logPath := filepath.Join(c.home, c.instance+".serve.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	args := append([]string{"--home", c.home, "--instance", c.instance, "serve"}, serveArgs...)
	cmd := exec.Command(exe, append(args, "--foreground")...)
	cmd.Stdout, cmd.Stderr = log, log
	// The daemon rotates this file like its supervision log (FAE-OBS-001).
	cmd.Env = append(os.Environ(), daemon.ServeLogEnv+"="+logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.NewTimer(serveReadyWait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if line, ok := startLine(logPath); ok {
			if _, err = out.Write(line); err != nil {
				return err
			}
			return nil
		}
		select {
		case e := <-exited:
			if line, ok := startLine(logPath); ok {
				_, err = out.Write(line)
				return errors.Join(err, fmt.Errorf("daemon exited after start: %v", e))
			}
			raw, _ := os.ReadFile(logPath)
			return fmt.Errorf("daemon exited before ready (%v): %s", e, bytes.TrimSpace(raw))
		case <-deadline.C:
			return fmt.Errorf("daemon not ready after %s; see %s (pid %d)", serveReadyWait, logPath, cmd.Process.Pid)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// startLine returns the first complete JSON object reporting readiness. The
// log also carries timestamped supervision lines, so text outside an object is
// skipped.
func startLine(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var buf bytes.Buffer
	for {
		chunk, err := r.ReadBytes('\n')
		if err != nil {
			return nil, false
		}
		if buf.Len() == 0 && !bytes.HasPrefix(chunk, []byte("{")) {
			continue
		}
		buf.Write(chunk)
		var probe struct {
			Ready bool `json:"ready"`
		}
		// The start line is indented JSON; decode once the object is complete.
		if json.Valid(buf.Bytes()) {
			if json.Unmarshal(buf.Bytes(), &probe) == nil && probe.Ready {
				return buf.Bytes(), true
			}
			buf.Reset()
		}
	}
}

// uiWarnings receives the serve-time UI warnings; tests capture it.
var uiWarnings io.Writer = os.Stderr

// uiFlags are the serve UI child flags (FAE-CLI-010, FAE-API-033).
type uiFlags struct {
	enabled, writes         *bool
	bin, dist, bind, rpcLog *string
	port                    *int
}

func addUIFlags(f *flag.FlagSet) uiFlags {
	return uiFlags{
		enabled: f.Bool("ui", false, "supervise the optional loopback UI child"),
		bin:     f.String("ui-bin", "", "UI child executable (default fae-ui beside this fae binary)"),
		dist:    f.String("ui-dist", "", "UI static assets directory (default the UI child's own assets)"),
		port:    f.Int("ui-port", 0, "UI port 1024..65535 (0 derives a stable per-instance port)"),
		bind:    f.String("ui-bind", ui.DefaultBind, "UI bind address; only a literal loopback address is accepted"),
		writes:  f.Bool("ui-allow-writes", false, "give the UI a write capability (writes still cite an authorizing message)"),
		rpcLog:  f.String("ui-rpc-log", "", "file where the UI child logs its daemon RPCs"),
	}
}

// config validates the UI flags before anything starts: they require --ui, a
// non-loopback bind is refused naming the value (FAE-API-030), and paths are
// made absolute against the current directory. The UI is a separate binary
// (FAE-GEN-010 non-goal 6); a missing one is a warning, never a refusal
// (FAE-API-033).
func (u uiFlags) config(f *flag.FlagSet) (*daemon.UIConfig, error) {
	set := false
	f.Visit(func(x *flag.Flag) { set = set || strings.HasPrefix(x.Name, "ui-") })
	if !*u.enabled {
		if set {
			return nil, usage("--ui-bin, --ui-dist, --ui-port, --ui-bind, --ui-allow-writes and --ui-rpc-log require --ui")
		}
		return nil, nil
	}
	if *u.port != 0 && (*u.port < 1024 || *u.port > 65535) {
		return nil, usage("--ui-port must be 1024..65535, or 0 to derive one")
	}
	bind, err := ui.ValidateBind(*u.bind)
	if err != nil {
		return nil, err
	}
	c := &daemon.UIConfig{Bind: bind, Port: *u.port, AllowWrites: *u.writes}
	for _, p := range []struct {
		from string
		to   *string
	}{{*u.bin, &c.Binary}, {*u.dist, &c.Dist}, {*u.rpcLog, &c.RPCLog}} {
		if p.from != "" {
			if *p.to, err = filepath.Abs(p.from); err != nil {
				return nil, err
			}
		}
	}
	binary := c.Binary
	if exe, err := os.Executable(); binary == "" && err == nil {
		binary = daemon.DefaultUIBinary(exe)
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		warnLine(uiWarnings, flagInstance(f), fmt.Sprintf("fae %s: UI child binary %s is missing or not executable; the daemon starts without the UI (install fae-ui beside fae or pass --ui-bin)", f.Name(), binary))
	}
	return c, nil
}

// tuiFlags are the serve TUI child flags.
type tuiFlags struct {
	enabled, writes *bool
	bin             *string
	refresh         *time.Duration
}

func addTUIFlags(f *flag.FlagSet) tuiFlags {
	return tuiFlags{
		enabled: f.Bool("tui", true, "supervise the terminal UI child (fae-tui) while the daemon is running"),
		bin:     f.String("tui-bin", "", "TUI child executable (default fae-tui beside this fae binary)"),
		writes:  f.Bool("tui-allow-writes", false, "give the supervised TUI write mode (--write)"),
		refresh: f.Duration("tui-refresh", 2*time.Second, "base polling interval for the supervised TUI child (250ms..10m)"),
	}
}

func (t tuiFlags) config(f *flag.FlagSet) (*daemon.TUIConfig, error) {
	subSet := false
	explicitTUI := false
	f.Visit(func(x *flag.Flag) {
		subSet = subSet || strings.HasPrefix(x.Name, "tui-")
		explicitTUI = explicitTUI || x.Name == "tui" || x.Name == "tui-bin"
	})
	if !*t.enabled {
		if subSet {
			return nil, usage("--tui-bin, --tui-allow-writes and --tui-refresh require --tui")
		}
		return nil, nil
	}
	if *t.refresh < 250*time.Millisecond || *t.refresh > 10*time.Minute {
		return nil, usage("--tui-refresh must be 250ms..10m")
	}
	c := &daemon.TUIConfig{AllowWrites: *t.writes, Refresh: *t.refresh}
	if *t.bin != "" {
		abs, err := filepath.Abs(*t.bin)
		if err != nil {
			return nil, err
		}
		c.Binary = abs
	}
	binary := c.Binary
	if exe, err := os.Executable(); binary == "" && err == nil {
		binary = daemon.DefaultTUIBinary(exe)
	}
	if explicitTUI {
		if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			warnLine(uiWarnings, flagInstance(f), fmt.Sprintf("fae %s: TUI child binary %s is missing or not executable; the daemon starts without the TUI (install fae-tui beside fae or pass --tui-bin)", f.Name(), binary))
		}
	}
	return c, nil
}

// symphonyFlags are the serve Symphony WORKFLOW.md & HTTP listener flags.
type symphonyFlags struct {
	workflowMD *string
	workflow   *string
	httpAddr   *string
}

func addSymphonyFlags(f *flag.FlagSet) symphonyFlags {
	return symphonyFlags{
		workflowMD: f.String("workflow-md", "", "WORKFLOW.md path to attach the Symphony LinearSync runner to the daemon"),
		workflow:   f.String("workflow", "", "WORKFLOW.md path (alias of --workflow-md)"),
		httpAddr:   f.String("symphony-http-addr", "", "optional host:port override for the Symphony HTTP listener (requires --workflow-md)"),
	}
}

func (s symphonyFlags) config(f *flag.FlagSet) (*daemon.SymphonyConfig, error) {
	wfVisited := false
	addrVisited := false
	f.Visit(func(x *flag.Flag) {
		switch x.Name {
		case "workflow-md", "workflow":
			wfVisited = true
		case "symphony-http-addr":
			addrVisited = true
		}
	})
	rawMD := strings.TrimSpace(*s.workflowMD)
	rawWF := strings.TrimSpace(*s.workflow)
	if rawMD != "" && rawWF != "" && rawMD != rawWF {
		return nil, usage("--workflow-md and --workflow must not specify different paths")
	}
	wfRaw := rawMD
	if wfRaw == "" {
		wfRaw = rawWF
	}
	addrRaw := strings.TrimSpace(*s.httpAddr)
	if wfVisited && wfRaw == "" {
		return nil, usage("--workflow-md requires a non-empty path")
	}
	if addrVisited && addrRaw == "" {
		return nil, usage("--symphony-http-addr requires a host:port value")
	}
	if addrRaw != "" && wfRaw == "" {
		return nil, usage("--symphony-http-addr requires --workflow-md")
	}
	if addrRaw != "" {
		if _, _, err := daemon.ParseSymphonyHTTPAddr(addrRaw); err != nil {
			return nil, usage("%v", err)
		}
	}
	if wfRaw == "" {
		return nil, nil
	}
	return daemon.ResolveSymphonyConfig(wfRaw, addrRaw)
}

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0 && f.Name() != os.DevNull
}

func interactiveTTYPath(in *os.File, out io.Writer) string {
	outFile, ok := out.(*os.File)
	if !ok || !isTerminalFile(in) || !isTerminalFile(outFile) {
		return ""
	}
	if target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", outFile.Fd())); err == nil && filepath.IsAbs(target) && strings.HasPrefix(target, "/dev/") {
		if info, err := os.Stat(target); err == nil && (info.Mode()&os.ModeCharDevice) != 0 {
			return filepath.Clean(target)
		}
	}
	return ""
}

// uiCommand: fae ui token prints the UI token, base URL and one-time login URL
// for the instance operator (FAE-API-031). It is read from the daemon, which
// never logs it.
func uiCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 || args[0] != "token" {
		return usage("usage: fae ui token")
	}
	f, opts := flags("ui token", c)
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	return rpc(ctx, *opts, "UIToken", "", map[string]any{}, out)
}
