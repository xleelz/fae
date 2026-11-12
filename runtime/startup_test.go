package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStartupNonexecutionProof(t *testing.T) {
	for _, kind := range []string{"missing", "prepared", "deadline", "gate", "launched"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := filepath.EvalSymlinks(t.TempDir())
			d := &Direct{Root: root, Command: []string{"/bin/sh", "-c", "exit 17", "{config}"}, StartTimeout: 30 * time.Millisecond}
			if kind == "missing" {
				d.Command[0] = filepath.Join(root, "absent")
			}
			if kind == "prepared" {
				d.prepareProcess = func(context.Context, *exec.Cmd, string) (*processHooks, error) {
					return nil, errors.New("before launch")
				}
			}
			if kind == "deadline" {
				d.prepareProcess = func(ctx context.Context, _ *exec.Cmd, _ string) (*processHooks, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			if kind == "gate" {
				// The workload is never released: registration refuses a shared registry.
				if e := os.Mkdir(filepath.Join(root, ".processes"), 0755); e != nil {
					t.Fatal(e)
				}
			}
			started := time.Now()
			out, err := d.Run(context.Background(), Launch{DispatchID: "d", SessionID: "s", TaskID: "t", Role: "coder", Mode: "local", Worktree: root, Brief: json.RawMessage(`{}`)})
			if err == nil {
				t.Fatal("missing error")
			}
			if out.NonExecution != (kind != "launched") || !out.ProcessesRetired {
				t.Fatalf("proof: %+v %v", out, err)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatal("startup deadline not bounded")
			}
		})
	}
	if startupLimit(0) != 180*time.Second || startupLimit(time.Second) != time.Second {
		t.Fatal("startup default")
	}
}

func TestStartupGateTimeoutRetiresWithoutLateProviderLaunch(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	d := &Direct{Root: root, Command: []string{"/bin/sleep", "60", "{config}"}, StartTimeout: 500 * time.Millisecond}
	// The child below models a blocked transport gate, never a provider. The hook
	// cannot release it after cancellation because Started runs synchronously.
	d.prepareProcess = func(ctx context.Context, cmd *exec.Cmd, _ string) (*processHooks, error) {
		cmd.Args = []string{"/bin/sleep", "60"}
		return &processHooks{NonExecution: func() bool { return true }, Started: func() error { <-ctx.Done(); return ctx.Err() }, Close: func() error { return nil }}, nil
	}
	out, e := d.Run(context.Background(), Launch{DispatchID: "d", SessionID: "s", TaskID: "t", Role: "coder", Mode: "local", Worktree: root, Brief: json.RawMessage(`{}`)})
	if e == nil || !out.NonExecution || !out.ProcessesRetired || len(d.Sessions()) != 0 {
		t.Fatalf("%+v %v", out, e)
	}
	records, err := filepath.Glob(filepath.Join(root, ".processes", "*.json"))
	if err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
}
