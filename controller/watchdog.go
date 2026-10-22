package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func (w Watchdog) Run(ctx context.Context) error {
	safe, e := Unsandboxed(os.Getpid())
	if e != nil || !safe {
		return errors.Join(e, errors.New("watchdog requires native unsandboxed runtime"))
	}
	period := w.Period
	if period <= 0 {
		period = time.Minute
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		if e = w.Tick(ctx, time.Now().UTC()); e != nil {
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
				_, _ = fmt.Fprintf(w.Output, "maintenance %s: %s; next: %s\n", marker.ID, marker.State, marker.NextAction)
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
			window := state.Rearms[cause]
			if window.Start.IsZero() || now.Sub(window.Start) >= time.Hour {
				window = rearmWindow{Start: now}
			}
			if window.Count < 3 {
				window.Count++
				state.Rearms[cause] = window
				if _, e = w.RPC(ctx, Params{Action: "rearm", AuthorizingMessage: w.Message, Cause: cause}); e != nil {
					alert("drive-rearm-failed", e.Error())
				}
			} else {
				alert("drive-rearm-exhausted", "three attempts consumed in current hour")
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
	for _, path := range w.Paths {
		check := w.CheckFilesystem
		if check == nil {
			check = inspectFilesystem
		}
		bytesLow, inodesLow, e := check(path)
		if e != nil {
			alert("filesystem-query:"+path, e.Error())
			continue
		}
		if bytesLow {
			alert("filesystem-bytes:"+path, "available blocks below twenty percent")
		}
		if inodesLow {
			alert("filesystem-inodes:"+path, "available inodes below twenty percent")
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

func FilesystemPressure(blocks, available, files, free uint64) (bool, bool) {
	return blocks > 0 && float64(available)/float64(blocks) <= 0.2, files > 0 && float64(free)/float64(files) <= 0.2
}
func inspectFilesystem(path string) (bool, bool, error) {
	var stat syscall.Statfs_t
	if e := syscall.Statfs(path, &stat); e != nil {
		return false, false, e
	}
	bytesLow, inodesLow := FilesystemPressure(stat.Blocks, stat.Bavail, stat.Files, stat.Ffree)
	return bytesLow, inodesLow, nil
}
