package main

import "testing"

func TestQuarantineCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"quarantine"}, "QuarantineInspect", map[string]any{}},
		{[]string{"quarantine", "inspect", "--task", "alpha"}, "QuarantineInspect", map[string]any{"id": "alpha"}},
		{[]string{"quarantine", "release", "--task", "alpha", "--authorizing-message", "m"}, "QuarantineRelease", map[string]any{"id": "alpha", "authorizing_message": "m"}},
		{[]string{"quarantine", "retire", "--task", "alpha", "--reason", "obsolete", "--authorizing-message", "m"}, "QuarantineRetire", map[string]any{"id": "alpha", "reason": "obsolete", "authorizing_message": "m"}},
	})
	checkRefused(t, [][]string{{"quarantine", "release", "--task", "a"}, {"quarantine", "retire", "--task", "a", "--reason", "r"}})
	checkUsage(t, [][]string{{"quarantine", "release", "--authorizing-message", "m"}, {"quarantine", "retire", "--task", "a", "--authorizing-message", "m"}, {"quarantine", "retire", "--task", "a", "--reason", " ", "--authorizing-message", "m"}, {"quarantine", "bogus"}, {"quarantine", "inspect", "extra"}, {"quarantine", "inspect", "--reason", "r"}})
}
