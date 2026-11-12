package symphony

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// WatcherOption configures a ConfigManager / Watcher.
type WatcherOption func(*ConfigManager)

// WithEnv sets the environment lookup function used during config resolution.
func WithEnv(env func(string) string) WatcherOption {
	return func(cm *ConfigManager) {
		cm.Env = env
	}
}

// WithSupportedTrackers sets the allowed tracker kinds for dispatch validation.
func WithSupportedTrackers(kinds ...string) WatcherOption {
	return func(cm *ConfigManager) {
		cm.SupportedTrackers = append([]string(nil), kinds...)
	}
}

// WithOnReload registers a callback invoked after a successful reload.
func WithOnReload(fn func(def *WorkflowDefinition, cfg *ServiceConfig)) WatcherOption {
	return func(cm *ConfigManager) {
		cm.OnReload = fn
	}
}

// WithOnError registers a callback invoked when a reload or validation fails.
func WithOnError(fn func(err error)) WatcherOption {
	return func(cm *ConfigManager) {
		cm.OnError = fn
	}
}

// ConfigManager watches and dynamically reloads WORKFLOW.md while preserving
// the last known good configuration on invalid reloads (Symphony §6.2).
type ConfigManager struct {
	Path              string
	Env               func(string) string
	SupportedTrackers []string
	OnReload          func(def *WorkflowDefinition, cfg *ServiceConfig)
	OnError           func(err error)

	mu          sync.RWMutex
	def         *WorkflowDefinition
	cfg         *ServiceConfig
	lastModTime time.Time
	lastDigest  [32]byte
	hasLoaded   bool
	lastErr     error
}

// Watcher is an alias for ConfigManager.
type Watcher = ConfigManager

// NewConfigManager creates a ConfigManager for explicitPath and performs the initial load.
func NewConfigManager(explicitPath string, opts ...WatcherOption) (*ConfigManager, error) {
	cm := &ConfigManager{
		Path: ResolveWorkflowPath(explicitPath),
		Env:  os.Getenv,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cm)
		}
	}
	if err := cm.ForceReload(); err != nil {
		return nil, err
	}
	return cm, nil
}

// NewWatcher creates a Watcher for explicitPath and performs the initial load.
func NewWatcher(explicitPath string, opts ...WatcherOption) (*Watcher, error) {
	return NewConfigManager(explicitPath, opts...)
}

// Workflow returns the current effective WorkflowDefinition.
func (cm *ConfigManager) Workflow() *WorkflowDefinition {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.def
}

// Config returns the current effective ServiceConfig.
func (cm *ConfigManager) Config() *ServiceConfig {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.cfg
}

// Snapshot atomically returns the current effective WorkflowDefinition and ServiceConfig.
func (cm *ConfigManager) Snapshot() (*WorkflowDefinition, *ServiceConfig) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.def, cm.cfg
}

// LastError returns the error from the most recent reload attempt, or nil if the last reload succeeded.
func (cm *ConfigManager) LastError() error {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.lastErr
}

// ModTime returns the modification time of the last successfully loaded WORKFLOW.md.
func (cm *ConfigManager) ModTime() time.Time {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.lastModTime
}

// Digest returns the hex-encoded SHA-256 content digest of the last successfully loaded WORKFLOW.md.
func (cm *ConfigManager) Digest() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if !cm.hasLoaded {
		return ""
	}
	return hex.EncodeToString(cm.lastDigest[:])
}

// ReloadIfChanged checks the file mtime and SHA-256 digest of WORKFLOW.md.
// If the file content has changed (or has not yet been loaded), it parses and
// validates the new configuration. On failure, the last known good configuration
// is preserved and LastError / OnError are updated.
func (cm *ConfigManager) ReloadIfChanged() (bool, error) {
	path := ResolveWorkflowPath(cm.Path)
	info, err := os.Stat(path)
	if err != nil {
		wrapped := fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
		cm.recordError(wrapped)
		return false, wrapped
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		wrapped := fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
		cm.recordError(wrapped)
		return false, wrapped
	}

	digest := sha256.Sum256(raw)

	cm.mu.RLock()
	hasLoaded := cm.hasLoaded
	sameDigest := hasLoaded && cm.lastDigest == digest
	cm.mu.RUnlock()

	if sameDigest {
		cm.mu.Lock()
		cm.lastModTime = info.ModTime()
		cm.mu.Unlock()
		return false, nil
	}

	if err := cm.applyBytes(path, raw, info.ModTime(), digest); err != nil {
		return false, err
	}
	return true, nil
}

// ForceReload unconditionally reads, parses, and validates WORKFLOW.md.
// On failure, the last known good configuration is preserved.
func (cm *ConfigManager) ForceReload() error {
	path := ResolveWorkflowPath(cm.Path)
	info, err := os.Stat(path)
	if err != nil {
		wrapped := fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
		cm.recordError(wrapped)
		return wrapped
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		wrapped := fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
		cm.recordError(wrapped)
		return wrapped
	}

	digest := sha256.Sum256(raw)
	return cm.applyBytes(path, raw, info.ModTime(), digest)
}

func (cm *ConfigManager) applyBytes(path string, raw []byte, modTime time.Time, digest [32]byte) error {
	def, err := ParseWorkflowBytes(raw, path)
	if err != nil {
		cm.recordError(err)
		return err
	}

	if err := ValidatePromptTemplate(def.PromptTemplate); err != nil {
		cm.recordError(err)
		return err
	}

	cfg, err := ResolveServiceConfig(def, cm.Env)
	if err != nil {
		cm.recordError(err)
		return err
	}

	if err := cm.validateCandidateConfig(cfg); err != nil {
		cm.recordError(err)
		return err
	}

	cm.mu.Lock()
	cm.def = def
	cm.cfg = cfg
	cm.lastModTime = modTime
	cm.lastDigest = digest
	cm.hasLoaded = true
	cm.lastErr = nil
	onReload := cm.OnReload
	cm.mu.Unlock()

	if onReload != nil {
		onReload(def, cfg)
	}
	return nil
}

func (cm *ConfigManager) validateCandidateConfig(cfg *ServiceConfig) error {
	if cfg == nil {
		return errors.New("service config is nil")
	}
	if strings.TrimSpace(cfg.Codex.Command) == "" {
		return errors.New("codex.command must not be empty")
	}
	if cfg.Hooks.TimeoutMS <= 0 {
		return fmt.Errorf("hooks.timeout_ms must be > 0, got %d", cfg.Hooks.TimeoutMS)
	}
	if cfg.Agent.MaxTurns <= 0 {
		return fmt.Errorf("agent.max_turns must be > 0, got %d", cfg.Agent.MaxTurns)
	}
	if cfg.Agent.MaxConcurrentAgents <= 0 {
		return fmt.Errorf("agent.max_concurrent_agents must be > 0, got %d", cfg.Agent.MaxConcurrentAgents)
	}
	if cfg.Worker.MaxConcurrentAgentsPerHost < 0 || (cfg.Worker.hasExplicitPerHostLimit && cfg.Worker.MaxConcurrentAgentsPerHost <= 0) {
		return fmt.Errorf("worker.max_concurrent_agents_per_host must be > 0, got %d", cfg.Worker.MaxConcurrentAgentsPerHost)
	}

	cm.mu.RLock()
	hadTrackerKind := cm.cfg != nil && strings.TrimSpace(cm.cfg.Tracker.Kind) != ""
	supported := cm.SupportedTrackers
	cm.mu.RUnlock()

	if len(supported) > 0 || hadTrackerKind || strings.TrimSpace(cfg.Tracker.Kind) != "" {
		if hadTrackerKind && strings.TrimSpace(cfg.Tracker.Kind) == "" {
			return errors.New("tracker.kind is required")
		}
		if len(supported) > 0 || strings.EqualFold(strings.TrimSpace(cfg.Tracker.Kind), "linear") {
			return ValidateDispatchConfig(cfg, supported...)
		}
	}
	return nil
}

func (cm *ConfigManager) recordError(err error) {
	cm.mu.Lock()
	cm.lastErr = err
	onErr := cm.OnError
	cm.mu.Unlock()

	if onErr != nil {
		onErr(err)
	}
}

// Run polls for WORKFLOW.md changes at the given interval until ctx is canceled.
func (cm *ConfigManager) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 1 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, _ = cm.ReloadIfChanged()
		}
	}
}
