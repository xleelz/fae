package main

import (
	"context"
	"io"
	"strings"

	"github.com/xleelz/fae/daemon"
)

// messageCommand records an operator message or reply, or reads the inbox.
// The daemon assigns message ids unless --id names one explicitly.
func messageCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	if verb == "inbox" {
		task := f.String("task", "", "only this task's threads and items")
		kinds := f.String("kind", "", "comma-separated: threads,escalated,blocked,uncertain,decisions,questions")
		limit := f.Int("limit", 50, "threads 1..500")
		perThread := f.Int("per-thread", 20, "messages per thread 1..500")
		if err := parse(f, args); err != nil {
			return err
		}
		p := daemon.InboxParams{Task: *task, Limit: *limit, PerThread: *perThread}
		if *limit < 1 || *limit > 500 || *perThread < 1 || *perThread > 500 {
			return usage("inbox --limit and --per-thread must be 1..500")
		}
		if *kinds != "" {
			for _, k := range strings.Split(*kinds, ",") {
				if !strings.Contains(",threads,escalated,blocked,uncertain,decisions,questions,", ","+k+",") {
					return usage("unknown inbox kind %q", k)
				}
				p.Kinds = append(p.Kinds, k)
			}
		}
		return rpc(ctx, *opts, "Inbox", "", p, out)
	}
	text := f.String("text", "", "operator text")
	file := f.String("file", "", "operator message file")
	id := f.String("id", "", "explicit message ID (default: assigned by the daemon)")
	task := f.String("task", "", "scope the message to a task")
	var to *string
	if verb == "reply" {
		to = f.String("to", "", "message ID being answered")
	}
	if err := parse(f, args); err != nil {
		return err
	}
	if to != nil && *to == "" {
		return usage("reply requires --to MESSAGE_ID")
	}
	content, err := inputText(*text, *file)
	if err != nil {
		return err
	}
	p := daemon.MessageParams{ID: *id, Text: content, Task: *task}
	if to != nil {
		p.ReplyTo = *to
	}
	return rpc(ctx, *opts, "Message", "", p, out)
}
