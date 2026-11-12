package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xleelz/fae/processowner"
)

// sandboxProbe is the native sandbox probe used for the watchdog's post-start
// acceptance (FAE-CTL-001 step 6, FAE-HOST-030); tests substitute it.
var sandboxProbe = Unsandboxed

type watchdogIdentity struct {
	PID   int    `json:"pid"`
	Birth string `json:"birth"`
}

// WatchdogIdentityPath is the private identity file beside the watchdog state.
func WatchdogIdentityPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "watchdog-identity.json")
}

// WriteWatchdogIdentity records the running watchdog's exact process identity.
func WriteWatchdogIdentity(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute watchdog identity path required")
	}
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		return e
	}
	raw, _ := json.Marshal(watchdogIdentity{os.Getpid(), birth})
	temp := path + ".tmp"
	if e = os.WriteFile(temp, raw, 0600); e != nil {
		return e
	}
	return os.Rename(temp, path)
}

// watchdogAcceptance checks the recorded watchdog's sandbox state. A watchdog
// proved stopped is reported, not accepted as unsandboxed; a live one must be
// positively unsandboxed.
func watchdogAcceptance(path string) (string, error) {
	if path == "" {
		return "not checked", nil
	}
	raw, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return "not running", nil
	}
	if e != nil {
		return "", e
	}
	var id watchdogIdentity
	if e = json.Unmarshal(raw, &id); e != nil || id.PID <= 1 || id.Birth == "" {
		return "", errors.Join(e, errors.New("invalid watchdog identity"))
	}
	if processowner.ProvedDead(id.PID, id.Birth) {
		return "not running", nil
	}
	if birth, e := processowner.Birth(id.PID); e != nil || birth != id.Birth {
		return "", errors.Join(e, errors.New("watchdog identity is ambiguous"))
	}
	safe, e := sandboxProbe(id.PID)
	if e != nil || !safe {
		return "", errors.Join(e, fmt.Errorf("watchdog pid %d runs with an inherited sandbox", id.PID))
	}
	return fmt.Sprintf("unsandboxed pid %d", id.PID), nil
}
