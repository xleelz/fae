package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/provider"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}
type doctorEnv struct {
	isolation            func(string, string) provider.IsolationCapability
	home, instance, dolt string
	lookPath             func(string) (string, error)
	version              func(string, ...string) (string, error)
	statfs               func(string) (bytes, inodes uint64, err error)
	sandboxed            func() (bool, error)
	// goRootGap is commandshim.GoRootGap for this process's environment;
	// nil skips the check.
	goRootGap func() (goBinary, gap string)
}

var doctorGitVersion = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// doctorChecks is read-only: it probes prerequisites and the instance layout
// and starts no daemon. Every non-passing check names a remedy.
func doctorChecks(env doctorEnv) []doctorCheck {
	checks := []doctorCheck{}
	add := func(c doctorCheck) { checks = append(checks, c) }
	if env.isolation != nil {
		fact := env.isolation(env.home, env.instance)
		status := "pass"
		if !fact.Available {
			status = "fail"
		}
		checks = append(checks, doctorCheck{Name: "session-isolation", Status: status, Detail: fact.Step + ": " + fact.Reason, Remedy: fact.Remedy})
	}
	if path, err := env.lookPath("git"); err != nil {
		add(doctorCheck{"git", "fail", "git not found on PATH", "install git 2.45 or newer and put it on PATH"})
	} else if out, err := env.version(path, "--version"); err != nil {
		add(doctorCheck{"git", "fail", err.Error(), "repair the git installation at " + path})
	} else if m := doctorGitVersion.FindStringSubmatch(out); m == nil {
		add(doctorCheck{"git", "fail", "unrecognised version: " + out, "install git 2.45 or newer"})
	} else if major, _ := strconv.Atoi(m[1]); major < 2 || (major == 2 && doctorAtoi(m[2]) < 45) {
		add(doctorCheck{"git", "fail", out, "upgrade git to 2.45 or newer"})
	} else {
		add(doctorCheck{"git", "pass", path, ""})
	}
	if path, err := env.lookPath(env.dolt); err != nil {
		add(doctorCheck{"dolt", "fail", env.dolt + " not found", "install Dolt and pass fae serve --dolt PATH"})
	} else {
		add(doctorCheck{"dolt", "pass", path, ""})
	}
	for _, cli := range []struct{ name, status, remedy string }{
		{"claude", "fail", "install Claude Code (the claude CLI, the default --prophet-provider and dogfood provider) and authenticate it with an explicit CLAUDE_CONFIG_DIR"},
		{"codex", "warn", "install the codex CLI only if a workflow or --prophet-provider selects codex"},
	} {
		if path, err := env.lookPath(cli.name); err != nil {
			add(doctorCheck{"agent-cli:" + cli.name, cli.status, cli.name + " not found on PATH", cli.remedy})
		} else {
			add(doctorCheck{"agent-cli:" + cli.name, "pass", path, ""})
		}
	}
	if path, err := env.lookPath("herdr-core"); err == nil {
		add(doctorCheck{"multiplexer:herdr", "pass", path, ""})
	} else if path, err := env.lookPath("herdr"); err == nil {
		add(doctorCheck{"multiplexer:herdr", "pass", path, ""})
	} else {
		add(doctorCheck{"multiplexer:herdr", "warn", "herdr-core not found on PATH", "install herdr-core and set herdr_binary in the live configuration to use --adapter multiplexer"})
	}
	// FAE-HOST-014: brokered runs mirror the GOROOT of the go on PATH so the
	// go a Go test starts meets the run's wrappers; a go outside any GOROOT
	// gets no mirror.
	if env.goRootGap != nil {
		if gobin, gap := env.goRootGap(); gap != "" {
			add(doctorCheck{"go-toolchain:goroot", "warn", gap, "put a go that sits in its GOROOT's bin/ (or a symlink to one) first on the daemon's PATH, or set GOROOT in each HostCommand stage's environment"})
		} else if gobin != "" {
			add(doctorCheck{"go-toolchain:goroot", "pass", gobin, ""})
		}
	}
	layout, err := daemon.Paths(env.home, env.instance)
	switch {
	case env.home == "":
		add(doctorCheck{"instance-layout", "warn", "no instance home: $HOME is not defined and no --home was given", "set HOME, or pass --home PATH to every fae command"})
	case err != nil:
		add(doctorCheck{"instance-layout", "fail", err.Error(), "use an absolute canonical --home and a valid --instance name"})
	default:
		info, err := os.Lstat(layout.Root)
		if os.IsNotExist(err) {
			add(doctorCheck{"instance-layout", "warn", layout.Root + " does not exist", "start the instance once with fae serve --workspace PATH from the workspace"})
		} else if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			add(doctorCheck{"instance-layout", "fail", fmt.Sprintf("%s is not a private directory", layout.Root), "restore owner-only (0700) permissions on the instance root"})
		} else if c, err := os.Lstat(layout.Credential); err == nil && (!c.Mode().IsRegular() || c.Mode().Perm() != 0600) {
			add(doctorCheck{"instance-layout", "fail", "credential file is not a private regular file", "restore mode 0600 on " + layout.Credential})
		} else if _, err := os.Stat(layout.Binding); err != nil {
			add(doctorCheck{"instance-layout", "warn", "no workspace binding recorded", "start the instance with fae serve --workspace PATH"})
		} else {
			add(doctorCheck{"instance-layout", "pass", layout.Root, ""})
		}
	}
	if sandboxed, err := env.sandboxed(); err != nil {
		add(doctorCheck{"sandbox", "fail", err.Error(), "run fae from an unsandboxed operator shell or the supported launcher"})
	} else if sandboxed {
		add(doctorCheck{"sandbox", "fail", "this process inherits a sandbox (seccomp or no-new-privileges)", "launch fae serve from an unsandboxed operator shell or dedicated multiplexer pane, never from an agent session"})
	} else {
		add(doctorCheck{"sandbox", "pass", "no inherited sandbox", ""})
	}
	for _, fs := range []struct{ name, path string }{{"instance-home", env.home}, {"temporary", os.TempDir()}} {
		path := fs.path
		if path == "" {
			add(doctorCheck{"filesystem:" + fs.name, "warn", "no instance home to check", "set HOME, or pass --home PATH"})
			continue
		}
		for {
			if _, err := os.Stat(path); err == nil || filepath.Dir(path) == path {
				break
			}
			path = filepath.Dir(path)
		}
		bytes, inodes, err := env.statfs(path)
		status, remedy := "pass", ""
		switch {
		case err != nil:
			status, remedy = "fail", "make "+path+" readable"
		case bytes < 1<<30 || inodes < 10000:
			status, remedy = "fail", "free space and inodes on the filesystem holding "+path
		case bytes < 5<<30 || inodes < 100000:
			status, remedy = "warn", "free space and inodes on the filesystem holding "+path
		}
		detail := fmt.Sprintf("%s: %d MiB and %d inodes free", path, bytes>>20, inodes)
		if err != nil {
			detail = err.Error()
		}
		add(doctorCheck{"filesystem:" + fs.name, status, detail, remedy})
	}
	return checks
}
func doctorAtoi(s string) int { n, _ := strconv.Atoi(s); return n }

// doctorHost supplies the host probes of fae doctor; tests substitute fixed ones.
var doctorHost = func(ctx context.Context) doctorEnv {
	return doctorEnv{lookPath: exec.LookPath, isolation: func(home, instance string) provider.IsolationCapability {
		return daemon.ProbeIsolation(ctx, home, instance)
	},
		version: func(name string, args ...string) (string, error) {
			b, err := exec.CommandContext(ctx, name, args...).Output()
			return string(b), err
		},
		statfs: func(path string) (uint64, uint64, error) {
			var st syscall.Statfs_t
			if err := syscall.Statfs(path, &st); err != nil {
				return 0, 0, err
			}
			return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Ffree), nil
		},
		sandboxed: func() (bool, error) {
			free, err := controller.Unsandboxed(os.Getpid())
			return !free, err
		},
		goRootGap: func() (string, string) { return commandshim.GoRootGap(os.Environ()) }}
}

// doctorLiveConfig checks the live configuration offline (FAE-CLI-054): the
// --live-config file, else the one the recorded serve invocation names.
func doctorLiveConfig(path string, c common) doctorCheck {
	if path == "" {
		layout, err := daemon.Paths(c.home, c.instance)
		if err != nil {
			return doctorCheck{"configuration:live", "pass", "no live configuration named", ""}
		}
		var args []string
		var binding struct {
			Workspace string `json:"workspace"`
		}
		raw, err := os.ReadFile(filepath.Join(layout.Root, "serve-args.json"))
		if err != nil || json.Unmarshal(raw, &args) != nil {
			return doctorCheck{"configuration:live", "pass", "no recorded serve invocation names a live configuration", ""}
		}
		for i, a := range args {
			if a == "--live-config" && i+1 < len(args) {
				path = args[i+1]
			} else if v, ok := strings.CutPrefix(a, "--live-config="); ok {
				path = v
			}
		}
		if path == "" {
			return doctorCheck{"configuration:live", "pass", "the recorded serve invocation runs the scripted adapter", ""}
		}
		if raw, err = os.ReadFile(layout.Binding); err == nil && json.Unmarshal(raw, &binding) == nil && !filepath.IsAbs(path) {
			path = filepath.Join(binding.Workspace, path)
		}
	}
	raw, err := readSecretFile(path)
	if err != nil {
		return doctorCheck{"configuration:live", "fail", err.Error(), "keep the live configuration an owner-only (0600) file outside every repository"}
	}
	var live daemon.LiveConfig
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&live); err != nil {
		return doctorCheck{"configuration:live", "fail", path + ": " + err.Error(), "correct the live configuration's JSON fields"}
	}
	if issues := daemon.CheckLiveConfig(live); len(issues) > 0 {
		return doctorCheck{"configuration:live", "fail", path + ": " + strings.Join(issues, "; "), "correct each named live configuration issue before fae serve"}
	}
	return doctorCheck{"configuration:live", "pass", path, ""}
}

// doctorTargets validates each target configuration's latest revision
// through a running daemon; with none running the check is a warning.
func doctorTargets(ctx context.Context, c common) doctorCheck {
	var listed bytes.Buffer
	if err := rpc(ctx, c, "TargetConfigList", "", map[string]any{}, &listed); err != nil {
		if exitCode(err) == 3 {
			return doctorCheck{"configuration:targets", "warn", "daemon not running; target configurations are checked through it", "start fae serve and rerun fae doctor"}
		}
		return doctorCheck{"configuration:targets", "fail", err.Error(), "inspect the daemon with fae status"}
	}
	var revisions []struct {
		ID      string `json:"id"`
		Retired bool   `json:"retired"`
	}
	if err := json.Unmarshal(listed.Bytes(), &revisions); err != nil {
		return doctorCheck{"configuration:targets", "fail", err.Error(), "inspect fae template target-config-list"}
	}
	targets, seen := revisions[:0], map[string]bool{}
	for _, r := range revisions {
		if !r.Retired && !seen[r.ID] {
			seen[r.ID] = true
			targets = append(targets, r)
		}
	}
	failed := []string{}
	for _, t := range targets {
		var got, verdict bytes.Buffer
		var doc struct {
			Document json.RawMessage `json:"document"`
		}
		err := rpc(ctx, c, "TargetConfigGet", "", map[string]any{"id": t.ID}, &got)
		if err == nil {
			err = json.Unmarshal(got.Bytes(), &doc)
		}
		if err == nil {
			err = rpc(ctx, c, "TargetConfigValidate", "", map[string]any{"document": doc.Document}, &verdict)
		}
		var v struct {
			Valid  bool     `json:"valid"`
			Errors []string `json:"errors"`
		}
		if err == nil {
			err = json.Unmarshal(verdict.Bytes(), &v)
		}
		if err != nil {
			failed = append(failed, t.ID+": "+err.Error())
		} else if !v.Valid {
			failed = append(failed, t.ID+": "+strings.Join(v.Errors, "; "))
		}
	}
	if len(failed) > 0 {
		return doctorCheck{"configuration:targets", "fail", strings.Join(failed, " | "), "put a corrected revision with fae template target-config-put, or retire the target"}
	}
	return doctorCheck{"configuration:targets", "pass", fmt.Sprintf("%d target configuration(s) valid", len(targets)), ""}
}

func doctorCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	dolt := f.String("dolt", "dolt", "Dolt executable")
	liveConfig := f.String("live-config", "", "live configuration to check (default the one the recorded serve invocation names)")
	if err := parse(f, args); err != nil {
		return err
	}
	env := doctorHost(ctx)
	env.home, env.instance, env.dolt = opts.home, opts.instance, *dolt
	checks := append(doctorChecks(env), doctorLiveConfig(*liveConfig, *opts))
	if opts.home == "" {
		// Without a home there is no instance, so no daemon to ask.
		checks = append(checks, doctorCheck{"configuration:targets", "warn", "no instance home; target configurations are checked through a running daemon", "set HOME, or pass --home PATH, and rerun fae doctor"})
	} else {
		checks = append(checks, doctorTargets(ctx, *opts))
	}
	ok := true
	for _, check := range checks {
		ok = ok && check.Status != "fail"
	}
	if err := jsonOut(out, map[string]any{"ok": ok, "checks": checks}); err != nil {
		return err
	}
	if !ok {
		return errors.New("doctor found failing checks")
	}
	return nil
}
