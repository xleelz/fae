package symphony

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default configuration constants (Symphony §5.3 & §6.4).
const (
	DefaultPollingIntervalMS   = 30000
	DefaultWorkspaceSubdir     = "symphony_workspaces"
	DefaultHooksTimeoutMS      = 60000
	DefaultMaxConcurrentAgents = 10
	DefaultMaxTurns            = 20
	DefaultMaxRetryBackoffMS   = 300000
	DefaultCodexCommand        = "codex app-server"
	DefaultCodexTurnTimeoutMS  = 3600000
	DefaultCodexReadTimeoutMS  = 5000
	DefaultCodexStallTimeoutMS = 300000
)

// ResolveServiceConfig resolves a typed ServiceConfig from a parsed WorkflowDefinition,
// applying built-in defaults, $VAR indirection, and path normalization (Symphony §§5.3, 6.1, 6.4).
func ResolveServiceConfig(def *WorkflowDefinition, env func(string) string) (*ServiceConfig, error) {
	if def == nil {
		return nil, errors.New("workflow definition is nil")
	}
	if env == nil {
		env = os.Getenv
	}

	rawCfg := def.Config
	if rawCfg == nil {
		rawCfg = map[string]any{}
	}

	defaultRoot := filepath.Join(os.TempDir(), DefaultWorkspaceSubdir)
	if absDefault, err := filepath.Abs(defaultRoot); err == nil {
		defaultRoot = absDefault
	}

	cfg := &ServiceConfig{
		Fae: FaeConfig{Mode: "Local"},
		Tracker: TrackerConfig{
			Provider:       map[string]any{},
			RequiredLabels: []string{},
			ActiveStates:   []string{},
			TerminalStates: []string{},
		},
		Polling: PollingConfig{
			IntervalMS: DefaultPollingIntervalMS,
		},
		Workspace: WorkspaceConfig{
			Root: defaultRoot,
		},
		Hooks: HooksConfig{
			TimeoutMS: DefaultHooksTimeoutMS,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents:        DefaultMaxConcurrentAgents,
			MaxTurns:                   DefaultMaxTurns,
			MaxRetryBackoffMS:          DefaultMaxRetryBackoffMS,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		Codex: CodexConfig{
			Command:        DefaultCodexCommand,
			TurnTimeoutMS:  DefaultCodexTurnTimeoutMS,
			ReadTimeoutMS:  DefaultCodexReadTimeoutMS,
			StallTimeoutMS: DefaultCodexStallTimeoutMS,
		},
	}

	// 1. Tracker section
	var hasExplicitActiveStates, hasExplicitTerminalStates bool
	var rawAPIKey any
	var hasExplicitAPIKey bool
	if trackerMap, ok := asStringMap(rawCfg["tracker"]); ok {
		if v, exists := trackerMap["kind"]; exists && v != nil {
			cfg.Tracker.Kind = strings.TrimSpace(expandEnvString(fmt.Sprint(v), env))
		}
		if provMap, ok := asStringMap(trackerMap["provider"]); ok {
			if ak, exists := provMap["api_key"]; exists && ak != nil {
				rawAPIKey = ak
				hasExplicitAPIKey = true
			}
			cfg.Tracker.Provider = expandProviderMap(provMap, env)
		}
		// Also copy any additional provider-specific keys directly under tracker if not standard keys
		for k, v := range trackerMap {
			switch k {
			case "kind", "provider", "required_labels", "active_states", "terminal_states":
				continue
			default:
				if k == "api_key" && !hasExplicitAPIKey && v != nil {
					rawAPIKey = v
					hasExplicitAPIKey = true
				}
				if _, already := cfg.Tracker.Provider[k]; !already {
					cfg.Tracker.Provider[k] = expandProviderValue(v, env)
				}
			}
		}
		if v, exists := trackerMap["required_labels"]; exists && v != nil {
			if list, err := toStringSlice(v); err == nil {
				cfg.Tracker.RequiredLabels = list
			} else {
				return nil, fmt.Errorf("tracker.required_labels: %w", err)
			}
		}
		if v, exists := trackerMap["active_states"]; exists && v != nil {
			hasExplicitActiveStates = true
			if list, err := toStringSlice(v); err == nil {
				cfg.Tracker.ActiveStates = list
			} else {
				return nil, fmt.Errorf("tracker.active_states: %w", err)
			}
		}
		if v, exists := trackerMap["terminal_states"]; exists && v != nil {
			hasExplicitTerminalStates = true
			if list, err := toStringSlice(v); err == nil {
				cfg.Tracker.TerminalStates = list
			} else {
				return nil, fmt.Errorf("tracker.terminal_states: %w", err)
			}
		}
	}

	if hasExplicitAPIKey {
		cfg.Tracker.Provider["_api_key_explicit"] = true
		if rawStr, ok := rawAPIKey.(string); ok {
			if envVar := extractSingleEnvVarName(rawStr); envVar != "" {
				cfg.Tracker.Provider["_api_key_env"] = envVar
			}
		}
	}

	if strings.EqualFold(cfg.Tracker.Kind, "linear") {
		if !hasExplicitActiveStates && len(cfg.Tracker.ActiveStates) == 0 {
			cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
		}
		if !hasExplicitTerminalStates && len(cfg.Tracker.TerminalStates) == 0 {
			cfg.Tracker.TerminalStates = []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
		}
		if !hasExplicitAPIKey {
			cfg.Tracker.Provider["api_key"] = strings.TrimSpace(env("LINEAR_API_KEY"))
			cfg.Tracker.Provider["_api_key_env"] = "LINEAR_API_KEY"
		}
	}

	// 2. Polling section
	if pollingMap, ok := asStringMap(rawCfg["polling"]); ok {
		if v, exists := pollingMap["interval_ms"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("polling.interval_ms must be an integer, got %v", v)
			}
			cfg.Polling.IntervalMS = n
		}
	}

	// 3. Workspace section
	if wsMap, ok := asStringMap(rawCfg["workspace"]); ok {
		if v, exists := wsMap["root"]; exists && v != nil {
			rawRoot := strings.TrimSpace(fmt.Sprint(v))
			if rawRoot != "" {
				resolvedRoot, err := resolveWorkspaceRoot(rawRoot, def.Path, env)
				if err != nil {
					return nil, fmt.Errorf("workspace.root: %w", err)
				}
				cfg.Workspace.Root = resolvedRoot
			}
		}
	}

	// 4. Hooks section
	if hooksMap, ok := asStringMap(rawCfg["hooks"]); ok {
		if s, ok := toOptionalString(hooksMap, "after_create"); ok {
			cfg.Hooks.AfterCreate = &s
		}
		if s, ok := toOptionalString(hooksMap, "before_run"); ok {
			cfg.Hooks.BeforeRun = &s
		}
		if s, ok := toOptionalString(hooksMap, "after_run"); ok {
			cfg.Hooks.AfterRun = &s
		}
		if s, ok := toOptionalString(hooksMap, "before_remove"); ok {
			cfg.Hooks.BeforeRemove = &s
		}
		if v, exists := hooksMap["timeout_ms"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("hooks.timeout_ms must be an integer, got %v", v)
			}
			cfg.Hooks.TimeoutMS = n
		}
	}

	// 5. Agent section
	if agentMap, ok := asStringMap(rawCfg["agent"]); ok {
		if v, exists := agentMap["max_concurrent_agents"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("agent.max_concurrent_agents must be an integer, got %v", v)
			}
			cfg.Agent.MaxConcurrentAgents = n
		}
		if v, exists := agentMap["max_turns"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("agent.max_turns must be an integer, got %v", v)
			}
			cfg.Agent.MaxTurns = n
		}
		if v, exists := agentMap["max_retry_backoff_ms"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("agent.max_retry_backoff_ms must be an integer, got %v", v)
			}
			cfg.Agent.MaxRetryBackoffMS = n
		}
		if byStateMap, ok := asStringMap(agentMap["max_concurrent_agents_by_state"]); ok {
			for stateName, rawLimit := range byStateMap {
				norm := NormalizeState(stateName)
				if norm == "" {
					continue
				}
				limit, ok := toInt(rawLimit)
				if !ok || limit <= 0 {
					// Ignore non-positive or non-numeric entries per §5.3.5
					continue
				}
				cfg.Agent.MaxConcurrentAgentsByState[norm] = limit
			}
		}
	}

	// 6. Codex section
	if codexMap, ok := asStringMap(rawCfg["codex"]); ok {
		if v, exists := codexMap["command"]; exists && v != nil {
			// Preserve as shell command string; do not expand ~ or rewrite URIs (§5.3.6 / §6.1)
			cfg.Codex.Command = fmt.Sprint(v)
		}
		if v, exists := codexMap["approval_policy"]; exists {
			cfg.Codex.ApprovalPolicy = v
		}
		if v, exists := codexMap["thread_sandbox"]; exists {
			cfg.Codex.ThreadSandbox = v
		}
		if v, exists := codexMap["turn_sandbox_policy"]; exists {
			cfg.Codex.TurnSandboxPolicy = v
		}
		if v, exists := codexMap["turn_timeout_ms"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("codex.turn_timeout_ms must be an integer, got %v", v)
			}
			cfg.Codex.TurnTimeoutMS = n
		}
		if v, exists := codexMap["read_timeout_ms"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("codex.read_timeout_ms must be an integer, got %v", v)
			}
			cfg.Codex.ReadTimeoutMS = n
		}
		if v, exists := codexMap["stall_timeout_ms"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("codex.stall_timeout_ms must be an integer, got %v", v)
			}
			cfg.Codex.StallTimeoutMS = n
		}
	}

	// 7. Server section
	if serverMap, ok := asStringMap(rawCfg["server"]); ok {
		if v, exists := serverMap["port"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("server.port must be an integer, got %v", v)
			}
			cfg.Server.Port = &n
		}
		if v, exists := serverMap["host"]; exists && v != nil {
			cfg.Server.Host = strings.TrimSpace(fmt.Sprint(v))
		}
	}

	// 8. Worker section (Appendix A SSH Worker Extension)
	if workerMap, ok := asStringMap(rawCfg["worker"]); ok {
		if v, exists := workerMap["ssh_hosts"]; exists && v != nil {
			list, err := toStringSlice(v)
			if err != nil {
				return nil, fmt.Errorf("worker.ssh_hosts: %w", err)
			}
			hosts := make([]string, 0, len(list))
			for _, h := range list {
				expanded := strings.TrimSpace(expandEnvString(h, env))
				if expanded != "" {
					hosts = append(hosts, expanded)
				}
			}
			cfg.Worker.SSHHosts = hosts
		}
		if v, exists := workerMap["max_concurrent_agents_per_host"]; exists && v != nil {
			n, ok := toInt(v)
			if !ok {
				return nil, fmt.Errorf("worker.max_concurrent_agents_per_host must be an integer, got %v", v)
			}
			cfg.Worker.MaxConcurrentAgentsPerHost = n
			cfg.Worker.hasExplicitPerHostLimit = true
		}
	}

	// Embedded FAE engine selectors are separate from standalone Codex settings.
	if raw, exists := rawCfg["fae"]; exists && raw != nil {
		section, ok := asStringMap(raw)
		if !ok {
			return nil, errors.New("fae must be a configuration map")
		}
		for key, value := range section {
			if value == nil {
				continue
			}
			switch key {
			case "workflow_id", "target_config", "authorizing_message", "mode":
				text, ok := value.(string)
				if !ok {
					return nil, fmt.Errorf("fae.%s must be a string", key)
				}
				text = strings.TrimSpace(expandEnvString(text, env))
				switch key {
				case "workflow_id":
					cfg.Fae.WorkflowID = text
				case "target_config":
					cfg.Fae.TargetConfig = text
				case "authorizing_message":
					cfg.Fae.AuthorizingMessage = text
				case "mode":
					if text != "" && !strings.EqualFold(text, "Local") {
						return nil, errors.New("fae.mode must be Local; external deployment is not authorized by the Linear bridge")
					}
					cfg.Fae.Mode = "Local"
				}
			case "workflow_revision", "target_revision":
				revision, ok := faeRevision(value, env)
				if !ok || revision < 0 {
					return nil, fmt.Errorf("fae.%s must be a nonnegative integer", key)
				}
				if key == "workflow_revision" {
					cfg.Fae.WorkflowRevision = revision
				} else {
					cfg.Fae.TargetRevision = revision
				}
			case "sync_state", "post_comments":
				enabled, ok := value.(bool)
				if !ok {
					return nil, fmt.Errorf("fae.%s must be a boolean", key)
				}
				if key == "sync_state" {
					cfg.Fae.SyncState = enabled
				} else {
					cfg.Fae.PostComments = enabled
				}
			default:
				return nil, fmt.Errorf("unknown fae configuration field %q", key)
			}
		}
	}
	return cfg, nil
}

func extractSingleEnvVarName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "${") && strings.HasSuffix(trimmed, "}") {
		name := strings.TrimSpace(trimmed[2 : len(trimmed)-1])
		if name != "" && !strings.ContainsAny(name, " $/") {
			return name
		}
		return ""
	}
	if strings.HasPrefix(trimmed, "$") {
		name := strings.TrimSpace(trimmed[1:])
		if name != "" && !strings.ContainsAny(name, " ${}/") {
			return name
		}
	}
	return ""
}

// ValidateDispatchConfig performs preflight validation on a resolved ServiceConfig (Symphony §6.3).
func ValidateDispatchConfig(cfg *ServiceConfig, supportedTrackers ...string) error {
	if cfg == nil {
		return errors.New("service config is nil")
	}
	kind := strings.TrimSpace(cfg.Tracker.Kind)
	if kind == "" {
		return errors.New("tracker.kind is required")
	}
	if len(supportedTrackers) > 0 {
		matched := false
		for _, supported := range supportedTrackers {
			if strings.EqualFold(strings.TrimSpace(supported), kind) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("unsupported_tracker_kind: unsupported tracker.kind %q", cfg.Tracker.Kind)
		}
	}
	if strings.EqualFold(kind, "linear") {
		apiKey := ""
		explicit := false
		explicitEnv := ""
		if cfg.Tracker.Provider != nil {
			if v, ok := cfg.Tracker.Provider["api_key"].(string); ok {
				apiKey = strings.TrimSpace(v)
			}
			explicit, _ = cfg.Tracker.Provider["_api_key_explicit"].(bool)
			explicitEnv, _ = cfg.Tracker.Provider["_api_key_env"].(string)
		}
		if strings.HasPrefix(apiKey, "$") {
			if envName := extractSingleEnvVarName(apiKey); envName != "" {
				apiKey = strings.TrimSpace(os.Getenv(envName))
			} else {
				apiKey = ""
			}
		} else if apiKey == "" {
			if explicit {
				if explicitEnv != "" {
					apiKey = strings.TrimSpace(os.Getenv(explicitEnv))
				}
			} else {
				envName := explicitEnv
				if envName == "" {
					envName = "LINEAR_API_KEY"
				}
				apiKey = strings.TrimSpace(os.Getenv(envName))
			}
		}
		if apiKey == "" {
			return errors.New("missing_tracker_secret: missing Linear api_key")
		}

		hasScope := false
		if cfg.Tracker.Provider != nil {
			for _, k := range []string{"project_slug", "team_id", "team_key", "project_id"} {
				if v, ok := cfg.Tracker.Provider[k].(string); ok && strings.TrimSpace(v) != "" {
					hasScope = true
					break
				}
			}
		}
		if !hasScope {
			return errors.New("invalid_tracker_config: missing Linear project_slug (or team_id/team_key/project_id)")
		}
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
	return nil
}

func expandEnvString(s string, env func(string) string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return os.Expand(s, env)
}

func expandProviderMap(in map[string]any, env func(string) string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = expandProviderValue(v, env)
	}
	return out
}

func expandProviderValue(v any, env func(string) string) any {
	switch tv := v.(type) {
	case string:
		return expandEnvString(tv, env)
	case map[string]any:
		return expandProviderMap(tv, env)
	case []any:
		out := make([]any, len(tv))
		for i, elem := range tv {
			out[i] = expandProviderValue(elem, env)
		}
		return out
	case []string:
		out := make([]string, len(tv))
		for i, elem := range tv {
			out[i] = expandEnvString(elem, env)
		}
		return out
	default:
		return v
	}
}

func resolveWorkspaceRoot(rawRoot string, workflowPath string, env func(string) string) (string, error) {
	expanded := strings.TrimSpace(expandEnvString(rawRoot, env))
	if expanded == "" {
		defaultRoot := filepath.Join(os.TempDir(), DefaultWorkspaceSubdir)
		return filepath.Abs(defaultRoot)
	}

	if expanded == "~" || strings.HasPrefix(expanded, "~/") || strings.HasPrefix(expanded, `~\`) {
		home := env("HOME")
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h
			}
		}
		if home != "" {
			if expanded == "~" {
				expanded = home
			} else {
				expanded = filepath.Join(home, expanded[2:])
			}
		}
	}

	if !filepath.IsAbs(expanded) {
		var baseDir string
		if strings.TrimSpace(workflowPath) != "" {
			baseDir = filepath.Dir(workflowPath)
		} else {
			if cwd, err := os.Getwd(); err == nil {
				baseDir = cwd
			}
		}
		if baseDir != "" {
			expanded = filepath.Join(baseDir, expanded)
		}
	}

	abs, err := filepath.Abs(filepath.Clean(expanded))
	if err != nil {
		return "", err
	}
	return abs, nil
}

func asStringMap(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func toOptionalString(m map[string]any, key string) (string, bool) {
	v, exists := m[key]
	if !exists || v == nil {
		return "", false
	}
	return fmt.Sprint(v), true
}

func toStringSlice(v any) ([]string, error) {
	switch tv := v.(type) {
	case []string:
		out := make([]string, len(tv))
		copy(out, tv)
		return out, nil
	case []any:
		out := make([]string, 0, len(tv))
		for _, item := range tv {
			if item == nil {
				continue
			}
			out = append(out, fmt.Sprint(item))
		}
		return out, nil
	case string:
		trimmed := strings.TrimSpace(tv)
		if trimmed == "" {
			return []string{}, nil
		}
		parts := strings.Split(trimmed, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected list of strings, got %T", v)
	}
}

func toInt(v any) (int, bool) {
	switch tv := v.(type) {
	case int:
		return tv, true
	case int64:
		return int(tv), true
	case int32:
		return int(tv), true
	case float64:
		if math.Trunc(tv) == tv {
			return int(tv), true
		}
		return 0, false
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(tv))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func faeRevision(value any, env func(string) string) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || math.Abs(v) > 9007199254740991 {
			return 0, false
		}
		return int64(v), true
	case string:
		revision, err := strconv.ParseInt(strings.TrimSpace(expandEnvString(v, env)), 10, 64)
		return revision, err == nil
	}
	return 0, false
}

// ValidateFaeConfig is embedded-only admission preflight. ResolveServiceConfig
// deliberately leaves these selectors optional for standalone Symphony.
func ValidateFaeConfig(config FaeConfig) error {
	if strings.TrimSpace(config.WorkflowID) == "" {
		return errors.New("fae.workflow_id is required for embedded FAE execution")
	}
	if strings.TrimSpace(config.TargetConfig) == "" {
		return errors.New("fae.target_config is required for embedded FAE execution")
	}
	if strings.TrimSpace(config.AuthorizingMessage) == "" {
		return errors.New("fae.authorizing_message is required for embedded FAE execution")
	}
	if config.WorkflowRevision < 0 || config.TargetRevision < 0 {
		return errors.New("fae workflow and target revisions must be nonnegative")
	}
	if config.Mode != "" && !strings.EqualFold(strings.TrimSpace(config.Mode), "Local") {
		return errors.New("fae.mode must be Local; the bridge does not authorize deployment")
	}
	return nil
}
