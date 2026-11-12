package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

type Watchdog struct {
	CheckFilesystem                func(string) (bool, bool, error)
	RPC                            RPC
	MarkerPath, StatePath, Message string
	Paths                          []string
	Period, Stall                  time.Duration
	Output                         io.Writer
	// RearmBudget re-arms per cause within RearmWindow (defaults 3 per 1 h)
	// and FilesystemThreshold the used fraction of bytes or inodes that
	// alerts (default 0.8); Appendix A defaults, configurable.
	RearmBudget         int
	RearmWindow         time.Duration
	FilesystemThreshold float64
	// JSON writes the heartbeat as a JSON object (fae watchdog --json), so
	// every output line is JSON; Instance names the instance in it.
	JSON     bool
	Instance string
}

// MinWatchdogPeriod is the shortest period Watchdog.Run accepts. The spec
// gives the period no default (FAE-CTL-020), so a period outside the range is
// refused, not replaced. Each tick reads the daemon and writes the watchdog's
// state, and a shorter period would run them nearly back to back for the
// watchdog's whole life; tests and the dogfood use 20 ms to 100 ms.
const MinWatchdogPeriod = 10 * time.Millisecond

// Defaults of the watchdog's configurable bounds (Appendix A).
const (
	DefaultRearmBudget         = 3
	DefaultRearmWindow         = time.Hour
	DefaultFilesystemThreshold = 0.8
)

func (w Watchdog) bounds() (int, time.Duration, float64) {
	budget, window, threshold := w.RearmBudget, w.RearmWindow, w.FilesystemThreshold
	if budget <= 0 {
		budget = DefaultRearmBudget
	}
	if window <= 0 {
		window = DefaultRearmWindow
	}
	// NaN fails every comparison, so test the valid range.
	if !(threshold > 0 && threshold < 1) {
		threshold = DefaultFilesystemThreshold
	}
	return budget, window, threshold
}

type WatchdogAlert struct {
	Cause, Detail string
	At            time.Time
}
type rearmWindow struct {
	Start time.Time
	Count int
}
type watchdogState struct {
	Pending   map[string]WatchdogAlert
	Rearms    map[string]rearmWindow
	Heartbeat time.Time
}

// Run ticks every Period until ctx ends. A Period below MinWatchdogPeriod,
// zero or negative included, is refused before anything runs: it has no
// default to fall back to (FAE-CTL-020).
func (w Watchdog) Run(ctx context.Context) error {
	if w.Period < MinWatchdogPeriod {
		return fmt.Errorf("watchdog period must be at least %s, not %s", MinWatchdogPeriod, w.Period)
	}
	safe, e := Unsandboxed(os.Getpid())
	if e != nil || !safe {
		return errors.Join(e, errors.New("watchdog requires native unsandboxed runtime"))
	}
	if filepath.IsAbs(w.StatePath) {
		if e = WriteWatchdogIdentity(WatchdogIdentityPath(w.StatePath)); e != nil {
			return e
		}
	}
	ticker := time.NewTicker(w.Period)
	defer ticker.Stop()
	for {
		if e = w.Tick(ctx, time.Now().UTC()); e != nil {
			if ctx.Err() != nil {
				return nil // stopped mid-tick: the cancellation is the stop request
			}
			return e
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (w Watchdog) Tick(ctx context.Context, now time.Time) error {
	if w.Message == "" || !filepath.IsAbs(w.StatePath) {
		return errors.New("watchdog authorization and private state path required")
	}
	state := watchdogState{Pending: map[string]WatchdogAlert{}, Rearms: map[string]rearmWindow{}}
	if b, e := os.ReadFile(w.StatePath); e == nil {
		if e = json.Unmarshal(b, &state); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if state.Pending == nil {
		state.Pending = map[string]WatchdogAlert{}
	}
	if state.Rearms == nil {
		state.Rearms = map[string]rearmWindow{}
	}
	alert := func(cause, detail string) {
		a := WatchdogAlert{cause, detail, now}
		state.Pending[cause] = a
		if w.Output != nil {
			_ = json.NewEncoder(w.Output).Encode(a)
		}
	}
	marker, e := ReadMarker(w.MarkerPath)
	if e != nil {
		return e
	}
	h, healthErr := w.RPC(ctx, Params{Action: "health", AuthorizingMessage: w.Message})
	if healthErr == nil {
		if _, e = w.RPC(ctx, Params{Action: "watchdog-register", AuthorizingMessage: w.Message}); e != nil {
			return fmt.Errorf("watchdog grant: %w", e)
		}
	}
	if marker != nil && marker.State == "preflight-failed" {
		marker = nil
	}
	if marker != nil {
		if state.Heartbeat.IsZero() || now.Sub(state.Heartbeat) >= time.Hour {
			if w.Output != nil {
				w.heartbeat(now, marker)
			}
			state.Heartbeat = now
		}
	} else if healthErr != nil {
		alert("daemon-dead", healthErr.Error())
	} else {
		if !h.DriveAlive {
			alert("drive-exit", "drive reported stopped")
			cause := h.DriveCause
			if cause == "" {
				cause = "drive-exit"
			}
			budget, span, _ := w.bounds()
			window := state.Rearms[cause]
			if window.Start.IsZero() || now.Sub(window.Start) >= span {
				window = rearmWindow{Start: now}
			}
			if window.Count < budget {
				window.Count++
				state.Rearms[cause] = window
				if _, e = w.RPC(ctx, Params{Action: "rearm", AuthorizingMessage: w.Message, Cause: cause}); e != nil {
					alert("drive-rearm-failed", e.Error())
				}
			} else {
				alert("drive-rearm-exhausted", fmt.Sprintf("%d attempts consumed in the current %s window", budget, span))
			}
		}
		stall := w.Stall
		if stall <= 0 {
			stall = 45 * time.Minute
		}
		if h.InFlight > 0 && !h.LastTransition.IsZero() && now.Sub(h.LastTransition) >= stall {
			alert("drive-stall", "allowlisted work has no new transition")
		}
	}
	_, _, threshold := w.bounds()
	paths := w.Paths
	// FAE-CTL-020: the workspace filesystem is watched too.
	if healthErr == nil && h.Workspace != "" && !slices.Contains(paths, h.Workspace) {
		paths = append(slices.Clone(paths), h.Workspace)
	}
	for _, path := range paths {
		check := w.CheckFilesystem
		if check == nil {
			check = func(path string) (bool, bool, error) { return inspectFilesystem(path, threshold) }
		}
		bytesLow, inodesLow, e := check(path)
		if e != nil {
			alert("filesystem-query:"+path, e.Error())
			continue
		}
		if bytesLow {
			alert("filesystem-bytes:"+path, fmt.Sprintf("bytes used at or above %.0f%%", threshold*100))
		}
		if inodesLow {
			alert("filesystem-inodes:"+path, fmt.Sprintf("inodes used at or above %.0f%%", threshold*100))
		}
	}
	if healthErr == nil && marker == nil {
		for cause, a := range state.Pending {
			if _, e = w.RPC(ctx, Params{Action: "alert", AuthorizingMessage: w.Message, Cause: cause, Note: a.Detail}); e != nil {
				break
			}
			delete(state.Pending, cause)
		}
	}
	b, _ := json.Marshal(state)
	return savePrivate(w.StatePath, b)
}

// Heartbeat is the hourly maintenance line (FAE-CTL-010). Like every log
// line it carries an RFC 3339 time, the instance and the component
// (FAE-OBS-001); under --json it is one JSON object.
type Heartbeat struct {
	Time       string `json:"time"`
	Instance   string `json:"instance"`
	Component  string `json:"component"`
	Event      string `json:"event"`
	Marker     string `json:"marker"`
	State      string `json:"state"`
	NextAction string `json:"next_action"`
}

func (w Watchdog) heartbeat(now time.Time, m *Marker) {
	h := Heartbeat{now.Format(time.RFC3339), w.Instance, "watchdog", "maintenance-heartbeat", m.ID, m.State, m.NextAction}
	if w.JSON {
		_ = json.NewEncoder(w.Output).Encode(h)
		return
	}
	_, _ = fmt.Fprintf(w.Output, "%s instance=%s component=%s maintenance %s: %s; next: %s\n", h.Time, h.Instance, h.Component, h.Marker, h.State, h.NextAction)
}
func savePrivate(path string, data []byte) error {
	parent := filepath.Dir(path)
	st, e := os.Stat(parent)
	if e != nil {
		return e
	}
	actual, e := filepath.EvalSymlinks(parent)
	if e != nil || actual != parent || st.Mode().Perm() != 0700 {
		return errors.New("private physical controller state directory required")
	}
	f, e := os.CreateTemp(parent, ".controller-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	dir, e := os.Open(parent)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}

// FilesystemPressure applies the default threshold (80% used).
func FilesystemPressure(blocks, available, files, free uint64) (bool, bool) {
	return FilesystemPressureAt(blocks, available, files, free, DefaultFilesystemThreshold)
}

// FilesystemPressureAt reports bytes and inodes used at or above a fraction.
func FilesystemPressureAt(blocks, available, files, free uint64, used float64) (bool, bool) {
	limit := math.Round((1-used)*1e6) / 1e6 // 0.8 used is exactly 0.2 free
	return blocks > 0 && float64(available)/float64(blocks) <= limit, files > 0 && float64(free)/float64(files) <= limit
}
func inspectFilesystem(path string, used float64) (bool, bool, error) {
	var stat syscall.Statfs_t
	if e := syscall.Statfs(path, &stat); e != nil {
		return false, false, e
	}
	bytesLow, inodesLow := FilesystemPressureAt(stat.Blocks, stat.Bavail, stat.Files, stat.Ffree, used)
	return bytesLow, inodesLow, nil
}
