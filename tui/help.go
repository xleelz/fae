package tui

import (
	"fmt"
	"strings"
)

// helpBar lists the keys that work here.
func (m *Model) helpBar() string {
	keys := []string{}
	add := func(k, what string) { keys = append(keys, styleKey.Render(k)+" "+what) }
	switch {
	case m.modal != nil:
		for _, kv := range m.modal.keys() {
			add(kv[0], kv[1])
		}
		return strings.Join(keys, " · ")
	case m.help:
		add("any key", "close help")
		return strings.Join(keys, " · ")
	case m.detail != nil:
		add("↑↓", "scroll")
		add("esc", "back")
	case m.view == viewMolecules:
		add("←→", "molecule")
		add("↑↓", "task")
		add("enter", "open")
		add("z", "hide empty stages")
		add("/", "filter")
	case m.view == viewGraph:
		add("↑↓←→", "move")
		add("enter", "open")
		add("v", "list/columns")
		add("f", "hide finished")
		add("/", "find")
	case m.view == viewStream:
		add("↑↓", "move")
		add("enter", "open task")
		add("/", "filter")
		if m.opts.Write {
			add("n/N", "notify")
		}
	case m.view == viewPools:
		add("↑↓", "move")
		add("/", "filter")
		if m.opts.Write {
			add("s", "resize")
			add("q/Q", "quota")
			add("p/u", "pause/unpause")
		}
	case m.view == viewSymphony:
		add("↑↓", "move")
		add("enter", "open task")
		add("/", "filter")
		if m.opts.Write {
			add("S", "sync")
		}
	case m.view == viewSecurity:
		add("↑↓", "move")
		add("enter", "open task")
		add("/", "filter")
		if m.opts.Write {
			add("w", "widen")
			add("L", "land")
		}
	default:
		add("↑↓", "move")
		add("enter", "open task")
		if m.opts.Write {
			add("A", "ack alert")
			add("d/u/w", "act")
		}
	}
	if m.currentParent() != nil {
		add("p/[", "parent")
	}
	if m.currentChild() != nil {
		add("c/]", "child")
	}
	if m.opts.Write {
		if m.currentChild() == nil && m.detail == nil {
			add("c", "create")
		}
		if m.currentTask() != "" {
			if m.isEscalatedTask(m.currentTask()) {
				add("I/u", "un-escalate")
			}
			add("a", "actions")
			add("n/N", "notify")
			add("f", "follow-up")
			add("e", "edit")
			add("D", "delete")
			add("+/-", "edge")
		}
	}
	add(":", "palette")
	add("tab/1-7", "views")
	add("r", "refresh")
	add("?", "help")
	add("q", "quit")
	return strings.Join(keys, " · ")
}

func (m *Model) helpLines() []string {
	return m.helpScreen(m.width)
}

func (m *Model) helpScreen(w int) []string {
	lines := []string{
		styleHeading.Render("fae-tui"),
		"A terminal view of a running Fae daemon, refreshed every " + m.opts.Refresh.String() + " (each poll reads what the view shows;",
		fmt.Sprintf("the interval stretches up to %d× while nothing changes; r polls now).", backoffMax),
		"",
		styleBold.Render("Views"),
		"  1 Molecules   each running molecule's stage graph in its workflow's normal-case order, with the tasks at each stage",
		"  2 Graph       the backlog graph laid out by dependency depth; every task shows its implementation state",
		"  3 Attention   escalated, quarantined, uncertain, blocked and decision-waiting tasks, Prophet questions, and alerts",
		"  4 Stream      live execution panes, in-flight turns, turn budgets and durable checkpoints",
		"  5 Pools       provider concurrency pools, admission gauges, binding health, quota backoff and dispatch pauses",
		"  6 Symphony    Linear / Symphony tracker issues (running, retrying, known) and outbox circuit state",
		"  7 Security    target configs, read roots, read policies, scope-widen requests and recent audit events",
		"  enter opens a task: its fields, description, edges and the trace of stages it has been through.",
		"",
		styleBold.Render("Keys"),
		"  tab, shift+tab, 1-7   switch view          r   refresh now          : / ctrl+p   command palette",
		"  ↑↓ ←→ (or k j h l)    move                 R   show or hide retired tasks",
		"  pgup pgdn home end    page                 /   quick filter (esc clears)  ?   help;  q or ctrl+c quits",
		"  [ / ] (or p / c)      jump to parent / child task or molecule (SubWorkflow hierarchy)",
		"  molecules: z hides the stages that hold no task",
		"  graph: v list or columns, f hide finished tasks, / find a task, p n jump to a prerequisite or dependent",
		"",
		styleBold.Render("Writes") + " (only with --write; each first records an authorizing operator message)",
		"  c / C  create a task    a  action menu      e  edit task            D  delete task          +/-  add/remove edge",
		"  n / N  notify task (next-turn / in-flight)  f  follow-up task       A  acknowledge alert",
		"  attention: d decide, u resolve-uncertain, R resume, I restart, w approve-widen, G quarantine-release, X retire",
		"  pools: s resize pool, q/Q set/clear quota, p/u pause/unpause · symphony: S sync · security: w widen, L land",
	}
	if !m.opts.Write {
		lines = append(lines, "", styleWarn.Render("This session is read-only: start fae-tui with --write to make changes."))
	}
	legend := []string{}
	for _, c := range []stateClass{classReady, classWaiting, classStage, classRunning, classBlocked, classEscalated, classDone, classClosed, classUnknown} {
		legend = append(legend, classStyles[c].Render(classGlyphs[c]+" "+classNames[c]))
	}
	lines = append(lines, "", styleBold.Render("States"), "  "+strings.Join(legend, "  "))
	return lines
}

var classNames = map[stateClass]string{
	classUnknown: "unknown", classReady: "Pending/ready", classWaiting: "withheld, deferred or parked",
	classStage: "in a stage", classRunning: "turn in flight", classBlocked: "Blocked", classEscalated: "Escalated",
	classDone: "Done", classClosed: "Cancelled or Retired",
}
