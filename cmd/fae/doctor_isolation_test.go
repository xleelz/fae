package main

import (
	"github.com/xleelz/fae/provider"
	"testing"
)

func TestDoctorReportsIsolationStepAndRemedy(t *testing.T) {
	for _, available := range []bool{false, true} {
		env := doctorEnv{home: t.TempDir(), instance: "default", dolt: "dolt", lookPath: func(name string) (string, error) { return "/bin/" + name, nil }, version: func(string, ...string) (string, error) { return "git version 2.45.0", nil }, statfs: func(string) (uint64, uint64, error) { return 1 << 40, 1 << 20, nil }, sandboxed: func() (bool, error) { return false, nil }, isolation: func(string, string) provider.IsolationCapability {
			return provider.IsolationCapability{Available: available, Step: "namespace-tool", Reason: "missing bubblewrap", Remedy: "install bubblewrap"}
		}}
		found := false
		for _, check := range doctorChecks(env) {
			if check.Name == "session-isolation" {
				found = true
				if available && check.Status != "pass" || !available && (check.Status != "fail" || check.Remedy == "") {
					t.Fatal(check)
				}
			}
		}
		if !found {
			t.Fatal("isolation probe omitted")
		}
	}
}
