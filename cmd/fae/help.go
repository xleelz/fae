package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// timeSources states FAE-CLI-002's rule for every recorded time.
const timeSources = "Recorded times (admission, transitions, retirement, messages) come from the daemon clock; explicit time inputs (--until, --anchor, --end) are RFC3339 facts recorded as given. Loop expiry is either explicit (--expires-at, recorded as given) or computed by the daemon clock from --expires-in at creation."

var exitCodes = map[string]string{"0": "success, including help requested with --help", "1": "refusal or validation failure, including invalid content in a file the command reads (reason on standard error)", "2": "usage error: an unknown, missing or conflicting flag or operand, or a flag value that does not parse (usage on standard error)", "3": "daemon unreachable"}

// verbExitCodes replace exitCodes in the help of the commands whose codes
// differ from FAE-CLI-001's table: the brokers pass their command's code
// through, and the pre-tool hook exits only 0 or 2.
var verbExitCodes = func() map[string]map[string]string {
	hook := map[string]string{"0": "success: the decision is on standard output (or help requested with --help)", "2": "the tool call is blocked: a usage error, an unreadable request, no $HOME, or a decision that cannot be written or would be discarded; never 1, on which the provider would run the tool"}
	run := map[string]string{"0": "the command succeeded (or help requested with --help)", "1": "with --shim-policy in account mode, cancelled before the tool ran; otherwise the command's own 1", "2": "usage error (usage on standard error)", "75": "the heavy-op lease or host guard refused; nothing ran", "77": "a role shim policy refused", "78": "Local mode refused a git push or remote change", "126": "with --shim-policy in account mode, the tool could not be executed; otherwise the command's own 126", "other": "the command's own exit code"}
	gate := map[string]string{"0": "the command succeeded (or help requested with --help)", "1": "the working directory or a run identity could not be read, so nothing ran; otherwise the command's own 1", "2": "usage error (usage on standard error)", "75": "the admission wait timed out; nothing ran", "76": "admission was refused; nothing ran", "other": "the command's own exit code"}
	both := map[string]string{"0": "run: the command succeeded; hook: the decision is on standard output", "2": "run: usage error; hook: the tool call is blocked (any hook failure; never 1)"}
	for code, text := range run {
		if both[code] == "" {
			both[code] = "run: " + text
		}
	}
	return map[string]map[string]string{"heavy-op hook": hook, "heavy-op run": run, "heavy-op": both, "gate run": gate, "gate": gate}
}()

// exitCodesFor is the exit-code table of command's help.
func exitCodesFor(command string) map[string]string {
	if codes, ok := verbExitCodes[command]; ok {
		return codes
	}
	return exitCodes
}

// configuredDefaults states REQUIREMENTS.md's rule for a flag that carries a
// configurable default the spec states.
const configuredDefaults = "A flag carrying a configurable default the spec states (the common --home and --instance; serve's adapter, default mode, UI bind, limits, waits, windows, retention, Dolt port and stop wait, inline limit, re-arm budget and Prophet failovers; the watchdog's stall, re-arm and filesystem bounds; the roll and maintenance waits; land --then-roll's roll wait and overlap hold; gate run's landing floor, wait and capacity; heavy-op run's maximum wait; census pacing; a loop's --expires-in; provider-health's window) that is empty, out of range or not finite falls back to its default with a warning on standard error naming the command, the flag, the value and the default. --home, --instance, the adapter, the mode and the bind fall back only when empty; any other invalid value is refused as before. A value that does not parse is a usage error (2). Warnings are log lines: RFC 3339 time, instance=NAME, component=cli."

var templateVerbs = map[string]bool{
	"workflow-put": true, "workflow-get": true, "workflow-latest": true, "workflow-list": true, "workflow-retire": true, "workflow-diff": true, "workflow-validate": true, "workflow-simulate": true,
	"target-config-put": true, "target-config-get": true, "target-config-list": true, "target-config-retire": true, "target-config-validate": true, "target-config-diff": true, "target-config-simulate": true,
	"adapter-put": true, "adapter-get": true, "adapter-list": true, "adapter-retire": true,
}

// commandList is the fae help inventory; add one row per new verb.
var commandList = [][2]string{
	{"workflow-limit", "[--max -1|1..1000000 --authorizing-message ID]"},
	{"change-request", "list --task ID | disposition --task ID --id ID --state accepted|declined --reason TEXT --authorizing-message ID"},
	{"resume-at-stage", "--task ID [--context TEXT] --authorizing-message ID"},
	{"tree", "<task> [--stats]"},
	{"tree-limit", "--task ID --max-sessions N --authorizing-message ID"},
	{"serve", "--workspace PATH [--adapter multiplexer|process|scripted] [--live-config FILE | --scenario FILE] [--default-mode local|normal] [--run-id ID] [--dolt PATH] [--dolt-port N] [--dolt-stop-wait D] [--inline-limit BYTES (4096..65536, default 65536)] [--max-panes N] [--max-tasks-in-flight N] [--recursion-ceiling N] [--max-molecules N] [--max-workflow-molecules N] [--max-heavy-weight N] [--event-retention-age D] [--event-retention-count N] [--message-max-age D] [--rearm-budget N] [--rearm-window D] [--prophet-routes FILE] [--prophet-max-failovers N] [--prophet-provider NAME] [--prophet-model MODEL] [--principal-config FILE] [--scratch] [--ui [--ui-bin PATH] [--ui-dist DIR] [--ui-port N] [--ui-bind 127.0.0.1] [--ui-allow-writes] [--ui-rpc-log FILE]] [--quarantine-unrunnable-revisions] [--foreground] (an empty or out-of-range limit, wait or window, or an empty adapter, mode or bind, falls back to its default with a warning)"},
	{"status", "[--tasks] [--include-retired]"},
	{"run", "--prompt TEXT|--content TEXT|--file FILE [--title TEXT] [--id ID] [--mode local | --mode normal --dry-run|--commit-external (normal takes exactly one)] [--workflow ID [--revision N]] [--target-config ID [--target-revision N]] [--priority N>=10] [--critical] [--run-id ID] [--ticket-room ROOM] [--review-destination-branch BRANCH] [--push-namespace NS] [--idempotence-key KEY] [--task-class CLASS] [--carry-from TASK] [--input NAME=VALUE|NAME=@FILE]... [--parent TASK] [--from-parent INPUT=ARTIFACT]... [--max-sessions N] [--declared-file PATH]... [--start] --authorizing-message ID"},
	{"start-work-item", "--title TEXT --content TEXT|--file FILE [--priority N>=10] [--declared-file PATH]... [selector and disposition flags] --authorizing-message ID"},
	{"resume", "[--run-id ID] [--task ID] --authorizing-message ID"},
	{"show", "--task ID | --run-id ID [--include-retired]"},
	{"history", "--task ID [--summary|--records [--after N] [--limit N]]"},
	{"topology", "[--workflow ID] [--after MOLECULE] [--limit N] [--include-retired]"},
	{"provenance", "--commit SHA [--limit N] (chain from a commit back to the spec)"},
	{"board", "[--after ID] [--limit N] [--include-retired] [--state S,...] [--target ID] [--workflow ID] [--mode local|normal] [--molecule ID] [--run-id ID] [--min-priority N] [--in-flight]"},
	{"leases", "(role slots, heavy operations, deployment holds and pane reservations with holders and waiters)"},
	{"panes", "(live sessions with role, task, binding, owner and age, and pane reservations)"},
	{"events", "[--after N] [--limit N] [--follow]"},
	{"message", "--text TEXT|--file FILE [--id ID] [--task ID]"},
	{"reply", "--to MESSAGE_ID --text TEXT|--file FILE [--id ID] [--task ID]"},
	{"inbox", "[--task ID] [--kind threads,escalated,blocked,uncertain,decisions,questions] [--limit N] [--per-thread N]"},
	{"backlog", "admit --file FILE --authorizing-message ID | edge add --from ID --to ID [--kind blocking|relates] [--accept-in-flight] --authorizing-message ID | edge remove --from ID --to ID [--kind blocking|relates] --authorizing-message ID | show [--state STATE] [--include-retired]"},
	{"reconcile-done", "[--dry-run] --authorizing-message ID"},
	{"task", taskUsage},
	{"retire|escalate|block|cancel|defer|undefer|reprioritize|set-attempt-limit|restart|resume-with-context", "--task ID [--reason TEXT] [--until RFC3339|--undefer] [--priority N] [--limit N] [--information TEXT|--information-file FILE] [--context TEXT|--context-file FILE] --authorizing-message ID"},
	{"notify", "--task ID --kind breaking-change|cross-area-need|note --body TEXT [--from-task ID] [--delivery next-turn|in-flight] [--actor ACTOR] [--reason TEXT]"},
	{"follow-up", "--task DONE_TASK_ID --text PROMPT [--id ID] [--wall-seconds N] [--max-turns N] [--actor ACTOR] [--reason TEXT]"},
	{"resolve-uncertain", "--id TASK --dispatch ID --action redispatch|accept|escalate [--result FILE] --authorizing-message ID"},
	{"attach-design-doc", "--task ID|--item ID --reference URL|PATH --authorizing-message ID"},
	{"migrate", "--task ID --revision N [--stage STAGE] --authorizing-message ID"},
	{"decide", "--task ID [--option OPTION] [--decision ID] [--items FILE | --item-option INDEX_OR_ID=OPTION ... --item-rationale INDEX_OR_ID=TEXT ...] [--rationale TEXT] --authorizing-message ID"},
	{"supply-artifact", "--task ID [--stage STAGE] --file JSON --authorizing-message ID"},
	{"analyze-session", "--task ID"},
	{"reclaim", "[--host-wide] [--dry-run] --authorizing-message ID"},
	{"quarantine", "[inspect] [--task ID] | release --task ID --authorizing-message ID | retire --task ID --reason TEXT --authorizing-message ID"},
	{"prophet", "session|chat|global [--task ID] [--text TEXT] | restart --information TEXT|--information-file FILE | resume --context TEXT|--context-file FILE (--task ID --authorizing-message ID)"},
	{"template", "workflow-put|workflow-get|workflow-latest|workflow-list|workflow-retire|workflow-diff|workflow-validate|workflow-simulate | target-config-put|-get|-list|-retire|-validate|-diff|-simulate | adapter-put|-get|-list|-retire"},
	{"workflow", "default [--local]|scenario | validate FILE | simulate FILE SCRIPT TASK (offline; SCRIPT is the simulator's records or a scripted-adapter scenario such as scenario prints)"},
	{"schedule", "create|list|inspect|pause|resume|retire --id ID [...]"},
	{"runtime-config", "[--pane-limit N] [--heavy-capacity N] [--tasks-in-flight N] [--host-thresholds JSON] [--nested-commands refuse|account] --authorizing-message ID"},
	{"pool", "resize --pool ID [--provider NAME] [--credential ID] --cap N [--actor ACTOR] --reason TEXT --authorizing-message ID"},
	{"quota", "set [--pool ID | --provider NAME --model MODEL] (--until RFC3339 | --duration D) [--grace D] [--actor ACTOR] [--reason TEXT] --authorizing-message ID | clear [--pool ID | --provider NAME --model MODEL] [--actor ACTOR] [--reason TEXT] --authorizing-message ID"},
	{"pause", "[--scope instance|workflow|task_set|pool] [--target ID] [--workflow ID] [--run-id ID] [--tasks ID,...] [--pool ID] [--actor ACTOR] --reason TEXT --authorizing-message ID | list"},
	{"unpause", "[--scope instance|workflow|task_set|pool] [--target ID] [--workflow ID] [--run-id ID] [--tasks ID,...] [--pool ID] [--actor ACTOR] [--reason TEXT] --authorizing-message ID"},
	{"provider-health", "[--window D (1s..24h, default 15m; an empty or out-of-range window falls back to 15m with a warning)]"},
	{"provider-log", "[--after N] [--limit N]"},
	{"alerts", "[list] [--after N] [--limit N] [--unacknowledged] | ack --id N --authorizing-message ID"},
	{"metrics", "[--window D (1s..168h)]"},
	{"host-metrics", ""},
	{"ui", "token (the UI child's token, URL and one-time login URL; instance operator only)"},
	{"drain", "--authorizing-message ID (finish active turns; pause dispatch until restart)"},
	{"shutdown", "--authorizing-message ID"},
	{"drive", "stop|rearm|status [--authorizing-message ID]"},
	{"maintenance", "stop --note TEXT --launcher-socket PATH [--binary PATH] [--gated-ref REF] [--max-wait D] [--overlap-wait D] [--ack-overlap] [--when-clear] --authorizing-message ID | start [--launcher-socket PATH] [--binary PATH] [--max-wait D] --authorizing-message ID | status"},
	{"roll", "--note TEXT --launcher-socket PATH (--binary PATH | --build-ref REF) [--gated-ref REF] [--max-wait D] [--overlap-wait D] [--ack-overlap] [--when-clear] --authorizing-message ID"},
	{"watchdog", "--authorizing-message ID [--period D (at least 10ms, default 1m)] [--stall D] [--rearm-budget N] [--rearm-window D] [--fs-threshold F] | --revoke-grant ID"},
	{"runtime-launcher", "--socket PATH (serves the --instance whose maintenance marker authorizes each launch)"},
	{"land", "--task ID|--batch ID,... --target BRANCH [--salvage --source-ref REF] [--poll D (at least 10ms, default 2s)] [--then-roll --launcher-socket PATH [--note TEXT] [--roll-max-wait D] [--overlap-wait D] [--ack-overlap]] --authorizing-message ID"},
	{"gates", "self-test [--offline] (through the daemon's GatesSelfTest read; --offline runs it in this process)"},
	{"gate", "run [--host-ledger DIR] [--weight 1|2] [--capacity N (default 2)] [--max-wait D (default 30m)] [--slice D] [--memory-floor-gib G (default 12; never lower than the hard memory floor)] [--dir DIR] [--authorizing-message ID] -- COMMAND... (exits with the command's code; 75 admission wait timed out, 76 admission refused, 1 when the working directory or a run identity cannot be read)"},
	{"workspace", "review-plan|review-prepare --task ID [--adapter ID]"},
	{"review-health", "--task ID"},
	{"census", "[--file FILE] [--include-blocked] | apply --file FILE [--gap D (0s..1m, default 2s)] --authorizing-message ID"},
	{"cleanup", "retired|done [--dry-run|--apply] [--backup PATH] [--pace D] [--limit N] | scratch [--dry-run|--apply] [--dolt PATH] | scratch-archive --scratch-instance NAME --source DIR --survivor DIR [--dry-run|--apply --plan-digest D] (through the serving --instance) [--authorizing-message ID]"},
	{"backup", "plane|unpin|quota|retention|replicate|retention-schedule|creation-schedule|replication-schedule [...]"},
	{"doctor", "[--dolt PATH] [--live-config FILE] (prerequisite, layout, sandbox, filesystem and configuration checks; target configurations through a running daemon)"},
	{"version", ""},
	{"help", "[--json]; COMMAND --help prints that command's flags"},
}

// hiddenCommands are dispatched and conformance-tested but left out of fae help.
var hiddenCommands = [][2]string{
	{"heavy-op", "run (--weight N --class role|controller|operator [--ledger DIR] | --shim-policy FILE) [--max-wait D] -- COMMAND... | hook --shim-dir DIR < REQUEST (FAE-CLI-053 broker verb; run exits with the command's code, 75 when refused, 77 when a role shim policy refuses, 78 when Local mode refuses a git push or remote change, and with --shim-policy in account mode 1 when cancelled before the tool runs and 126 when the tool cannot be executed; hook, the FAE-HOST-011 pre-tool hook, prints its decision and exits 0, or 2 on any failure (a usage error, an unreadable request, no $HOME, a decision it cannot write or that would be discarded: standard output closed, on /dev/null or a pipe whose reader has gone), which the provider treats as a block; never 1)"},
}

// isHelp is an explicit help request; it is success (exit 0), not a usage error.
func isHelp(arg string) bool { return arg == "--help" || arg == "-h" || arg == "-help" }

func globalHelp(asJSON bool, out io.Writer) error {
	if asJSON {
		rows := []map[string]string{}
		for _, c := range commandList {
			rows = append(rows, map[string]string{"command": c[0], "usage": c[1]})
		}
		return jsonOut(out, map[string]any{"commands": rows, "common_flags": []string{"--instance NAME (default default)", "--home PATH", "--json", "--authorizing-message ID"}, "exit_codes": exitCodes, "configured_defaults": configuredDefaults, "time": timeSources})
	}
	_, err := io.WriteString(out, globalHelpText())
	return err
}
func globalHelpText() string {
	var b strings.Builder
	for _, c := range commandList {
		fmt.Fprintf(&b, "fae %s %s\n", c[0], c[1])
	}
	fmt.Fprintf(&b, "Common flags: --instance NAME (default default) --home PATH --json --authorizing-message ID.\nExit codes: 0 success (and --help), 1 refusal or validation failure (including invalid file content), 2 usage error (flags and operands), 3 daemon unreachable.\n%s\n%s\n", configuredDefaults, timeSources)
	return b.String()
}

// subcommandHelp answers COMMAND --help for a command whose usage starts with a sub-verb.
func subcommandHelp(command, usage string, asJSON bool, out io.Writer) error {
	if asJSON {
		return jsonOut(out, map[string]any{"command": command, "usage": usage, "exit_codes": exitCodesFor(command), "time": timeSources})
	}
	_, err := fmt.Fprintf(out, "fae %s %s\n%s\n", command, usage, timeSources)
	return err
}

// groupHelp answers GROUP --help for verbs whose usage starts with a sub-verb.
func groupHelp(verb string, asJSON bool, out io.Writer) bool {
	for _, c := range append(append([][2]string{}, commandList...), hiddenCommands...) {
		if c[0] == verb && c[1] != "" && c[1][0] >= 'a' && c[1][0] <= 'z' {
			return subcommandHelp(verb, c[1], asJSON, out) == nil
		}
	}
	return false
}

// commandUsage answers COMMAND --help with that command's flags.
func commandUsage(f *flag.FlagSet, asJSON bool, out io.Writer) error {
	type option struct {
		Name    string `json:"name"`
		Default string `json:"default"`
		Usage   string `json:"usage"`
	}
	options := []option{}
	f.VisitAll(func(x *flag.Flag) { options = append(options, option{x.Name, x.DefValue, x.Usage}) })
	if asJSON {
		return jsonOut(out, map[string]any{"command": f.Name(), "flags": options, "exit_codes": exitCodesFor(f.Name()), "time": timeSources})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fae %s\n", f.Name())
	for _, o := range options {
		fmt.Fprintf(&b, "  --%s (default %q): %s\n", o.Name, o.Default, o.Usage)
	}
	if codes, ok := verbExitCodes[f.Name()]; ok {
		keys := make([]string, 0, len(codes))
		for code := range codes {
			keys = append(keys, code)
		}
		sort.Strings(keys)
		fmt.Fprint(&b, "Exit codes:")
		for _, code := range keys {
			fmt.Fprintf(&b, " %s %s;", code, codes[code])
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, timeSources)
	_, err := io.WriteString(out, b.String())
	return err
}

func sortedKeys(m map[string]bool) []string {
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
