package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

type commandError struct {
	code int
	err  error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }
func exitCode(err error) int {
	var ce *commandError
	if errors.As(err, &ce) {
		return ce.code
	}
	return 1
}
func usage(format string, args ...any) error { return &commandError{2, fmt.Errorf(format, args...)} }
func unreachable(err error) error {
	return &commandError{3, fmt.Errorf("daemon unreachable; start it with fae serve --workspace PATH --adapter scripted: %w", err)}
}

type common struct {
	home, instance, message string
	json                    bool
}

func flags(name string, c common) (*flag.FlagSet, *common) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&c.home, "home", c.home, "instance home (default ~/.fae/instances)")
	f.StringVar(&c.instance, "instance", c.instance, "instance name")
	f.StringVar(&c.message, "authorizing-message", c.message, "previously recorded operator message ID")
	f.BoolVar(&c.json, "json", c.json, "JSON output")
	return f, &c
}
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return usage("%s: %v", f.Name(), err)
	}
	if f.NArg() != 0 {
		return usage("unexpected arguments: %s", strings.Join(f.Args(), " "))
	}
	return nil
}
func identity(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
func jsonOut(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
func execute(ctx context.Context, args []string, out io.Writer) error {
	userhome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c := common{home: filepath.Join(userhome, ".fae", "instances"), instance: "default"}
	// Common flags are accepted before the verb as well as after it.
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		if args[0] == "--json" {
			c.json = true
			args = args[1:]
			continue
		}
		if len(args) < 2 {
			return usage("missing value for %s", args[0])
		}
		switch args[0] {
		case "--home":
			c.home = args[1]
		case "--instance":
			c.instance = args[1]
		case "--authorizing-message":
			c.message = args[1]
		default:
			return usage("unknown global flag %s", args[0])
		}
		args = args[2:]
	}
	if len(args) == 0 {
		return usage("usage: fae serve|status|run|board|history|events|provider-health|provider-log|runtime-config|message|drain|shutdown|workflow|template")
	}
	verb, args := args[0], args[1:]
	if verb == "workspace" {
		return reviewWorkspaceCommand(ctx, args, c, out)
	}
	if verb == "census" {
		return censusCommand(ctx, args, c, out)
	}
	if verb == "cleanup" || verb == "backup" {
		return cleanupCommand(ctx, verb, args, c, out)
	}
	if verb == "backlog" {
		return backlogCommand(ctx, args, c, out)
	}
	if verb == "reconcile-done" {
		return reconcileDoneCommand(ctx, args, c, out)
	}
	if verb == "prophet" && len(args) > 0 && (args[0] == "restart" || args[0] == "resume") {
		sub := args[0]
		if sub == "resume" {
			sub = "resume-with-context"
		}
		return taskControlCommand(ctx, sub, args[1:], c, out)
	}
	if verb == "prophet" {
		return prophetCommand(ctx, args, c, out)
	}
	if _, ok := taskControlMethods[verb]; ok {
		return taskControlCommand(ctx, verb, args, c, out)
	}
	if verb == "review-health" {
		return reviewHealthCommand(ctx, args, c, out)
	}
	if verb == "drive" || verb == "maintenance" || verb == "roll" || verb == "watchdog" || verb == "runtime-launcher" {
		return controllerCommand(ctx, verb, args, c, out)
	}
	if verb == "schedule" {
		return scheduleCommand(ctx, args, c, out)
	}
	if verb == "version" {
		f, opts := flags(verb, c)
		if err := parse(f, args); err != nil {
			return err
		}
		if opts.json {
			return jsonOut(out, map[string]string{"version": "development", "adapter": "scripted", "status": "partial requirements implementation"})
		}
		return offline([]string{"version"}, out)
	}
	if verb == "workflow" {
		if len(args) > 0 && (args[0] == "default" || args[0] == "scenario") {
			f, _ := flags("workflow "+args[0], c)
			if err := parse(f, args[1:]); err != nil {
				return err
			}
			if args[0] == "default" {
				return jsonOut(out, defaults.Workflow())
			}
			return jsonOut(out, defaults.HappyScript())
		}
		clean := []string{verb}
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--json":
			case "--instance", "--home":
				if i+1 >= len(args) {
					return usage("missing global flag value")
				}
				i++
			default:
				clean = append(clean, args[i])
			}
		}
		return offline(clean, out)
	}
	if verb == "help" {
		_, err := fmt.Fprintln(out, "fae serve --workspace PATH --adapter scripted [--dolt PATH] [--foreground]\nfae message --text TEXT [--id ID]\nfae template workflow-put --content FILE --authorizing-message ID\nfae run --prompt TEXT [--workflow ID] --authorizing-message ID\nfae status | board | history --task ID | events [--after N]\nfae provider-health | provider-log [--after N] [--limit N]\nfae drain --authorizing-message ID (finish active turns; pause dispatch until restart)\nfae shutdown --authorizing-message ID\nCommon flags: --home PATH --instance NAME --json. Recorded timestamps come from the daemon clock. Direct mode requires an explicitly configured trusted agent bridge and live target file.")
		return err
	}
	if verb == "template" {
		if len(args) == 0 {
			return usage("template requires workflow-put")
		}
		verb, args = args[0], args[1:]
	}
	f, opts := flags(verb, c)
	switch verb {
	case "serve":
		scratch := f.Bool("scratch", false, "mark a newly created disposable instance")
		workspace := f.String("workspace", "", "canonical workspace; must equal current directory")
		binary := f.String("dolt", "dolt", "Dolt executable")
		adapter := f.String("adapter", "", "adapter: scripted, direct or multiplexer")
		liveFile := f.String("live-config", "", "pinned live target and trusted bridge JSON")
		scenario := f.String("scenario", "", "scripted stage outputs JSON; default built-in rehearsal")
		runID := f.String("run-id", "backlog-drain", "run ID")
		prophetRoutesFile := f.String("prophet-routes", "", "ordered Prophet binding JSON file (maximum 8)")
		prophetFailovers := f.Int("prophet-max-failovers", 0, "Prophet failovers: 0 defaults to one, -1 disables, maximum 7")
		prophetProvider := f.String("prophet-provider", "codex", "Prophet provider binding")
		prophetModel := f.String("prophet-model", "gpt-6-astra", "Prophet provider model")
		principalConfig := f.String("principal-config", "", "owner-only delegated credential configuration")
		eventAge := f.Duration("event-retention-age", 24*time.Hour, "minimum retained event age window")
		eventCount := f.Int("event-retention-count", 100000, "minimum retained event count window")
		messageMaxAge := f.Duration("message-max-age", 24*time.Hour, "maximum age of operator message authorization (positive duration)")
		max := f.Int("max-tasks-in-flight", 4, "maximum concurrent scripted stages")
		port := f.Int("dolt-port", 0, "private Dolt port (0 derives from data directory)")
		foreground := f.Bool("foreground", false, "run in foreground")
		_ = foreground
		if err := parse(f, args); err != nil {
			return err
		}
		if *workspace == "" {
			return usage("serve requires --workspace")
		}
		if *adapter != "scripted" && *adapter != "direct" && *adapter != "multiplexer" {
			return fmt.Errorf("adapter %q is not implemented; select scripted, direct or multiplexer explicitly", *adapter)
		}
		var live *daemon.LiveConfig
		if *adapter == "direct" || *adapter == "multiplexer" {
			if *liveFile == "" || *scenario != "" {
				return usage("live adapter requires --live-config and forbids --scenario")
			}
			raw, e := readBounded(*liveFile)
			if e != nil {
				return e
			}
			raw, e = domain.CanonicalJSON(raw)
			if e != nil {
				return e
			}
			live = &daemon.LiveConfig{}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if e = dec.Decode(live); e != nil {
				return e
			}
			if live.Adapter != "" && live.Adapter != *adapter {
				return usage("live configuration adapter differs from --adapter")
			}
			live.Adapter = *adapter
		} else if *liveFile != "" {
			return usage("--live-config requires --adapter direct or multiplexer")
		}
		if *messageMaxAge <= 0 {
			return usage("message-max-age must be positive")
		}
		if *max < 1 {
			return fmt.Errorf("max-tasks-in-flight must be positive")
		}
		var executor engine.Executor
		if *scenario != "" {
			raw, e := readBounded(*scenario)
			if e != nil {
				return e
			}
			raw, e = domain.CanonicalJSON(raw)
			if e != nil {
				return e
			}
			var steps map[string][]engine.ScriptStep
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if e = dec.Decode(&steps); e != nil {
				return e
			}
			executor = engine.NewScripted(steps)
		}
		var prophetRoutes []workflow.Binding
		if *prophetRoutesFile != "" {
			raw, e := os.ReadFile(*prophetRoutesFile)
			if e != nil {
				return e
			}
			if e = json.Unmarshal(raw, &prophetRoutes); e != nil {
				return e
			}
			if len(prophetRoutes) == 0 {
				return errors.New("Prophet routes file must not be empty")
			}
		}
		d, err := daemon.Start(ctx, daemon.Config{Scratch: *scratch, ProphetRoutes: prophetRoutes, ProphetMaxFailovers: *prophetFailovers, ProphetProvider: *prophetProvider, ProphetModel: *prophetModel, EventRetentionAge: *eventAge, EventRetentionCount: *eventCount, PrincipalConfig: *principalConfig, MessageMaxAge: *messageMaxAge, Executor: executor, Live: live, Root: opts.home, Instance: opts.instance, Workspace: *workspace, DoltBinary: *binary, RunID: *runID, Port: *port, MaxInFlight: *max})
		if err != nil {
			return err
		}
		if err = jsonOut(out, map[string]any{"instance": opts.instance, "adapter": *adapter, "pid": os.Getpid(), "ready": true}); err != nil {
			_ = d.Close(context.Background())
			return err
		}
		return d.Run(ctx)
	case "provider-health":
		if err := parse(f, args); err != nil {
			return err
		}
		return rpc(ctx, *opts, "ProviderHealth", "", map[string]any{}, out)
	case "provider-log":
		after := f.Int64("after", 0, "resume after audit sequence")
		limit := f.Int("limit", 100, "page size 1..500")
		if err := parse(f, args); err != nil {
			return err
		}
		if *after < 0 || *limit < 1 || *limit > 500 {
			return usage("provider-log cursor must be nonnegative and limit 1..500")
		}
		return rpc(ctx, *opts, "ProviderObservations", "", map[string]any{"after": *after, "limit": *limit}, out)
	case "status", "board":
		includeRetired := f.Bool("include-retired", false, "include retired tasks")
		var after *string
		var limit *int
		if verb == "board" {
			after = f.String("after", "", "resume after task ID")
			limit = f.Int("limit", 100, "page size 1..500")
		}
		if verb == "status" {
			_ = f.Bool("tasks", false, "include tasks")
		}
		if err := parse(f, args); err != nil {
			return err
		}
		if verb == "board" {
			if *limit < 1 || *limit > 500 {
				return fmt.Errorf("board limit must be 1..500")
			}
			return rpc(ctx, *opts, "Board", "", map[string]any{"after": *after, "limit": *limit, "include_retired": *includeRetired}, out)
		}
		return rpc(ctx, *opts, "Status", "", map[string]any{"include_retired": *includeRetired}, out)
	case "history", "show":
		includeRetired := f.Bool("include-retired", false, "include retired tasks")
		task := f.String("task", "", "task ID")
		summary := f.Bool("summary", false, "bounded history event page and artifact references")
		after := f.Int64("after", 0, "resume after event sequence")
		limit := f.Int("limit", 100, "history page size 1..500")
		if err := parse(f, args); err != nil {
			return err
		}
		if *task == "" {
			return usage("%s requires --task", verb)
		}
		if *after < 0 || *limit < 1 || *limit > 500 {
			return fmt.Errorf("history cursor must be nonnegative and limit 1..500")
		}
		params := map[string]any{"id": *task, "include_retired": *includeRetired}
		if *summary {
			params["summary"], params["after"], params["limit"] = true, *after, *limit
		} else if *after != 0 || *limit != 100 {
			return usage("history pagination requires --summary")
		}
		return rpc(ctx, *opts, "History", "", params, out)
	case "events":
		includeRetired := f.Bool("include-retired", false, "include retired tasks")
		after := f.Int64("after", 0, "resume after sequence")
		follow := f.Bool("follow", false, "stream committed events until interrupted")
		limit := f.Int("limit", 100, "maximum events")
		if err := parse(f, args); err != nil {
			return err
		}
		if *after < 0 || *limit < 1 || *limit > 1000 {
			return fmt.Errorf("invalid event cursor or limit")
		}
		if *follow {
			return streamEvents(ctx, *opts, *after, out, *includeRetired)
		}
		return rpc(ctx, *opts, "Events", "", map[string]any{"after": *after, "limit": *limit, "include_retired": *includeRetired}, out)
	case "message":
		text := f.String("text", "", "operator text")
		file := f.String("file", "", "operator message file")
		id := f.String("id", "", "message ID")
		if err := parse(f, args); err != nil {
			return err
		}
		content, err := inputText(*text, *file)
		if err != nil {
			return err
		}
		if *id == "" {
			*id, err = identity("msg-")
			if err != nil {
				return err
			}
		}
		return rpc(ctx, *opts, "Message", "", map[string]string{"id": *id, "text": content}, out)
	case "resolve-uncertain":
		id := f.String("id", "", "task ID")
		dispatch := f.String("dispatch", "", "original dispatch ID")
		action := f.String("action", "", "redispatch, accept or escalate")
		result := f.String("result", "", "recorded completion JSON file for accept")
		if err := parse(f, args); err != nil {
			return err
		}
		if *id == "" || *dispatch == "" || opts.message == "" || (*action != "redispatch" && *action != "accept" && *action != "escalate") {
			return usage("resolve-uncertain requires id, dispatch, action and authorizing-message")
		}
		p := daemon.ResolveUncertainParams{ID: *id, DispatchID: *dispatch, Action: *action, AuthorizingMessage: opts.message}
		if *result != "" {
			b, err := os.ReadFile(*result)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(b, &p.Result); err != nil {
				return err
			}
		}
		if *action != "accept" && p.Result != nil {
			return usage("only accept permits a recorded provider result")
		}
		return rpc(ctx, *opts, "ResolveUncertain", "", p, out)
	case "runtime-config":
		limit := f.Int("pane-limit", 0, "dynamic pane limit")
		heavy := f.Int("heavy-capacity", 0, "dynamic heavy-operation capacity")
		tasks := f.Int("tasks-in-flight", 0, "dynamic task concurrency")
		host := f.String("host-thresholds", "", "JSON hostguard.Config with all host thresholds")
		if err := parse(f, args); err != nil {
			return err
		}
		if opts.message == "" || (*limit == 0 && *heavy == 0 && *tasks == 0 && *host == "") || (*limit != 0 && (*limit < 2 || *limit > 1000000)) || *heavy < 0 || *heavy > 1000000 || *tasks < 0 || *tasks > 256 {
			return usage("runtime-config requires valid limits and --authorizing-message")
		}
		c := daemon.RuntimeConfig{PaneLimit: *limit, HeavyCapacity: *heavy, TasksInFlight: *tasks, AuthorizingMessage: opts.message}
		if *host != "" {
			if err := json.Unmarshal([]byte(*host), &c.HostThresholds); err != nil || c.HostThresholds == nil {
				return usage("invalid host-thresholds JSON")
			}
		}
		return rpc(ctx, *opts, "RuntimeConfig", "", c, out)
	case "shutdown", "drain":
		if err := parse(f, args); err != nil {
			return err
		}
		if opts.message == "" {
			return fmt.Errorf("%s requires --authorizing-message", verb)
		}
		method := "Shutdown"
		if verb == "drain" {
			method = "Drain"
		}
		return rpc(ctx, *opts, method, "", map[string]string{"authorizing_message": opts.message}, out)
	case "adapter-put", "adapter-get", "adapter-list", "adapter-retire":
		return adapterCommand(ctx, verb, f, opts, args, out)
	case "target-config-put", "target-config-get", "target-config-list", "target-config-retire":
		return targetCommand(ctx, verb, f, opts, args, out)
	case "workflow-put":
		file := f.String("content", "", "workflow JSON file")
		id := f.String("workflow", "", "workflow ID (defaults to document ID)")
		_ = f.String("name", "", "display name")
		if err := parse(f, args); err != nil {
			return err
		}
		if *file == "" {
			return usage("workflow-put requires --content")
		}
		if opts.message == "" {
			return fmt.Errorf("workflow-put requires --authorizing-message")
		}
		raw, err := readBounded(*file)
		if err != nil {
			return err
		}
		doc, err := workflow.Parse(raw)
		if err != nil {
			return err
		}
		if *id == "" {
			*id = doc.ID
		}
		if *id != doc.ID {
			return fmt.Errorf("workflow ID differs from document ID")
		}
		return rpc(ctx, *opts, "WorkflowInstall", "", map[string]any{"id": *id, "document": doc, "authorizing_message": opts.message}, out)
	case "run", "start-work-item":
		prompt := f.String("prompt", "", "task content")
		file := f.String("file", "", "task content file")
		title := f.String("title", "", "task title")
		id := f.String("id", "", "task ID")
		target := f.String("target-config", "", "target configuration selector")
		targetRev := f.Int64("target-revision", 0, "target revision; zero selects latest")
		wf := f.String("workflow", "", "workflow selector")
		rev := f.Int("revision", 0, "pinned revision; 0 means latest")
		critical := f.Bool("critical", false, "protect task from timed deployment overlap overrides")
		priority := f.Int("priority", 10, "priority >=10")
		runID := f.String("run-id", "", "run ID")
		mode := f.String("mode", "local", "local or normal (dry-run unless --commit-external)")
		commitExternal := f.Bool("commit-external", false, "allow pinned external effects")
		ticketRoom := f.String("ticket-room", "", "pinned ticket room")
		destination := f.String("destination", "", "pinned review destination")
		pushNamespace := f.String("push-namespace", "", "pinned push namespace")
		key := f.String("idempotence-key", "", "admission key")
		if err := parse(f, args); err != nil {
			return err
		}
		if opts.message == "" {
			return fmt.Errorf("run requires --authorizing-message")
		}
		if *priority < 10 || *rev < 0 {
			return fmt.Errorf("priority must be >=10 and revision nonnegative")
		}
		if *mode != "local" && *mode != "normal" {
			return fmt.Errorf("mode must be local or normal")
		}
		content, err := inputText(*prompt, *file)
		if err != nil {
			return err
		}
		if *id == "" && *key != "" {
			sum := sha256.Sum256([]byte(*key))
			*id = "task-" + hex.EncodeToString(sum[:16])
		}
		if *id == "" {
			*id, err = identity("task-")
			if err != nil {
				return err
			}
		}
		if *key == "" {
			*key = *id
		}
		return rpc(ctx, *opts, "Admit", *key, map[string]any{"id": *id, "workflow_id": *wf, "revision": *rev, "target_config": *target, "target_revision": *targetRev, "title": *title, "content": content, "critical": *critical, "priority": *priority, "run_id": *runID, "mode": map[string]string{"local": "Local", "normal": "Normal"}[*mode], "commit_external": *commitExternal, "ticket_room": *ticketRoom, "destination": *destination, "push_namespace": *pushNamespace, "authorizing_message": opts.message}, out)
	default:
		return usage("unsupported command %q; run fae help", verb)
	}
}
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("input exceeds 1 MiB")
	}
	return raw, nil
}
func inputText(text, file string) (string, error) {
	if (text == "") == (file == "") {
		return "", usage("provide exactly one of --prompt/--text or --file")
	}
	if file != "" {
		raw, err := readBounded(file)
		if err != nil {
			return "", err
		}
		text = string(raw)
	}
	if len(text) == 0 || len(text) > 1<<20 {
		return "", fmt.Errorf("content must contain 1..1048576 bytes")
	}
	return text, nil
}
func readCredential(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 32 {
		return nil, fmt.Errorf("unsafe credential file")
	}
	b := make([]byte, 32)
	_, err = io.ReadFull(f, b)
	return b, err
}
func rpc(ctx context.Context, c common, method, key string, params any, out io.Writer) error {
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		return err
	}
	credential, err := readCredential(layout.Credential)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return unreachable(err)
		}
		return err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	raw, err = domain.CanonicalJSON(raw)
	if err != nil {
		return err
	}
	result, err := api.Call(ctx, layout.Socket, credential, api.Request{Version: api.Version, Method: method, Key: key, Params: raw})
	if err != nil {
		var neterr *net.OpError
		if errors.As(err, &neterr) {
			return unreachable(err)
		}
		return err
	}
	if result.Version != api.Version {
		return fmt.Errorf("unsupported RPC response version %d", result.Version)
	}
	return jsonOut(out, result.Result)
}

// streamEvents is read-only: it never starts a daemon or creates credentials.
func streamEvents(ctx context.Context, c common, after int64, out io.Writer, include ...bool) error {
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		return err
	}
	credential, err := readCredential(layout.Credential)
	if err != nil {
		if os.IsNotExist(err) {
			return unreachable(err)
		}
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", layout.Socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, "GET", "http://unix/v1/events?after="+strconv.FormatInt(after, 10)+"&include_retired="+strconv.FormatBool(len(include) > 0 && include[0]), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", hex.EncodeToString(credential))
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		var network *net.OpError
		if errors.As(err, &network) {
			return unreachable(err)
		}
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream HTTP status %d", response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var event json.RawMessage
		err = decoder.Decode(&event)
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, string(event)); err != nil {
			return err
		}
	}
}
