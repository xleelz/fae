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
	files := flag.String("files", "", "comma-separated declared source files")
	test := flag.String("test-file", "", "pinned RED test file")
	tasks := flag.String("task-scopes", "", "pinned per-task file scope JSON literal")
	timeout := flag.Duration("timeout", 5*time.Minute, "provider timeout")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := provider.Run(ctx, provider.Config{SessionRoot: *sessions, TaskScopesJSON: *tasks, Binary: *binary, Files: strings.Split(*files, ","), TestFile: *test, Timeout: *timeout}, *file); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
