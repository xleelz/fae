package commandshim

import (
	"os"
	"path/filepath"
	"regexp/syntax"
	"strconv"
	"strings"
)

// TypeScriptCommandKind identifies the deliberately bounded compiler/test
// surface. Generic Node programs and package-manager scripts are not adapters.
func TypeScriptCommandKind(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	switch filepath.Base(argv[0]) {
	case "tsc", "tsc.js":
		return "build", true
	case "node", "nodejs":
		for _, arg := range argv[1:] {
			if arg == "--test" {
				return "test", true
			}
		}
	}
	return "", false
}
func ClampTypeScriptEnv(env []string) ([]string, error) {
	out := []string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if value != "" && (key == "NODE_OPTIONS" || key == "NODE_PATH" || key == "NODE_REPL_EXTERNAL_MODULE" || key == "NODE_V8_COVERAGE" || key == "NODE_REDIRECT_WARNINGS" || key == "NODE_COMPILE_CACHE" || strings.HasPrefix(key, "TS_NODE_") || strings.HasPrefix(key, "TSX_")) {
			return nil, refuse("TypeScript runtime injection environment is unsupported: " + key)
		}
		if key == "UV_THREADPOOL_SIZE" || key == "NODE_DISABLE_COMPILE_CACHE" {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "UV_THREADPOOL_SIZE=1", "NODE_DISABLE_COMPILE_CACHE=1"), nil
}
func TypeScriptEnvironment(argv, env []string, scratch string) ([]string, error) {
	if _, ok := TypeScriptCommandKind(argv); !ok {
		return env, nil
	}
	return ClampTypeScriptEnv(env)
}
func (p Policy) PrepareTypeScript(argv, env []string) ([]string, error) {
	if !p.CodeRole || !p.LeaseHeld {
		return nil, refuse("TypeScript build/test requires a code role and host heavy-operation lease")
	}
	var err error
	p.Worktree, err = filepath.EvalSymlinks(p.Worktree)
	if err != nil {
		return nil, err
	}
	if p.CWD == "" {
		p.CWD = p.Worktree
	}
	p.CWD, err = filepath.EvalSymlinks(p.CWD)
	if err != nil {
		return nil, err
	}
	if !p.inScope(p.CWD) {
		return nil, refuse("TypeScript working directory is outside the own worktree")
	}
	if _, err = ClampTypeScriptEnv(env); err != nil {
		return nil, err
	}
	kind, ok := TypeScriptCommandKind(argv)
	if !ok {
		return nil, refuse("only tsc --project and node --test compiled JavaScript are supported")
	}
	if kind == "test" {
		return p.prepareNodeTest(argv)
	}
	return p.prepareTSC(argv)
}
func exactNodePattern(value string) bool {
	parsed, err := syntax.Parse(value, syntax.Perl)
	if err != nil || parsed.Op != syntax.OpConcat || len(parsed.Sub) != 3 {
		return false
	}
	return parsed.Sub[0].Op == syntax.OpBeginText && parsed.Sub[1].Op == syntax.OpLiteral && len(parsed.Sub[1].Rune) > 0 && parsed.Sub[2].Op == syntax.OpEndText
}
func (p Policy) prepareNodeTest(argv []string) ([]string, error) {
	file, pattern := "", ""
	seen := map[string]bool{}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		key, value, assigned := strings.Cut(arg, "=")
		switch key {
		case "--test":
			if assigned || seen[key] {
				return nil, refuse("duplicate or invalid Node test mode")
			}
			seen[key] = true
		case "--test-name-pattern", "--test-reporter", "--test-concurrency", "--v8-pool-size":
			if seen[key] {
				return nil, refuse("duplicate Node test option: " + key)
			}
			seen[key] = true
			if !assigned {
				if i+1 >= len(argv) {
					return nil, refuse("missing Node test option value")
				}
				i++
				value = argv[i]
			}
			switch key {
			case "--test-name-pattern":
				if !exactNodePattern(value) {
					return nil, refuse("Node test selection must be one exact anchored literal name")
				}
				pattern = value
			case "--test-reporter":
				if value != "tap" {
					return nil, refuse("Node tests require the built-in TAP reporter")
				}
			default:
				if n, e := strconv.Atoi(value); e != nil || n < 1 {
					return nil, refuse("invalid Node worker count")
				}
			}
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, refuse("unsupported Node test option: " + arg)
			}
			if file != "" || strings.ContainsAny(arg, "*?[]{}") {
				return nil, refuse("Node tests require exactly one literal compiled JavaScript file")
			}
			file = arg
		}
	}
	if !seen["--test"] || file == "" {
		return nil, refuse("Node tests require --test and one compiled JavaScript file")
	}
	path, err := p.scoped(file)
	if err != nil {
		return nil, err
	}
	if !inside(p.Worktree, path) {
		return nil, refuse("Node test file must be inside the own worktree")
	}
	switch filepath.Ext(path) {
	case ".js", ".cjs", ".mjs", ".ts", ".mts", ".cts":
	default:
		return nil, refuse("Node tests require JavaScript or native erasable TypeScript (.ts/.mts/.cts); loaders and .tsx are unsupported")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, refuse("Node test target must be an existing regular file")
	}
	result := []string{argv[0], "--v8-pool-size=1", "--test", "--test-concurrency=1", "--test-reporter=tap"}
	if pattern != "" {
		result = append(result, "--test-name-pattern="+pattern)
	}
	return append(result, path), nil
}
func (p Policy) prepareTSC(argv []string) ([]string, error) {
	project := ""
	overrides := map[string]string{}
	seen := map[string]bool{}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		key, value, assigned := strings.Cut(arg, "=")
		if key == "-p" {
			key = "--project"
		}
		if seen[key] {
			return nil, refuse("duplicate TypeScript compiler option: " + key)
		}
		seen[key] = true
		switch key {
		case "--project", "--outDir", "--rootDir", "--declarationDir", "--tsBuildInfoFile":
			if !assigned {
				if i+1 >= len(argv) {
					return nil, refuse("missing TypeScript option value")
				}
				i++
				value = argv[i]
			}
			if value == "" {
				return nil, refuse("empty TypeScript path")
			}
			if key == "--project" {
				project = value
			} else {
				overrides[strings.TrimPrefix(key, "--")] = value
			}
		case "--noEmit", "--pretty", "--declaration", "--emitDeclarationOnly", "--incremental":
			if !assigned && i+1 < len(argv) && (argv[i+1] == "true" || argv[i+1] == "false") {
				i++
				value = argv[i]
				assigned = true
			}
			if !assigned {
				value = "true"
			}
			if value != "true" && value != "false" {
				return nil, refuse("TypeScript boolean option requires true or false")
			}
			overrides[strings.TrimPrefix(key, "--")] = value
		default:
			return nil, refuse("unsupported TypeScript compiler option: " + arg)
		}
	}
	if project == "" {
		return nil, refuse("TypeScript requires one explicit --project configuration")
	}
	config, err := p.scoped(project)
	if err != nil {
		return nil, err
	}
	if info, e := os.Stat(config); e == nil && info.IsDir() {
		config = filepath.Join(config, "tsconfig.json")
	}
	if !inside(p.Worktree, config) {
		return nil, refuse("TypeScript config must be inside the own worktree")
	}
	options, err := p.validateTSConfig(config, map[string]bool{}, 0)
	if err != nil {
		return nil, err
	}
	for key, value := range overrides {
		options[key] = value
	}
	noEmit := options["noEmit"] == true || options["noEmit"] == "true"
	if !noEmit {
		value, ok := options["outDir"].(string)
		if !ok || value == "" {
			return nil, refuse("TypeScript emission requires an explicit in-worktree outDir")
		}
	}
	for _, key := range []string{"outDir", "declarationDir", "tsBuildInfoFile"} {
		if value, ok := options[key].(string); ok && value != "" {
			if _, err = p.buildOutput(value); err != nil {
				return nil, err
			}
		}
	}
	if options["outFile"] != nil {
		return nil, refuse("TypeScript outFile bundles are not supported; use outDir")
	}
	result := []string{argv[0], "--project", config, "--pretty", "false"}
	for _, key := range []string{"noEmit", "outDir", "rootDir", "declarationDir", "tsBuildInfoFile", "declaration", "emitDeclarationOnly", "incremental"} {
		if value, ok := overrides[key]; ok {
			if key == "rootDir" {
				value, err = p.scoped(value)
			} else if key == "outDir" || key == "declarationDir" || key == "tsBuildInfoFile" {
				value, err = p.buildOutput(value)
			}
			if err != nil {
				return nil, err
			}
			result = append(result, "--"+key, value)
		}
	}
	return result, nil
}
