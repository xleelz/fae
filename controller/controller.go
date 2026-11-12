// Package controller implements lifecycle operations through authenticated daemon
// RPC and a separately started, positively unsandboxed runtime launcher.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/processowner"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LaunchSpec pins a daemon launch. A recorded spec names its environment only
// by EnvironmentDigest (see SealEnvironment); values travel to the launcher.
type LaunchSpec struct {
	Binary            string   `json:"binary"`
	Arguments         []string `json:"arguments"`
	Directory         string   `json:"directory"`
	Environment       []string `json:"environment,omitempty"`
	EnvironmentDigest string   `json:"environment_digest,omitempty"`
	SHA256            string   `json:"sha256"`
}
type Marker struct {
	OldBirth                                      string
	Replacement                                   *LaunchSpec
	GatedRef                                      string
	DeployedTip, IntegrationRef                   string
	Holds                                         []OverlapHold
	ID, Owner, Reason, Message, State, NextAction string
	Started                                       time.Time
	Launch                                        LaunchSpec
	OldPID                                        int
	Launcher                                      string
	AckOverlap                                    bool
}
type Health struct {
	BinaryHash, ProcessBirth string
	Workspace                string
	HostLedger               string
	Holds                    []OverlapHold
	PID, DoltPID             int
	OldPID                   int
	DriveAlive               bool
	DriveGeneration          uint64
	DriveCause               string
	DriveRounds              uint64
	LastTransition           time.Time
	InFlight                 int
	Maintenance              *Marker
	EnvironmentHash          string
	Consistent               bool
	Unsafe                   []string
	QA                       []string
	Overlap                  []string
	Watchdog                 string `json:",omitempty"`
	// The replaced daemon's binary and both builds' source versions, reported
	// by a roll (FAE-CTL-001 outputs).
	OldBinaryHash, OldVersion, NewVersion string `json:",omitempty"`
}
type Params struct {
	GrantMessage       string        `json:"grant_message,omitempty"`
	Action             string        `json:"action"`
	AuthorizingMessage string        `json:"authorizing_message"`
	Note               string        `json:"note,omitempty"`
	Marker             *Marker       `json:"marker,omitempty"`
	Binary             string        `json:"binary,omitempty"`
	GatedRef           string        `json:"gated_ref,omitempty"`
	AckOverlap         bool          `json:"ack_overlap,omitempty"`
	WhenClear          bool          `json:"when_clear,omitempty"`
	Cause              string        `json:"cause,omitempty"`
	OverlapWait        time.Duration `json:"overlap_wait,omitempty"`
}
type RPC func(context.Context, Params) (Health, error)
type Options struct {
	Message, Note, Launcher, Binary, GatedRef, BuildRef string
	OverlapWait                                         time.Duration
	MaxWait                                             time.Duration
	AckOverlap, WhenClear                               bool
}
type Client struct {
	RPC        RPC
	MarkerPath string
	Poll       time.Duration
	Log        io.Writer
	// WatchdogIdentity, when set, makes the running watchdog's sandbox state
	// part of post-start acceptance.
	WatchdogIdentity string
}

func HashFile(path string) (string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func EnvironmentHash(env []string) string {
	b, _ := json.Marshal(env)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func ReadMarker(path string) (*Marker, error) {
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > 1<<20 {
		return nil, errors.New("invalid private maintenance marker")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	var m Marker
	if e = json.Unmarshal(b, &m); e != nil {
		return nil, e
	}
	if m.ID == "" || m.Message == "" {
		return nil, errors.New("incomplete maintenance authorization")
	}
	return &m, nil
}
func (c Client) pause(ctx context.Context) error {
	delay := c.Poll
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}
func (c Client) Status(ctx context.Context) (*Marker, error) { return ReadMarker(c.MarkerPath) }
func (c Client) Stop(ctx context.Context, o Options) (*Marker, error) {
	if o.Message == "" || o.Note == "" || !filepath.IsAbs(o.Launcher) {
		return nil, errors.New("message, note and absolute launcher socket required")
	}
	max := o.MaxWait
	if max <= 0 {
		max = 300 * time.Second
	}
	overlapWait := o.OverlapWait
	if overlapWait == 0 {
		overlapWait = 2 * time.Hour
	}
	if overlapWait < 0 {
		return nil, errors.New("overlap wait must be positive")
	}
	commitDeadline := time.Now().Add(max)
	ctx, cancel := context.WithTimeout(ctx, max+overlapWait)
	defer cancel()
	var h Health
	for {
		var e error
		h, e = c.RPC(ctx, Params{Action: "prepare", AuthorizingMessage: o.Message, Note: o.Note, Binary: o.Binary, GatedRef: o.GatedRef, AckOverlap: o.AckOverlap, WhenClear: o.WhenClear, OverlapWait: o.OverlapWait, Marker: &Marker{Launcher: o.Launcher}})
		if e != nil {
			return nil, c.preparationFailed(o, e)
		}
		if h.Maintenance != nil {
			break
		}
		if (len(h.Unsafe) > 0 || o.WhenClear && len(h.QA) > 0) && time.Now().After(commitDeadline) {
			return nil, c.preparationFailed(o, errors.New("commit/QA window wait expired"))
		}
		if e = c.pause(ctx); e != nil {
			return nil, c.preparationFailed(o, fmt.Errorf("commit/overlap hold expired: %w", e))
		}
	}
	marker := h.Maintenance
	for {
		h, e := c.RPC(ctx, Params{Action: "health", AuthorizingMessage: o.Message})
		if e != nil {
			return marker, e
		}
		if h.InFlight == 0 {
			break
		}
		if e = c.pause(ctx); e != nil {
			return marker, c.abort(context.Background(), o.Message, "drain-timeout", "inspect active work; retry graceful stop", e)
		}
	}
	if _, e := c.RPC(ctx, Params{Action: "stop", AuthorizingMessage: o.Message, Marker: marker}); e != nil {
		return marker, c.abort(context.Background(), o.Message, "stop-failed", "inspect daemon and retry graceful stop", e)
	}
	for {
		_, e := c.RPC(ctx, Params{Action: "health", AuthorizingMessage: o.Message})
		if e != nil && processowner.ProvedDead(marker.OldPID, marker.OldBirth) {
			return marker, nil
		}
		if e = c.pause(ctx); e != nil {
			return marker, c.abort(context.Background(), o.Message, "stop-timeout", "daemon remains alive; inspect graceful shutdown", e)
		}
	}
}
func (c Client) abort(ctx context.Context, message, state, next string, cause error) error {
	_, e := c.RPC(ctx, Params{Action: "abort", AuthorizingMessage: message, Note: state + ": " + next + ": " + cause.Error()})
	return errors.Join(cause, e)
}
func (c Client) Start(ctx context.Context, o Options) (Health, error) {
	max := o.MaxWait
	if max <= 0 {
		max = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, max)
	defer cancel()
	marker, e := ReadMarker(c.MarkerPath)
	if e != nil {
		return Health{}, e
	}
	if marker == nil {
		return Health{}, errors.New("authorized maintenance marker required")
	}
	if o.Message != marker.Message {
		return Health{}, errors.New("offline start must cite the maintenance authorization message")
	}
	if (marker.State != "stopped" && marker.State != "failed") || !processowner.ProvedDead(marker.OldPID, marker.OldBirth) {
		return Health{}, errors.New("authorized old daemon is not proven stopped; refusing overlapping start")
	}
	spec := marker.Launch
	if o.Binary != "" && o.Binary != spec.Binary {
		if marker.Replacement == nil || o.Binary != marker.Replacement.Binary {
			return Health{}, errors.New("replacement binary was not authorized before maintenance")
		}
		spec = *marker.Replacement
	}
	launcher := marker.Launcher
	if o.Launcher != "" && o.Launcher != launcher {
		return Health{}, errors.New("launcher differs from authorized maintenance marker")
	}
	if spec, e = ResolveEnvironment(filepath.Dir(c.MarkerPath), spec); e != nil {
		return Health{}, fmt.Errorf("authorized launch environment unavailable: %w", e)
	}
	launchedPID, e := Launch(ctx, launcher, spec)
	if e != nil {
		failed := fmt.Errorf("start failed; maintenance marker retained; retry using previous binary: %w", e)
		// The daemon is down, so no RPC can record this: the marker names the
		// state and the next action, and the error goes to standard error.
		return Health{}, errors.Join(failed, markStartFailed(c.MarkerPath, failed))
	}
	launchedBirth, e := processowner.Birth(launchedPID)
	if e != nil {
		return Health{PID: launchedPID}, e
	}
	accept := ctx
	for {
		h, e := c.RPC(accept, Params{Action: "health", AuthorizingMessage: o.Message})
		if e == nil && h.PID == launchedPID && h.ProcessBirth == launchedBirth && h.BinaryHash == spec.SHA256 && h.DoltPID > 1 && h.DriveAlive && h.DriveRounds > 0 && h.Consistent && h.EnvironmentHash == EnvironmentHash(spec.Environment) {
			safe, probe := Unsandboxed(h.PID)
			childSafe, childProbe := Unsandboxed(h.DoltPID)
			if probe != nil || childProbe != nil || !safe || !childSafe {
				return h, c.abort(ctx, o.Message, "acceptance-failed", "restore previous binary", errors.Join(probe, childProbe, errors.New("sandbox acceptance refused")))
			}
			watchdog, watchErr := watchdogAcceptance(c.WatchdogIdentity)
			if watchErr != nil {
				return h, c.abort(ctx, o.Message, "acceptance-failed", "restore previous binary; restart the watchdog unsandboxed", watchErr)
			}
			accepted, acceptErr := c.RPC(ctx, Params{Action: "accept", AuthorizingMessage: o.Message, Marker: &Marker{ID: marker.ID}})
			if acceptErr != nil {
				return h, acceptErr
			}
			accepted.Watchdog = watchdog
			return accepted, nil
		}
		if e = c.pause(accept); e != nil {
			return Health{PID: launchedPID, ProcessBirth: launchedBirth}, c.abort(ctx, o.Message, "acceptance-timeout", "restore previous binary", e)
		}
	}
}
func (c Client) Roll(ctx context.Context, o Options) (Health, error) {
	bound := o.MaxWait
	if bound <= 0 {
		bound = 300 * time.Second
	}
	holdBound := o.OverlapWait
	if holdBound == 0 {
		holdBound = 2 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, 3*bound+holdBound)
	defer cancel()
	if o.BuildRef != "" {
		if o.Binary != "" {
			return Health{}, errors.New("binary and build-ref are exclusive")
		}
		h, e := c.RPC(ctx, Params{Action: "health", AuthorizingMessage: o.Message})
		if e != nil {
			return Health{}, c.preparationFailed(o, e)
		}
		deadline := time.Now().Add(bound)
		for o.WhenClear && (len(h.QA) > 0 || len(h.Unsafe) > 0) {
			if time.Now().After(deadline) {
				return Health{}, c.preparationFailed(o, errors.New("commit/QA window wait expired before build"))
			}
			if e = c.pause(ctx); e != nil {
				return Health{}, c.preparationFailed(o, e)
			}
			h, e = c.RPC(ctx, Params{Action: "health", AuthorizingMessage: o.Message})
			if e != nil {
				return Health{}, c.preparationFailed(o, e)
			}
		}
		if o.GatedRef == "" {
			o.GatedRef = o.BuildRef
		}
		binary, _, _, e := BuildGated(ctx, BuildLease{Ledger: h.HostLedger, Log: c.Log}, h.Workspace, o.BuildRef, o.GatedRef, filepath.Join(filepath.Dir(c.MarkerPath), "deployments"))
		if e != nil {
			return Health{}, c.preparationFailed(o, e)
		}
		// Keep deployed executable for future rollback and daemon restarts.
		o.Binary = binary
	}
	if o.Binary == "" {
		return Health{}, errors.New("roll requires binary or build-ref")
	}
	marker, e := c.Stop(ctx, o)
	if e != nil {
		return Health{}, e
	}
	h, e := c.Start(ctx, o)
	if e == nil {
		h.OldPID, h.Holds = marker.OldPID, marker.Holds
		h.OldBinaryHash, h.OldVersion, h.NewVersion = marker.Launch.SHA256, binaryVersion(marker.Launch.Binary), binaryVersion(o.Binary)
		return h, nil
	}
	rollback := o
	rollback.Binary = marker.Launch.Binary
	if h.PID > 1 && !processowner.ProvedDead(h.PID, h.ProcessBirth) {
		_, stopErr := c.RPC(ctx, Params{Action: "stop", AuthorizingMessage: o.Message, Marker: marker})
		if stopErr != nil {
			return h, c.abort(context.Background(), o.Message, "roll-rollback-failed", "rollback needs operator intervention", errors.Join(e, fmt.Errorf("rollback needs operator intervention: %w", stopErr)))
		}
		for !processowner.ProvedDead(h.PID, h.ProcessBirth) {
			if pause := c.pause(ctx); pause != nil {
				return h, c.abort(context.Background(), o.Message, "roll-rollback-failed", "rollback needs operator intervention", errors.Join(e, pause))
			}
		}
	}
	restored, re := c.Start(ctx, rollback)
	if re == nil {
		return restored, c.preparationFailed(o, fmt.Errorf("deployment failed; previous binary restored: %w", e))
	}
	return restored, c.abort(context.Background(), o.Message, "roll-rollback-failed", "restore previous binary", errors.Join(e, re))
}

func (c Client) preparationFailed(o Options, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, e := c.RPC(ctx, Params{Action: "preflight-failed", AuthorizingMessage: o.Message, Note: cause.Error(), Marker: &Marker{Launcher: o.Launcher}})
	return errors.Join(cause, e)
}

// binaryVersion is a build's source revision, or why it has none.
func binaryVersion(binary string) string {
	revision, err := SourceRevision(binary)
	if err != nil {
		return "unknown: " + err.Error()
	}
	return revision
}

// markStartFailed records a failed start in the private maintenance marker:
// state failed (a start may be retried) and the next action naming the cause.
func markStartFailed(path string, cause error) error {
	m, err := ReadMarker(path)
	if err != nil || m == nil {
		return err
	}
	m.State, m.NextAction = "failed", "start failed ("+cause.Error()+"); retry fae maintenance start with the previous binary"
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return savePrivate(path, raw)
}
