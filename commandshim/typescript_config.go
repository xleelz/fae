package commandshim

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// decodeTSConfig handles JSONC comments/trailing commas without interpreting
// executable configuration (JavaScript configs and compiler plugins are refused).
func decodeTSConfig(raw []byte) (map[string]any, error) {
	if len(raw) > 1<<20 {
		return nil, refuse("TypeScript config exceeds 1 MiB")
	}
	clean := append([]byte{}, raw...)
	quoted, escaped := false, false
	for i := 0; i < len(clean); i++ {
		ch := clean[i]
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
			continue
		}
		if ch == '"' {
			quoted = true
			continue
		}
		if ch == '/' && i+1 < len(clean) {
			if clean[i+1] == '/' {
				clean[i], clean[i+1] = ' ', ' '
				i += 2
				for i < len(clean) && clean[i] != '\n' {
					clean[i] = ' '
					i++
				}
				i--
			} else if clean[i+1] == '*' {
				clean[i], clean[i+1] = ' ', ' '
				i += 2
				closed := false
				for i+1 < len(clean) {
					if clean[i] == '*' && clean[i+1] == '/' {
						clean[i], clean[i+1] = ' ', ' '
						i++
						closed = true
						break
					}
					if clean[i] != '\n' {
						clean[i] = ' '
					}
					i++
				}
				if !closed {
					return nil, refuse("unterminated TypeScript config comment")
				}
			}
		}
	}
	quoted, escaped = false, false
	for i, ch := range clean {
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
			continue
		}
		if ch == '"' {
			quoted = true
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(clean) && (clean[j] == ' ' || clean[j] == '\t' || clean[j] == '\n' || clean[j] == '\r') {
				j++
			}
			if j < len(clean) && (clean[j] == '}' || clean[j] == ']') {
				clean[i] = ' '
			}
		}
	}
	var document map[string]any
	if err := json.Unmarshal(clean, &document); err != nil {
		return nil, fmt.Errorf("TypeScript config: %w", err)
	}
	return document, nil
}
func (p Policy) validateTSConfig(config string, active map[string]bool, depth int) (map[string]any, error) {
	calls := 0
	for calls < 128 && active["\x00call:"+strconv.Itoa(calls)] {
		calls++
	}
	if calls == 128 {
		return nil, refuse("TypeScript config graph exceeds scoped bound")
	}
	active["\x00call:"+strconv.Itoa(calls)] = true
	if depth > 16 || active[config] {
		return nil, refuse("TypeScript config inheritance/reference cycle or depth limit")
	}
	active[config] = true
	defer delete(active, config)
	if !inside(p.Worktree, config) {
		return nil, refuse("TypeScript config escapes own worktree")
	}
	resolved, err := p.scoped(config)
	if err != nil || !inside(p.Worktree, resolved) {
		return nil, refuse("TypeScript config resolves outside own worktree")
	}
	config = resolved
	file, err := os.Open(config)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	file.Close()
	if err != nil {
		return nil, err
	}
	document, err := decodeTSConfig(raw)
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(config)
	options := map[string]any{}
	extends := []string{}
	switch value := document["extends"].(type) {
	case nil:
	case string:
		extends = append(extends, value)
	case []any:
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, refuse("invalid TypeScript extends list")
			}
			extends = append(extends, text)
		}
	default:
		return nil, refuse("invalid TypeScript extends")
	}
	for _, base := range extends {
		path := base
		if !filepath.IsAbs(path) {
			if strings.HasPrefix(path, ".") {
				path = filepath.Join(directory, path)
			} else {
				path = filepath.Join(p.Worktree, "node_modules", path)
			}
		}
		if filepath.Ext(path) == "" {
			if _, e := os.Stat(path + ".json"); e == nil {
				path += ".json"
			} else {
				path = filepath.Join(path, "tsconfig.json")
			}
		}
		inherited, e := p.validateTSConfig(path, active, depth+1)
		if e != nil {
			return nil, e
		}
		for key, value := range inherited {
			options[key] = value
		}
	}
	if own, ok := document["compilerOptions"].(map[string]any); ok {
		for key, value := range own {
			options[key] = value
		}
		if own["plugins"] != nil {
			return nil, refuse("TypeScript compiler plugins are unsupported")
		}
		for _, key := range []string{"outDir", "outFile", "declarationDir", "tsBuildInfoFile", "rootDir", "baseUrl", "mapRoot", "sourceRoot"} {
			if value, ok := own[key].(string); ok && value != "" {
				path := value
				if !filepath.IsAbs(path) {
					path = filepath.Join(directory, path)
				}
				physical, e := p.scoped(path)
				if e != nil || !inside(p.Worktree, physical) {
					return nil, refuse("TypeScript " + key + " escapes own worktree")
				}
				options[key] = physical
			}
		}
		for _, key := range []string{"typeRoots", "rootDirs"} {
			if value, ok := own[key].([]any); ok {
				for _, entry := range value {
					path, ok := entry.(string)
					if !ok {
						return nil, refuse("invalid TypeScript " + key)
					}
					if _, e := p.tsInputPath(directory, path); e != nil {
						return nil, e
					}
				}
			}
		}
		if paths, ok := own["paths"].(map[string]any); ok {
			base := directory
			if value, ok := options["baseUrl"].(string); ok {
				base = value
			}
			for _, targets := range paths {
				list, ok := targets.([]any)
				if !ok {
					return nil, refuse("invalid TypeScript path aliases")
				}
				for _, target := range list {
					path, ok := target.(string)
					if !ok {
						return nil, refuse("invalid TypeScript path alias target")
					}
					if _, e := p.tsInputPath(base, path); e != nil {
						return nil, e
					}
				}
			}
		}
	}
	for _, key := range []string{"files", "include", "exclude"} {
		if list, ok := document[key].([]any); ok {
			for _, entry := range list {
				path, ok := entry.(string)
				if !ok {
					return nil, refuse("invalid TypeScript " + key)
				}
				if _, e := p.tsInputPath(directory, path); e != nil {
					return nil, e
				}
			}
		}
	}
	if references, ok := document["references"].([]any); ok {
		for _, entry := range references {
			reference, ok := entry.(map[string]any)
			if !ok {
				return nil, refuse("invalid TypeScript project reference")
			}
			path, ok := reference["path"].(string)
			if !ok {
				return nil, refuse("invalid TypeScript project reference path")
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(directory, path)
			}
			if info, e := os.Stat(path); e == nil && info.IsDir() {
				path = filepath.Join(path, "tsconfig.json")
			}
			if _, e := p.validateTSConfig(path, active, depth+1); e != nil {
				return nil, e
			}
		}
	}
	if document["watch"] == true || options["watch"] == true || options["generateTrace"] != nil || options["generateCpuProfile"] != nil {
		return nil, refuse("TypeScript watch/diagnostic file output is unsupported")
	}
	// Explicit and implicit source traversal may not follow a symlink out of the
	// project. Dependency packages remain the target's installed tool inputs.
	count := 0
	err = filepath.WalkDir(directory, func(full string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == ".git") {
			return filepath.SkipDir
		}
		count++
		if count > 100000 {
			return refuse("TypeScript project listing exceeds scoped bound")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, e := filepath.EvalSymlinks(full)
			if e != nil || !inside(p.Worktree, resolved) {
				return refuse("TypeScript project contains an escaping symlink")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return options, nil
}
func (p Policy) tsInputPath(directory, name string) (string, error) {
	if at := strings.IndexAny(name, "*?["); at >= 0 {
		name = name[:at]
		if !strings.HasSuffix(name, string(filepath.Separator)) {
			name = filepath.Dir(name)
		}
	}
	if name == "" {
		name = "."
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(directory, name)
	}
	physical, err := p.scoped(name)
	if err != nil || !inside(p.Worktree, physical) {
		return "", refuse("TypeScript input/config path escapes own worktree")
	}
	return physical, nil
}
