package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
)

func init() {
	actionPermissions["import_legacy"] = actionPermission{Class: "task"}
	rpcParams["import_legacy"] = ImportLegacyParams{}
}

// LegacyTaskEntry represents one validated legacy task from an export file (FAE-CLI-051).
type LegacyTaskEntry struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Goal        string   `json:"goal"`
	Description string   `json:"description,omitempty"`
	Workflow    string   `json:"workflow,omitempty"`
	Scope       []string `json:"scope,omitempty"`
	Files       []string `json:"files,omitempty"`
	Priority    int      `json:"priority"`
}

// ImportLegacyParams holds the parameters for the "import_legacy" daemon RPC (FAE-CLI-051).
type ImportLegacyParams struct {
	From               string            `json:"from,omitempty"`
	Source             string            `json:"-"`
	DryRun             bool              `json:"dry_run,omitempty"`
	Tasks              []LegacyTaskEntry `json:"tasks,omitempty"`
	AuthorizingMessage string            `json:"authorizing_message,omitempty"`
}

// ImportLegacyResult is the response returned by the "import_legacy" daemon RPC (FAE-CLI-051).
type ImportLegacyResult struct {
	DryRun    bool              `json:"dry_run"`
	Count     int               `json:"count"`
	Imported  int               `json:"imported"`
	Skipped   int               `json:"skipped"`
	TaskIDs   []string          `json:"task_ids"`
	AuditFile string            `json:"audit_file,omitempty"`
	Tasks     []LegacyTaskEntry `json:"tasks,omitempty"`
}

// ImportLegacyAuditRecord is written to import-legacy-audit.json in the instance root (FAE-CLI-051).
type ImportLegacyAuditRecord struct {
	ImportedAt         time.Time `json:"imported_at"`
	Source             string    `json:"source"`
	Imported           int       `json:"imported"`
	Skipped            int       `json:"skipped"`
	TaskIDs            []string  `json:"task_ids"`
	AuthorizingMessage string    `json:"authorizing_message,omitempty"`
}

type rawLegacyTaskEntry struct {
	ID            string          `json:"id"`
	TaskID        string          `json:"task_id"`
	Title         string          `json:"title"`
	Goal          string          `json:"goal"`
	Description   string          `json:"description"`
	Content       string          `json:"content"`
	Prompt        string          `json:"prompt"`
	Workflow      string          `json:"workflow"`
	WorkflowID    string          `json:"workflow_id"`
	Scope         json.RawMessage `json:"scope"`
	Files         json.RawMessage `json:"files"`
	DeclaredFiles json.RawMessage `json:"declared_files"`
	Priority      *int            `json:"priority"`
}

// ParseLegacyTasksFile reads and validates a legacy task export file at path.
// It supports JSON object {"tasks": [...]}, JSON array [...], or JSONL lines.
func ParseLegacyTasksFile(path string) ([]LegacyTaskEntry, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("legacy import path (--from) is required")
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("read legacy task export %s: %w", trimmed, err)
	}
	return ParseLegacyTasksBytes(raw)
}

// ParseLegacyTasksBytes parses and validates raw bytes of a legacy task export.
func ParseLegacyTasksBytes(raw []byte) ([]LegacyTaskEntry, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("legacy task export is empty")
	}

	var rawEntries []rawLegacyTaskEntry
	switch trimmed[0] {
	case '[':
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		if err := dec.Decode(&rawEntries); err != nil {
			return nil, fmt.Errorf("parse legacy task JSON array: %w", err)
		}
	case '{':
		// First check if it is a single wrapper object {"tasks": [...]}.
		var wrapper struct {
			Tasks *[]rawLegacyTaskEntry `json:"tasks"`
			Items *[]rawLegacyTaskEntry `json:"items"`
		}
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		if err := dec.Decode(&wrapper); err == nil && (wrapper.Tasks != nil || wrapper.Items != nil) {
			var extra any
			if dec.Decode(&extra) == nil {
				// Multiple JSON objects -> treat as JSONL below.
				rawEntries = nil
			} else if wrapper.Tasks != nil {
				rawEntries = *wrapper.Tasks
			} else {
				rawEntries = *wrapper.Items
			}
		}
		if rawEntries == nil && wrapper.Tasks == nil && wrapper.Items == nil {
			entries, err := parseLegacyJSONL(trimmed)
			if err != nil {
				return nil, err
			}
			rawEntries = entries
		}
	default:
		return nil, errors.New("legacy task export must be a JSON object {\"tasks\": [...]}, JSON array [...], or JSONL")
	}

	if len(rawEntries) == 0 {
		return nil, errors.New("legacy task export contains no tasks")
	}

	out := make([]LegacyTaskEntry, 0, len(rawEntries))
	seenIDs := make(map[string]bool, len(rawEntries))
	for idx, r := range rawEntries {
		entry, err := normalizeAndValidateLegacyEntry(idx, r)
		if err != nil {
			return nil, err
		}
		if seenIDs[entry.ID] {
			return nil, fmt.Errorf("legacy task entry %d: duplicate task id %q", idx+1, entry.ID)
		}
		seenIDs[entry.ID] = true
		out = append(out, entry)
	}
	return out, nil
}

func parseLegacyJSONL(raw []byte) ([]rawLegacyTaskEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []rawLegacyTaskEntry
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var item rawLegacyTaskEntry
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, fmt.Errorf("parse legacy JSONL line %d: %w", lineNum, err)
		}
		out = append(out, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan legacy JSONL: %w", err)
	}
	return out, nil
}

func parseLegacyFileSlice(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var slice []string
	if err := json.Unmarshal(trimmed, &slice); err == nil {
		return slice, nil
	}
	var single string
	if err := json.Unmarshal(trimmed, &single); err == nil {
		var parts []string
		for _, p := range strings.Split(single, ",") {
			if s := strings.TrimSpace(p); s != "" {
				parts = append(parts, s)
			}
		}
		return parts, nil
	}
	return nil, errors.New("scope/files must be a string array or comma-separated string")
}

func normalizeAndValidateLegacyEntry(idx int, r rawLegacyTaskEntry) (LegacyTaskEntry, error) {
	title := strings.TrimSpace(r.Title)
	goal := strings.TrimSpace(r.Goal)
	desc := strings.TrimSpace(r.Description)
	if goal == "" {
		switch {
		case desc != "":
			goal = desc
		case strings.TrimSpace(r.Content) != "":
			goal = strings.TrimSpace(r.Content)
		case strings.TrimSpace(r.Prompt) != "":
			goal = strings.TrimSpace(r.Prompt)
		}
	}
	if title == "" {
		return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d: title is required", idx+1)
	}
	if goal == "" {
		return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d (%q): goal or description is required", idx+1, title)
	}

	id := strings.TrimSpace(r.ID)
	if id == "" {
		id = strings.TrimSpace(r.TaskID)
	}
	if id == "" {
		id = deriveLegacyTaskID(idx, title, goal)
	}
	if len(id) > 255 {
		return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d: id exceeds 255 bytes", idx+1)
	}

	wf := strings.TrimSpace(r.Workflow)
	if wf == "" {
		wf = strings.TrimSpace(r.WorkflowID)
	}
	if wf == "" {
		wf = defaults.Workflow().ID
	}

	scopeList, err := parseLegacyFileSlice(r.Scope)
	if err != nil {
		return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d (%s): %w", idx+1, id, err)
	}
	filesList, err := parseLegacyFileSlice(r.Files)
	if err != nil {
		return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d (%s): %w", idx+1, id, err)
	}
	declaredList, err := parseLegacyFileSlice(r.DeclaredFiles)
	if err != nil {
		return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d (%s): %w", idx+1, id, err)
	}

	var combined []string
	seenFiles := make(map[string]bool)
	for _, list := range [][]string{scopeList, filesList, declaredList} {
		for _, f := range list {
			clean := strings.TrimSpace(f)
			if clean == "" {
				continue
			}
			if !DeclaredPathSafe(clean) {
				return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d (%s): invalid scope/file path %q", idx+1, id, clean)
			}
			if !seenFiles[clean] {
				seenFiles[clean] = true
				combined = append(combined, clean)
			}
		}
	}

	priority := 10
	if r.Priority != nil {
		if *r.Priority < 10 {
			return LegacyTaskEntry{}, fmt.Errorf("legacy task entry %d (%s): priority must be >= 10 (FAE-CLI-002), got %d", idx+1, id, *r.Priority)
		}
		priority = *r.Priority
	}

	return LegacyTaskEntry{
		ID:          id,
		Title:       title,
		Goal:        goal,
		Description: desc,
		Workflow:    wf,
		Scope:       combined,
		Files:       combined,
		Priority:    priority,
	}, nil
}

func deriveLegacyTaskID(idx int, title string, goal string) string {
	slug := strings.ToLower(SanitizeWorkspaceKey(title))
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "_-.")
	}
	if slug == "" {
		slug = fmt.Sprintf("task-%d", idx+1)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", idx, title, goal)))
	return fmt.Sprintf("legacy-%s-%s", slug, hex.EncodeToString(sum[:4]))
}

func resolveImportLegacyTasks(p *ImportLegacyParams) ([]LegacyTaskEntry, error) {
	srcPath := strings.TrimSpace(p.Source)
	if srcPath == "" {
		srcPath = strings.TrimSpace(p.From)
	}
	if srcPath != "" {
		p.From = srcPath
		p.Source = srcPath
		return ParseLegacyTasksFile(srcPath)
	}
	if len(p.Tasks) > 0 {
		raw, marshalErr := json.Marshal(p.Tasks)
		if marshalErr != nil {
			return nil, marshalErr
		}
		return ParseLegacyTasksBytes(raw)
	}
	return nil, errors.New("import_legacy requires source or from path")
}

// ImportLegacyOffline performs a one-way, idempotent legacy import directly
// against an existing stopped instance plane while holding the instance lock (FAE-CLI-051).
// It never provisions a new plane.
func ImportLegacyOffline(ctx context.Context, home string, instance string, p ImportLegacyParams) (ImportLegacyResult, error) {
	tasks, err := resolveImportLegacyTasks(&p)
	if err != nil {
		return ImportLegacyResult{}, err
	}
	if p.DryRun {
		taskIDs := make([]string, 0, len(tasks))
		for _, t := range tasks {
			taskIDs = append(taskIDs, t.ID)
		}
		return ImportLegacyResult{
			DryRun:   true,
			Count:    len(tasks),
			Imported: 0,
			Skipped:  0,
			TaskIDs:  taskIDs,
			Tasks:    tasks,
		}, nil
	}

	if strings.TrimSpace(instance) == "" {
		instance = "default"
	}
	l, err := Paths(home, instance)
	if err != nil {
		return ImportLegacyResult{}, err
	}
	doltDir := filepath.Join(l.Data, ".dolt")
	if info, statErr := os.Stat(doltDir); statErr != nil || !info.IsDir() {
		return ImportLegacyResult{}, fmt.Errorf("instance plane does not exist at %s (import-legacy never provisions a new plane)", l.Data)
	}

	var binding struct {
		Workspace string `json:"workspace"`
	}
	if rawBind, bindErr := os.ReadFile(l.Binding); bindErr == nil {
		_ = json.Unmarshal(rawBind, &binding)
	}

	unlockLifecycle, err := lifecycleLock(ctx, home, instance)
	if err != nil {
		return ImportLegacyResult{}, err
	}
	defer unlockLifecycle()

	lock, err := acquire(filepath.Join(l.Run, "lock"))
	if err != nil {
		return ImportLegacyResult{}, err
	}
	defer lock.close()

	doltBinary := strings.TrimSpace(os.Getenv("FAE_TEST_DOLT_BINARY"))
	if rawServeArgs, argsErr := os.ReadFile(filepath.Join(l.Root, "serve-args.json")); argsErr == nil {
		var serveArgs []string
		if json.Unmarshal(rawServeArgs, &serveArgs) == nil {
			for i := 0; i < len(serveArgs); i++ {
				if serveArgs[i] == "--dolt" && i+1 < len(serveArgs) && strings.TrimSpace(serveArgs[i+1]) != "" {
					doltBinary = strings.TrimSpace(serveArgs[i+1])
					break
				}
				if after, ok := strings.CutPrefix(serveArgs[i], "--dolt="); ok && strings.TrimSpace(after) != "" {
					doltBinary = strings.TrimSpace(after)
					break
				}
			}
		}
	}
	if doltBinary == "" {
		doltBinary = "dolt"
	}

	ownctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childctx, childcancel := context.WithCancel(context.Background())
	defer childcancel()
	drainctx, draincancel := context.WithCancel(context.Background())
	defer draincancel()

	c := Config{
		Root:                 home,
		Instance:             instance,
		Workspace:            binding.Workspace,
		DoltBinary:           doltBinary,
		RunID:                "backlog-drain",
		MaxInFlight:          -1,
		MaxMolecules:         -1,
		MaxWorkflowMolecules: -1,
		MessageMaxAge:        24 * time.Hour,
	}
	d := &Daemon{
		drainCtx:    drainctx,
		drainCancel: draincancel,
		quarantined: map[string]string{},
		config:      c,
		layout:      l,
		lock:        lock,
		ctx:         ownctx,
		cancel:      cancel,
		childCtx:    childctx,
		childCancel: childcancel,
		jobs:        map[string]controlplane.JobSnapshot{},
		active:      map[string]bool{},
		wake:        make(chan struct{}, 1),
		results:     make(chan completion, 1),
		done:        make(chan struct{}),
		reads:       make(chan struct{}, 8),
		eventWake:   make(chan struct{}),
		subscribers: make(chan struct{}, 16),
	}
	d.log, err = openSupervisionLog(l.Log, c.Instance, supervisionStderr)
	if err != nil {
		return ImportLegacyResult{}, err
	}
	defer d.log.Close()

	if err := d.startPlane(ctx); err != nil {
		return ImportLegacyResult{}, err
	}
	defer func() {
		if d.store != nil {
			_ = d.store.Close()
		}
		d.stopPlane()
	}()

	snapshots, err := d.loadActiveJobs(ctx)
	if err != nil {
		return ImportLegacyResult{}, err
	}
	for _, s := range snapshots {
		d.jobs[s.ID] = s
	}
	if err := d.installDefault(ctx); err != nil {
		return ImportLegacyResult{}, err
	}
	if strings.TrimSpace(p.AuthorizingMessage) != "" {
		ctx, err = d.store.AuthorizeAction(ctx, p.AuthorizingMessage, "operator", "import_legacy", "task", false, actionClasses())
		if err != nil {
			return ImportLegacyResult{}, err
		}
	}
	rawParams, _ := json.Marshal(p)
	audit := controlplane.Audit{
		Actor:     "operator",
		Action:    "import_legacy",
		MessageID: p.AuthorizingMessage,
		Inputs:    rawParams,
		Outcome:   "ok",
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.importLegacyLocked(ctx, p, tasks, audit)
}

func (d *Daemon) importLegacyRPC(ctx context.Context, r api.Request, principal api.Principal) (any, error) {
	var p ImportLegacyParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	tasks, err := resolveImportLegacyTasks(&p)
	if err != nil {
		return nil, err
	}

	if p.DryRun {
		taskIDs := make([]string, 0, len(tasks))
		for _, t := range tasks {
			taskIDs = append(taskIDs, t.ID)
		}
		return ImportLegacyResult{
			DryRun:   true,
			Count:    len(tasks),
			Imported: 0,
			Skipped:  0,
			TaskIDs:  taskIDs,
			Tasks:    tasks,
		}, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.publishStatus()

	if err := d.mutationRefusal("import_legacy", false); err != nil {
		return nil, err
	}
	d.awaitCheckpoint()
	if err := d.reconcileBacklogCheckpoint(ctx); err != nil {
		return nil, fmt.Errorf("backlog outcome unresolved: %w", err)
	}

	if strings.TrimSpace(p.AuthorizingMessage) != "" {
		var authErr error
		ctx, authErr = d.store.AuthorizeAction(ctx, p.AuthorizingMessage, principal.Actor, "import_legacy", "task", false, actionClasses())
		if authErr != nil {
			return nil, authErr
		}
	}

	audit := controlplane.Audit{
		Actor:     principal.Actor,
		Action:    "import_legacy",
		MessageID: p.AuthorizingMessage,
		Inputs:    r.Params,
		Outcome:   "ok",
	}

	return d.importLegacyLocked(ctx, p, tasks, audit)
}

func (d *Daemon) importLegacyLocked(ctx context.Context, p ImportLegacyParams, tasks []LegacyTaskEntry, audit controlplane.Audit) (ImportLegacyResult, error) {
	tree := d.resolveImportTree(ctx)
	var toAdmit []BacklogItemParams
	var importedIDs []string
	skipped := 0

	sourcePath := strings.TrimSpace(p.Source)
	if sourcePath == "" {
		sourcePath = strings.TrimSpace(p.From)
	}

	for _, t := range tasks {
		exists, err := d.taskExistsLocked(ctx, t.ID)
		if err != nil {
			return ImportLegacyResult{}, err
		}
		if exists {
			skipped++
			continue
		}
		wid := t.Workflow
		if wid == "" {
			wid = defaults.Workflow().ID
		}
		if _, _, wfErr := d.storedWorkflow(ctx, wid, 0); wfErr != nil {
			wid = defaults.Workflow().ID
		}
		admitPriority := t.Priority
		if admitPriority < 10 {
			return ImportLegacyResult{}, fmt.Errorf("legacy task %q: priority must be >= 10 (FAE-CLI-002), got %d", t.ID, admitPriority)
		}
		sourceRef := "legacy-import"
		if sourcePath != "" {
			sourceRef = "legacy:" + sourcePath
		}
		toAdmit = append(toAdmit, BacklogItemParams{
			AdmitParams: AdmitParams{
				ID:                 t.ID,
				Title:              t.Title,
				Content:            t.Goal,
				WorkflowID:         wid,
				Priority:           admitPriority,
				DeclaredFiles:      t.Scope,
				Tree:               tree,
				AuthorizingMessage: p.AuthorizingMessage,
			},
			Source: sourceRef,
		})
		importedIDs = append(importedIDs, t.ID)
	}

	if len(toAdmit) > 0 {
		_, _, err := d.admitBacklogBatch(ctx, BacklogParams{
			Items:              toAdmit,
			AuthorizingMessage: p.AuthorizingMessage,
		}, audit, backlogBatch{via: "import_legacy", create: true})
		if err != nil {
			return ImportLegacyResult{}, err
		}
	}

	if importedIDs == nil {
		importedIDs = []string{}
	}
	markerPayload, _ := json.Marshal(map[string]any{
		"from":     sourcePath,
		"source":   sourcePath,
		"imported": len(importedIDs),
		"skipped":  skipped,
		"task_ids": importedIDs,
	})
	if err := d.store.PutMarker(ctx, audit, "last-import-legacy", markerPayload); err != nil {
		return ImportLegacyResult{}, err
	}
	d.lifecycle("import_legacy", map[string]any{
		"from":     sourcePath,
		"source":   sourcePath,
		"imported": len(importedIDs),
		"skipped":  skipped,
		"task_ids": importedIDs,
	})

	var auditFilePath string
	if d.layout.Root != "" {
		auditFilePath = filepath.Join(d.layout.Root, "import-legacy-audit.json")
		auditRec := ImportLegacyAuditRecord{
			ImportedAt:         time.Now().UTC(),
			Source:             sourcePath,
			Imported:           len(importedIDs),
			Skipped:            skipped,
			TaskIDs:            importedIDs,
			AuthorizingMessage: p.AuthorizingMessage,
		}
		rawAudit, marshalErr := json.MarshalIndent(auditRec, "", "  ")
		if marshalErr != nil {
			return ImportLegacyResult{}, marshalErr
		}
		if err := writePrivate(auditFilePath, append(rawAudit, '\n')); err != nil {
			return ImportLegacyResult{}, fmt.Errorf("write import-legacy audit file: %w", err)
		}
	}

	return ImportLegacyResult{
		DryRun:    false,
		Count:     len(tasks),
		Imported:  len(importedIDs),
		Skipped:   skipped,
		TaskIDs:   importedIDs,
		AuditFile: auditFilePath,
		Tasks:     tasks,
	}, nil
}

func (d *Daemon) taskExistsLocked(ctx context.Context, id string) (bool, error) {
	if _, held := d.jobs[id]; held || d.quarantineHeld(id) {
		return true, nil
	}
	if _, ok, err := d.storedJob(ctx, id); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	found, err := d.store.ExistingJobs(ctx, []string{id})
	if err != nil {
		return false, err
	}
	return found[id], nil
}

func (d *Daemon) resolveImportTree(ctx context.Context) string {
	if d.live != nil {
		return ""
	}
	if strings.TrimSpace(d.config.Workspace) != "" {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD^{tree}")
		cmd.Dir = d.config.Workspace
		cmd.Env = cleanGitEnv()
		if out, err := cmd.Output(); err == nil {
			if tree := strings.TrimSpace(string(out)); tree != "" {
				return tree
			}
		}
	}
	return "legacy-import"
}
