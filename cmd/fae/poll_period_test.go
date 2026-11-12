package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// FAE-CLI-001 and FAE-CTL-020: a positive watchdog period below 10 ms is a
// usage error (2) naming --period and the floor, with no fallback warning,
// before any instance is read: the period has no default in the spec, and a
// shorter one would run the watchdog's ticks nearly back to back. Before,
// 1ns was accepted. The floor itself passes the check and reaches the
// never-started instance (3).
func TestWatchdogPeriodBelowTheFloorIsAUsageError(t *testing.T) {
	warnings := captureWarnings(t)
	home := shortHome(t)
	watchdog := func(period string) error {
		return execute(context.Background(), []string{"watchdog", "--home", home, "--instance", "dog", "--authorizing-message", "m", "--period", period}, &bytes.Buffer{})
	}
	for _, period := range []string{"1ns", "1us", "9ms"} {
		warnings.Reset()
		if err := watchdog(period); exitCode(err) != 2 || !strings.Contains(fmt.Sprint(err), "--period must be at least 10ms") || warnings.Len() != 0 {
			t.Errorf("watchdog --period %s: exit %d (%v), warnings %q; want usage 2 naming the floor", period, exitCode(err), err, warnings.String())
		}
	}
	if err := watchdog("10ms"); exitCode(err) != 3 {
		t.Errorf("watchdog --period 10ms on a never-started instance: exit %d (%v), want 3", exitCode(err), err)
	}
}

// FAE-CLI-001 and FAE-CTL-030: a nonpositive land --poll is a usage error
// whose message names --poll. Before, it exited 2 with the generic land usage
// line, which does not mention --poll.
func TestLandNonpositivePollNamesTheFlag(t *testing.T) {
	for _, poll := range []string{"0", "0s", "-1s"} {
		err := execute(context.Background(), []string{"land", "--authorizing-message", "m", "--target", "main", "--task", "t", "--poll", poll}, &bytes.Buffer{})
		if exitCode(err) != 2 || !strings.Contains(fmt.Sprint(err), "land --poll must be a positive duration") {
			t.Errorf("land --poll %s: exit %d (%v); want usage 2 naming --poll", poll, exitCode(err), err)
		}
	}
}

// FAE-CLI-001 and FAE-CTL-033: a positive land --poll below 10 ms is a usage
// error (2) naming --poll and the floor, before any instance is read: each
// poll is a LandStatus read, and a shorter interval would issue them back to
// back for the whole landing. Before, 1ns was accepted and reached the
// instance. The floor itself passes the check and reaches the never-started
// instance (3).
func TestLandPollBelowTheFloorIsAUsageError(t *testing.T) {
	home := shortHome(t)
	land := func(poll string) error {
		return execute(context.Background(), []string{"land", "--home", home, "--instance", "lander", "--authorizing-message", "m", "--target", "main", "--task", "t", "--poll", poll}, &bytes.Buffer{})
	}
	for _, poll := range []string{"1ns", "1us", "1ms", "9.999ms"} {
		if err := land(poll); exitCode(err) != 2 || !strings.Contains(fmt.Sprint(err), "land --poll must be at least 10ms") {
			t.Errorf("land --poll %s: exit %d (%v); want usage 2 naming the floor", poll, exitCode(err), err)
		}
	}
	if err := land("10ms"); exitCode(err) != 3 {
		t.Errorf("land --poll 10ms on a never-started instance: exit %d (%v), want 3", exitCode(err), err)
	}
}
