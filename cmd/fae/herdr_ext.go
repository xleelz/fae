package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/domain"
)

const herdrUsage = "snapshot [--json] | label-sync [--hc-bin PATH] [--json] | watch [--out PATH] [--interval D] [--once] [--hc-bin PATH] [--json]"

func herdrCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("herdr requires a subcommand: %s", herdrUsage)
	}
	if isHelp(args[0]) {
		return subcommandHelp("herdr", herdrUsage, c.json || wantsJSON(args), out)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "snapshot":
		f, opts := flags("herdr snapshot", c)
		if err := parse(f, rest); err != nil {
			return err
		}
		snap := gatherHerdrSnapshot(ctx, *opts)
		return jsonOut(out, snap)
	case "label-sync":
		f, opts := flags("herdr label-sync", c)
		hcBin := f.String("hc-bin", "", "herdr-core executable path (default: lookup herdr-core or hc on PATH)")
		if err := parse(f, rest); err != nil {
			return err
		}
		snap := gatherHerdrSnapshot(ctx, *opts)
		synced := syncHerdrLabelsFromSnapshot(ctx, *opts, snap, strings.TrimSpace(*hcBin))
		reachable, _ := snap["reachable"].(bool)
		return jsonOut(out, map[string]any{
			"instance":  opts.instance,
			"reachable": reachable,
			"synced":    synced,
		})
	case "watch":
		f, opts := flags("herdr watch", c)
		defaultOut := defaultHerdrSnapshotPath(c.home)
		outPath := f.String("out", defaultOut, "output path for atomic JSON snapshot")
		interval := f.Duration("interval", 2*time.Second, "poll interval between snapshots")
		once := f.Bool("once", false, "write one snapshot and exit")
		hcBin := f.String("hc-bin", "", "herdr-core executable path")
		if err := parse(f, rest); err != nil {
			return err
		}
		target := strings.TrimSpace(*outPath)
		if target == "" {
			target = defaultHerdrSnapshotPath(opts.home)
		}
		period := *interval
		if period <= 0 {
			period = 2 * time.Second
		}
		writePass := func() (map[string]any, int, error) {
			snap := gatherHerdrSnapshot(ctx, *opts)
			synced := syncHerdrLabelsFromSnapshot(ctx, *opts, snap, strings.TrimSpace(*hcBin))
			snap["synced_labels"] = synced
			if err := writeAtomicJSON(target, snap); err != nil {
				return snap, synced, err
			}
			return snap, synced, nil
		}
		snap, synced, err := writePass()
		if err != nil {
			return err
		}
		if *once {
			return jsonOut(out, map[string]any{
				"ok":        true,
				"instance":  opts.instance,
				"out":       target,
				"reachable": snap["reachable"],
				"synced":    synced,
			})
		}
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				_, _, _ = writePass()
			}
		}
	default:
		return usage("herdr: unknown subcommand %q (expected snapshot, label-sync or watch); run fae herdr --help", sub)
	}
}

func defaultHerdrSnapshotPath(fallbackHome string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".config", "herdr-app", "data", "fae", "snapshot.json")
	}
	if strings.TrimSpace(fallbackHome) != "" {
		return filepath.Join(fallbackHome, "herdr-snapshot.json")
	}
	return "/var/tmp/fae-herdr-snapshot.json"
}

func writeAtomicJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func callDaemonRaw(ctx context.Context, layout daemon.Layout, credential []byte, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	raw, err = domain.CanonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := api.Call(cctx, layout.Socket, credential, api.Request{
		Version: api.Version,
		Method:  method,
		Params:  raw,
	})
	if err != nil {
		return nil, err
	}
	if resp.Version != api.Version {
		return nil, fmt.Errorf("unsupported RPC response version %d", resp.Version)
	}
	if len(resp.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return resp.Result, nil
}

func gatherHerdrSnapshot(ctx context.Context, c common) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	snap := map[string]any{
		"instance":  c.instance,
		"home":      c.home,
		"timestamp": now,
		"reachable": false,
	}
	if strings.TrimSpace(c.home) == "" {
		snap["error"] = "instance home is not configured"
		return snap
	}
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		snap["error"] = err.Error()
		return snap
	}
	credential, err := readCredential(layout.Credential)
	if err != nil {
		snap["error"] = dialError(err).Error()
		return snap
	}
	statusRaw, err := callDaemonRaw(ctx, layout, credential, "Status", map[string]any{
		"include_retired": false,
		"tasks":           true,
	})
	if err != nil {
		snap["error"] = dialError(err).Error()
		return snap
	}

	readErrors := map[string]string{}
	callOrEmpty := func(method string, params any) json.RawMessage {
		res, e := callDaemonRaw(ctx, layout, credential, method, params)
		if e != nil {
			readErrors[method] = e.Error()
			return json.RawMessage(`null`)
		}
		if len(res) == 0 {
			readErrors[method] = "empty RPC result"
			return json.RawMessage(`null`)
		}
		return res
	}

	snap["reachable"] = true
	snap["status"] = statusRaw
	snap["board"] = callOrEmpty("Board", daemon.BoardParams{Limit: 200, IncludeRetired: false})
	snap["molecules"] = callOrEmpty("Topology", map[string]any{"limit": 100, "include_retired": false})
	snap["inbox"] = callOrEmpty("Inbox", daemon.InboxParams{Limit: 100, PerThread: 20})
	snap["pools"] = callOrEmpty("ProviderHealth", map[string]any{"window_seconds": 900})
	snap["pauses"] = callOrEmpty("DispatchPauseList", struct{}{})
	snap["leases"] = callOrEmpty("Leases", map[string]any{})
	snap["panes"] = callOrEmpty("Panes", map[string]any{})
	snap["host"] = callOrEmpty("HostMetrics", map[string]any{})
	snap["alerts"] = callOrEmpty("Alerts", daemon.AlertsParams{Limit: 50, Unacknowledged: true})
	snap["workflows"] = callOrEmpty("WorkflowList", daemon.WorkflowParams{})
	snap["events"] = callOrEmpty("Events", map[string]any{"after": 0, "limit": 50, "include_retired": false})
	snap["symphony"] = extractSymphonySummary(statusRaw)
	snap["partial"] = len(readErrors) > 0
	if len(readErrors) > 0 {
		snap["errors"] = readErrors
	}
	return snap
}

func extractSymphonySummary(statusRaw json.RawMessage) any {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(statusRaw, &parsed); err != nil {
		return map[string]any{"configured": false}
	}
	if sym, ok := parsed["symphony"]; ok && len(sym) > 0 && string(sym) != "null" {
		return sym
	}
	out := map[string]any{"configured": false}
	if outbox, ok := parsed["ticket_outbox"]; ok && len(outbox) > 0 {
		out["ticket_outbox"] = outbox
	}
	return out
}

type herdrTaskMeta struct {
	ID         string
	State      string
	MoleculeID string
	Stage      string
	Role       string
	Binding    string
	Session    string
}

func syncHerdrLabelsFromSnapshot(ctx context.Context, c common, snap map[string]any, hcBin string) int {
	reachable, _ := snap["reachable"].(bool)
	if !reachable {
		return 0
	}
	bin := hcBin
	if bin == "" {
		if p, err := exec.LookPath("herdr-core"); err == nil {
			bin = p
		} else if p, err := exec.LookPath("hc"); err == nil {
			bin = p
		}
	}
	if bin == "" {
		return 0
	}

	tasksByID := map[string]*herdrTaskMeta{}
	sessionsByID := map[string]*herdrTaskMeta{}

	upsertTask := func(id, state, mol string) *herdrTaskMeta {
		if id == "" {
			return nil
		}
		m, ok := tasksByID[id]
		if !ok {
			m = &herdrTaskMeta{ID: id}
			tasksByID[id] = m
		}
		if state != "" {
			m.State = state
		}
		if mol != "" {
			m.MoleculeID = mol
		}
		return m
	}

	if rawStatus, ok := snap["status"].(json.RawMessage); ok {
		var st struct {
			Tasks []struct {
				ID         string `json:"id"`
				State      string `json:"state"`
				MoleculeID string `json:"molecule_id"`
			} `json:"tasks"`
		}
		if json.Unmarshal(rawStatus, &st) == nil {
			for _, t := range st.Tasks {
				upsertTask(t.ID, t.State, t.MoleculeID)
			}
		}
	}

	if rawBoard, ok := snap["board"].(json.RawMessage); ok {
		var bd struct {
			Items []struct {
				ID         string `json:"id"`
				State      string `json:"state"`
				MoleculeID string `json:"molecule_id"`
			} `json:"items"`
			Tasks []struct {
				ID         string `json:"id"`
				State      string `json:"state"`
				MoleculeID string `json:"molecule_id"`
			} `json:"tasks"`
		}
		if json.Unmarshal(rawBoard, &bd) == nil {
			for _, t := range bd.Items {
				upsertTask(t.ID, t.State, t.MoleculeID)
			}
			for _, t := range bd.Tasks {
				upsertTask(t.ID, t.State, t.MoleculeID)
			}
		}
	}

	var activeFallback *herdrTaskMeta
	if rawPanes, ok := snap["panes"].(json.RawMessage); ok {
		var pn struct {
			Sessions []struct {
				Session string `json:"session"`
				Role    string `json:"role"`
				Task    string `json:"task"`
				Stage   string `json:"stage"`
				Binding string `json:"binding"`
			} `json:"sessions"`
		}
		if json.Unmarshal(rawPanes, &pn) == nil {
			for _, s := range pn.Sessions {
				tm := upsertTask(s.Task, "", "")
				if tm == nil {
					tm = &herdrTaskMeta{ID: s.Task}
				}
				if s.Stage != "" {
					tm.Stage = s.Stage
				}
				if s.Role != "" {
					tm.Role = s.Role
				}
				if s.Binding != "" {
					tm.Binding = s.Binding
				}
				if s.Session != "" {
					tm.Session = s.Session
					sessionsByID[s.Session] = tm
				}
				if activeFallback == nil {
					activeFallback = tm
				}
			}
		}
	}

	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lsOut, err := exec.CommandContext(cctx, bin, "ls", "--json").Output()
	if err != nil {
		return 0
	}
	var listing struct {
		Terminals []struct {
			Address string            `json:"address"`
			Name    string            `json:"name"`
			State   string            `json:"state"`
			Cwd     string            `json:"cwd"`
			Labels  map[string]string `json:"labels"`
		} `json:"terminals"`
	}
	if json.Unmarshal(lsOut, &listing) != nil {
		return 0
	}

	synced := 0
	for _, term := range listing.Terminals {
		if term.Address == "" || (term.State != "" && term.State != "live") {
			continue
		}
		var matched *herdrTaskMeta
		if taskID := term.Labels["fae.task"]; taskID != "" {
			matched = tasksByID[taskID]
			if matched == nil {
				matched = &herdrTaskMeta{ID: taskID}
			}
		}
		if matched == nil && term.Name != "" {
			if m, ok := sessionsByID[term.Name]; ok {
				matched = m
			} else if m, ok := tasksByID[term.Name]; ok {
				matched = m
			}
		}
		if matched == nil && term.Cwd != "" {
			for id, m := range tasksByID {
				if strings.HasSuffix(term.Cwd, "/"+id) || strings.Contains(term.Cwd, "/"+id+"/") {
					matched = m
					break
				}
			}
		}
		if matched == nil && (term.Labels["fae.instance"] == c.instance || term.Name == c.instance) && activeFallback != nil {
			matched = activeFallback
		}
		if matched == nil {
			continue
		}

		kvMap := map[string]string{
			"fae.instance": c.instance,
			"herdr.space":  c.instance,
		}
		if matched.ID != "" {
			kvMap["fae.task"] = matched.ID
		}
		if matched.State != "" {
			kvMap["fae.state"] = matched.State
		}
		if matched.Stage != "" {
			kvMap["fae.stage"] = matched.Stage
		}
		if matched.Role != "" {
			kvMap["fae.role"] = matched.Role
		}
		if matched.Binding != "" {
			kvMap["fae.binding"] = matched.Binding
		}
		if matched.MoleculeID != "" {
			kvMap["fae.molecule"] = matched.MoleculeID
			kvMap["herdr.tab"] = matched.MoleculeID
		}

		keys := make([]string, 0, len(kvMap))
		for k, v := range kvMap {
			if k != "" && v != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		cmdArgs := make([]string, 0, len(keys)+3)
		cmdArgs = append(cmdArgs, "label", term.Address)
		for _, k := range keys {
			cmdArgs = append(cmdArgs, k+"="+kvMap[k])
		}
		cmdArgs = append(cmdArgs, "--json")

		lctx, lcancel := context.WithTimeout(ctx, time.Second)
		err := exec.CommandContext(lctx, bin, cmdArgs...).Run()
		lcancel()
		if err == nil {
			synced++
		}
	}
	return synced
}
