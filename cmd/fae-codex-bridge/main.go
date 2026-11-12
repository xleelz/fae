package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/xleelz/fae/provider"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	sessions := flag.String("session-root", "", "private durable provider context cache for declared role scopes")
	binary := flag.String("codex", "", "absolute Codex executable")
	file := flag.String("config", "", "fae-agent-v1 config")
	files := flag.String("files", "", "comma-separated pinned source files; without it or --task-scopes the brief's task_scope block is the scope")
	test := flag.String("test-file", "", "pinned RED test file (requires --files)")
	tasks := flag.String("task-scopes", "", "pinned per-task file scope JSON literal")
	timeout := flag.Duration("timeout", 5*time.Minute, "provider timeout")
	start := flag.Duration("start-timeout", provider.DefaultStartTimeout, "agent-start bound: a provider that has not acknowledged the prompt (written its first event) within it is stopped with its processes and reported provider-not-started")
	flag.Parse()
	var pinned []string
	if *files != "" {
		pinned = strings.Split(*files, ",")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := provider.Run(ctx, provider.Config{SessionRoot: *sessions, TaskScopesJSON: *tasks, Binary: *binary, Files: pinned, TestFile: *test, Timeout: *timeout, StartTimeout: *start}, *file); err != nil {
		// A refusal the bridge recorded exits with the protocol's refusal
		// status, so the runtime reports its class; anything else exits 1.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(provider.ExitCode(err))
	}
}
