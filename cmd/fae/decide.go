package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xleelz/fae/workflow"
	"io"
	"sort"
	"strconv"

	"github.com/xleelz/fae/daemon"
)

// decideCommand disposes a task's pending HumanDecision (FAE-WF-030) through
// the daemon; the decision artifact cites the authorizing message.
func decideCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	p := daemon.DecideParams{}
	file := f.String("items", "", "JSON list of per-item choices")
	choices := parentBindings{}
	rationales := parentBindings{}
	f.Var(&choices, "item-option", "INDEX_OR_ID=OPTION; repeat for each item")
	f.Var(&choices, "item", "alias of --item-option")
	f.Var(&rationales, "item-rationale", "INDEX_OR_ID=RATIONALE; repeat for each item")
	f.StringVar(&p.ID, "task", "", "task ID")
	f.StringVar(&p.Option, "option", "", "declared option of the pending HumanDecision")
	f.StringVar(&p.Branch, "branch", "", "pending fan branch path")
	f.StringVar(&p.Decision, "decision", "", "pending decision id from fae inbox (refuses if the task waits on another)")
	f.StringVar(&p.Rationale, "rationale", "", "recorded rationale")
	if err := parse(f, args); err != nil {
		return err
	}
	if *file != "" {
		if len(choices) > 0 {
			return usage("--items and --item-option are mutually exclusive")
		}
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&p.Items); err != nil {
			return err
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return usage("trailing per-item decision JSON")
		}
	}
	if len(choices) > 0 {
		keys := make([]string, 0, len(choices))
		for key := range choices {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			choice := workflow.ItemDecision{Option: choices[key], Rationale: rationales[key]}
			if index, err := strconv.Atoi(key); err == nil && index >= 0 {
				choice.Index = &index
			} else {
				choice.ID = key
			}
			p.Items = append(p.Items, choice)
		}
	}
	for key := range rationales {
		if _, ok := choices[key]; !ok {
			return usage("item rationale %s has no item option", key)
		}
	}
	if p.ID == "" || (p.Option == "" && p.Items == nil) {
		return usage("decide requires --task and --option")
	}
	if err := requireMessage(verb, opts); err != nil {
		return err
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, "Decide", "", p, out)
}
