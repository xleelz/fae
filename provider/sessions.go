package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/xleelz/fae/kernel"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// SessionAudit links a disposable Fae turn to its provider context. The context
// is a cache, never a source of authoritative workflow or artifact state.
type SessionAudit struct {
	Scope      string `json:"scope"`
	ThreadID   string `json:"thread_id"`
	Resumed    bool   `json:"resumed"`
	TaskID     string `json:"task_id"`
	DispatchID string `json:"dispatch_id"`
}
type sessionState struct {
	Scope        string `json:"scope"`
	ThreadID     string `json:"thread_id"`
	Pending      bool   `json:"pending"`
	Checkpointed bool   `json:"checkpointed,omitempty"`
}
type providerSession struct {
	dir     string
	lock    *os.File
	state   sessionState
	resumed bool
}

var threadIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func privateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute session cache path required")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != filepath.Clean(path) {
		return errors.New("canonical session cache directory required")
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("private session cache directory required")
	}
	return nil
}
func openProviderSession(ctx context.Context, root string, l agentruntime.BridgeConfig) (*providerSession, error) {
	scopedPolicy := l.SessionPolicy == "workflow-global" || l.SessionPolicy == "task-bound" && l.TaskID != ""
	if root != "" && !scopedPolicy {
		return nil, errors.New("persistent provider requires a declared session scope and task identity")
	}
	if !kernel.CanResumeProvider(root != "", scopedPolicy, l.FreshContext || l.FreshRole) {
		return nil, nil
	}
	if l.WorkflowID == "" || l.WorkflowRevision == "" || l.Role == "" {
		return nil, errors.New("persistent provider requires workflow revision and role")
	}
	if err := privateDirectory(root); err != nil {
		return nil, err
	}
	// Revision includes role instructions, briefs and allowed output schemas.
	scope := []any{l.WorkflowID, l.WorkflowRevision, l.Role, l.Binding, l.Profile, l.Mode}
	// Preserve the workflow-global scope across tasks; task-bound context
	// resumes only within its exact task and is fresh for the next task.
	if l.SessionPolicy == "task-bound" {
		scope = append(scope, "task-bound", l.TaskID)
	}
	if l.BranchInstance != "" {
		scope = append(scope, "branch", l.BranchInstance)
	}
	scopeRaw, _ := json.Marshal(scope)
	sum := sha256.Sum256(scopeRaw)
	key := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, key)
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			lock.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	s := &providerSession{dir: dir, lock: lock, state: sessionState{Scope: key}}
	fail := func(err error) (*providerSession, error) { s.close(); return nil, err }
	path := filepath.Join(dir, "state.json")
	raw, err := regularRead(path, true)
	if err != nil {
		return fail(err)
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &s.state); err != nil {
			return fail(err)
		}
		if s.state.Scope != key || s.state.ThreadID != "" && !threadIDPattern.MatchString(s.state.ThreadID) {
			return fail(errors.New("invalid provider session cache"))
		}
		// A process death or failed validation may leave provider-side effects in
		// context. Rotate instead of replaying an uncertain turn into that context,
		// unless a durable FAE-RT-019 turn checkpoint recorded the session.
		if s.state.Pending && l.ResumeSession == "" && !s.state.Checkpointed {
			s.state.ThreadID = ""
		}
	}
	if l.ResumeSession != "" {
		if !threadIDPattern.MatchString(l.ResumeSession) {
			return fail(errors.New("invalid provider session cache"))
		}
		s.state.ThreadID = l.ResumeSession
	}
	s.resumed = s.state.ThreadID != ""
	s.state.Pending = true
	s.state.Checkpointed = false
	if err = s.save(); err != nil {
		return fail(err)
	}
	if err = privateDirectory(filepath.Join(dir, "cwd")); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *providerSession) close() {
	if s != nil && s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}
func (s *providerSession) save() error {
	raw, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *providerSession) checkpoint(thread string) error {
	if s == nil || !threadIDPattern.MatchString(thread) {
		return nil
	}
	if s.resumed && s.state.ThreadID != "" && thread != s.state.ThreadID {
		return errors.New("provider resumed a different thread")
	}
	s.state.ThreadID = thread
	s.state.Checkpointed = true
	return s.save()
}
func (s *providerSession) complete(thread string) error {
	if !threadIDPattern.MatchString(thread) {
		return errors.New("provider did not announce a valid thread")
	}
	if s.resumed && thread != s.state.ThreadID {
		return errors.New("provider resumed a different thread")
	}
	s.state.ThreadID = thread
	s.state.Pending = false
	s.state.Checkpointed = true
	return s.save()
}

// announcedThread consumes identity events only. Provider error classification
// remains exclusively in providerhealth's shared session-log reader.
func announcedThread(path string) (string, error) {
	raw, err := regularRead(path, false)
	if err != nil {
		return "", err
	}
	var thread string
	for _, line := range bytesLines(raw) {
		var v struct {
			Type   string `json:"type"`
			Thread string `json:"thread_id"`
		}
		if json.Unmarshal(line, &v) == nil && v.Type == "thread.started" {
			if !threadIDPattern.MatchString(v.Thread) || thread != "" && thread != v.Thread {
				return "", errors.New("invalid provider thread identity")
			}
			thread = v.Thread
		}
	}
	if thread == "" {
		return "", fmt.Errorf("provider thread identity missing in %s", path)
	}
	return thread, nil
}
func bytesLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

// Session scope and reuse policy constants (FAE-RT-015, FAE-OPS-034).
const (
	ScopeTask           = "task-bound"
	ScopeWorkflow       = "workflow-global"
	ReuseTaskBound      = "task-bound"
	ReuseWorkflowGlobal = "workflow-global"
)

// AcquireRequest describes a provider session acquisition for a task role turn.
type AcquireRequest struct {
	TaskID           string           `json:"task_id"`
	WorkflowID       string           `json:"workflow_id"`
	WorkflowRevision string           `json:"workflow_revision"`
	Role             string           `json:"role"`
	Stage            string           `json:"stage,omitempty"`
	Visit            int              `json:"visit,omitempty"`
	Scope            string           `json:"scope,omitempty"`
	Reuse            string           `json:"reuse,omitempty"`
	Fresh            bool             `json:"fresh,omitempty"`
	Provider         string           `json:"provider,omitempty"`
	Model            string           `json:"model,omitempty"`
	Binding          workflow.Binding `json:"binding,omitempty"`
	Profile          workflow.Profile `json:"profile,omitempty"`
	Mode             string           `json:"mode,omitempty"`
	Budget           *TurnBudgetSpec  `json:"budget,omitempty"`
	Now              time.Time        `json:"now,omitempty"`
}

// SessionRecord is the durable/in-memory provider session binding for a task or workflow scope.
type SessionRecord struct {
	Key              string          `json:"key"`
	Scope            string          `json:"scope"`
	TaskID           string          `json:"task_id"`
	WorkflowID       string          `json:"workflow_id"`
	WorkflowRevision string          `json:"workflow_revision"`
	Role             string          `json:"role"`
	Provider         string          `json:"provider,omitempty"`
	Model            string          `json:"model,omitempty"`
	Mode             string          `json:"mode,omitempty"`
	ThreadID         string          `json:"thread_id"`
	Fresh            bool            `json:"fresh,omitempty"`
	Reused           bool            `json:"reused"`
	TurnCount        int             `json:"turn_count"`
	Budget           *TurnBudgetSpec `json:"budget,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// SessionRegistry manages provider session keys across tasks and supports
// FAE-OPS-034 fresh-role isolation and task-bound session key transfer on FollowUp.
type SessionRegistry struct {
	mu      sync.Mutex
	root    string
	byKey   map[string]SessionRecord
	byTask  map[string]string
	counter uint64
}

// NewSessionRegistry creates a SessionRegistry optionally backed by root on disk.
func NewSessionRegistry(root string) *SessionRegistry {
	return &SessionRegistry{
		root:   root,
		byKey:  map[string]SessionRecord{},
		byTask: map[string]string{},
	}
}

func makeThreadUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-4%s-a%s-%s", h[0:8], h[8:12], h[13:16], h[17:20], h[20:32])
}

func taskBoundScopeKey(workflowID, workflowRevision, role string, binding workflow.Binding, profile workflow.Profile, mode, taskID string) string {
	scope := []any{workflowID, workflowRevision, role, binding, profile, mode, "task-bound", taskID}
	raw, _ := json.Marshal(scope)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// RefuseFollowUpIfFresh returns a typed refusal error when role has fresh: true (FAE-OPS-034).
func RefuseFollowUpIfFresh(roleName string, role workflow.Role) error {
	if role.Fresh {
		return fmt.Errorf("follow-up-refused: role %s has fresh: true", roleName)
	}
	return nil
}

// Acquire returns an existing or newly created provider SessionRecord. When
// req.Fresh is true, it never reuses a provider session created for another
// task (or prior visit), even if Reuse/Scope is workflow-global (FAE-OPS-034).
func (r *SessionRegistry) Acquire(req AcquireRequest) (SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.TaskID == "" {
		return SessionRecord{}, errors.New("task_id required to acquire provider session")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	policy := req.Reuse
	if policy == "" {
		policy = req.Scope
	}
	if policy == "" {
		policy = ScopeTask
	}
	providerName := req.Provider
	if providerName == "" {
		providerName = req.Binding.Provider
	}
	modelName := req.Model
	if modelName == "" {
		modelName = req.Binding.Model
	}
	r.counter++
	if req.Fresh {
		// FAE-OPS-034: a role with fresh: true never reuses a provider session
		// across tasks or visits, even when workflow-global is configured.
		key := fmt.Sprintf("fresh:%s:%s:%s:%s:%d:%d", req.WorkflowID, req.WorkflowRevision, req.Role, req.TaskID, req.Visit, r.counter)
		threadID := makeThreadUUID(key)
		rec := SessionRecord{
			Key:              key,
			Scope:            ScopeTask,
			TaskID:           req.TaskID,
			WorkflowID:       req.WorkflowID,
			WorkflowRevision: req.WorkflowRevision,
			Role:             req.Role,
			Provider:         providerName,
			Model:            modelName,
			Mode:             req.Mode,
			ThreadID:         threadID,
			Fresh:            true,
			Reused:           false,
			TurnCount:        1,
			Budget:           req.Budget,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		r.byKey[key] = rec
		r.byTask[req.TaskID] = key
		return rec, nil
	}
	var key string
	scope := ScopeTask
	if policy == ReuseWorkflowGlobal {
		scope = ScopeWorkflow
		key = fmt.Sprintf("workflow-global:%s:%s:%s:%s:%s:%s", req.WorkflowID, req.WorkflowRevision, req.Role, providerName, modelName, req.Mode)
	} else {
		if existingKey, ok := r.byTask[req.TaskID]; ok {
			key = existingKey
		} else {
			key = fmt.Sprintf("task-bound:%s:%s:%s:%s", req.WorkflowID, req.WorkflowRevision, req.Role, req.TaskID)
		}
	}
	if existing, ok := r.byKey[key]; ok && !existing.Fresh {
		existing.Reused = true
		existing.TaskID = req.TaskID
		existing.TurnCount++
		if req.Budget != nil {
			existing.Budget = req.Budget
		}
		existing.UpdatedAt = now
		r.byKey[key] = existing
		r.byTask[req.TaskID] = key
		return existing, nil
	}
	threadID := makeThreadUUID(fmt.Sprintf("%s:%d:%d", key, r.counter, now.UnixNano()))
	rec := SessionRecord{
		Key:              key,
		Scope:            scope,
		TaskID:           req.TaskID,
		WorkflowID:       req.WorkflowID,
		WorkflowRevision: req.WorkflowRevision,
		Role:             req.Role,
		Provider:         providerName,
		Model:            modelName,
		Mode:             req.Mode,
		ThreadID:         threadID,
		Fresh:            false,
		Reused:           false,
		TurnCount:        1,
		Budget:           req.Budget,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	r.byKey[key] = rec
	r.byTask[req.TaskID] = key
	return rec, nil
}

// BindThreadID records or updates the provider ThreadID for taskID's session.
func (r *SessionRegistry) BindThreadID(taskID, workflowID, workflowRevision, role, providerName, modelName, mode, threadID string, fresh bool, now time.Time) SessionRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	if key, ok := r.byTask[taskID]; ok {
		rec := r.byKey[key]
		if threadID != "" {
			rec.ThreadID = threadID
		}
		rec.UpdatedAt = now
		r.byKey[key] = rec
		return rec
	}
	r.counter++
	key := fmt.Sprintf("task-bound:%s:%s:%s:%s", workflowID, workflowRevision, role, taskID)
	if threadID == "" {
		threadID = makeThreadUUID(fmt.Sprintf("%s:%d", key, r.counter))
	}
	rec := SessionRecord{
		Key:              key,
		Scope:            ScopeTask,
		TaskID:           taskID,
		WorkflowID:       workflowID,
		WorkflowRevision: workflowRevision,
		Role:             role,
		Provider:         providerName,
		Model:            modelName,
		Mode:             mode,
		ThreadID:         threadID,
		Fresh:            fresh,
		Reused:           false,
		TurnCount:        1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	r.byKey[key] = rec
	r.byTask[taskID] = key
	return rec
}

// GetTaskSession returns the current SessionRecord bound to taskID, if any.
func (r *SessionRegistry) GetTaskSession(taskID string) (SessionRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.byTask[taskID]
	if !ok {
		return SessionRecord{}, false
	}
	rec, ok := r.byKey[key]
	return rec, ok
}

// TransferTaskSession moves the task-bound provider session key from fromTaskID
// to toTaskID so the follow-up task resumes in that exact provider session
// without mutating the Done predecessor task's historical records (FAE-OPS-034).
func (r *SessionRegistry) TransferTaskSession(fromTaskID, toTaskID string, now time.Time) (SessionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fromTaskID == "" || toTaskID == "" {
		return SessionRecord{}, errors.New("from_task_id and to_task_id required")
	}
	if fromTaskID == toTaskID {
		return SessionRecord{}, errors.New("from_task_id and to_task_id must differ")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	oldKey, ok := r.byTask[fromTaskID]
	if !ok {
		return SessionRecord{}, fmt.Errorf("no task-bound provider session found for task %s", fromTaskID)
	}
	rec, ok := r.byKey[oldKey]
	if !ok {
		return SessionRecord{}, fmt.Errorf("no task-bound provider session found for task %s", fromTaskID)
	}
	if rec.Fresh {
		return SessionRecord{}, fmt.Errorf("follow-up-refused: role %s has fresh: true", rec.Role)
	}
	delete(r.byKey, oldKey)
	delete(r.byTask, fromTaskID)
	newKey := fmt.Sprintf("task-bound:%s:%s:%s:%s", rec.WorkflowID, rec.WorkflowRevision, rec.Role, toTaskID)
	rec.Key = newKey
	rec.Scope = ScopeTask
	rec.TaskID = toTaskID
	rec.Reused = true
	rec.UpdatedAt = now
	r.byKey[newKey] = rec
	r.byTask[toTaskID] = newKey
	return rec, nil
}

// TransferTaskSessionOnDisk moves a task-bound on-disk provider session state file
// in root from fromTaskID's scope directory to toTaskID's scope directory.
func TransferTaskSessionOnDisk(root, workflowID, workflowRevision, role string, binding workflow.Binding, profile workflow.Profile, mode, fromTaskID, toTaskID string) (string, error) {
	if root == "" {
		return "", nil
	}
	fromKey := taskBoundScopeKey(workflowID, workflowRevision, role, binding, profile, mode, fromTaskID)
	toKey := taskBoundScopeKey(workflowID, workflowRevision, role, binding, profile, mode, toTaskID)
	fromStatePath := filepath.Join(root, fromKey, "state.json")
	raw, err := os.ReadFile(fromStatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var st sessionState
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", err
	}
	if st.ThreadID == "" {
		return "", nil
	}
	toDir := filepath.Join(root, toKey)
	if err := privateDirectory(toDir); err != nil {
		return "", err
	}
	newState := sessionState{Scope: toKey, ThreadID: st.ThreadID, Pending: false}
	encoded, err := json.Marshal(newState)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(toDir, "state.json"), encoded, 0600); err != nil {
		return "", err
	}
	_ = os.Remove(fromStatePath)
	return st.ThreadID, nil
}
