package commandshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func tsPolicyFixture(t *testing.T) (Policy, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src/math.ts"), []byte("export function add(a: number,b: number): number {return a+b;}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "tsconfig.json")
	if err := os.WriteFile(config, []byte(`{"compilerOptions":{"strict":true,"target":"ES2022","module":"commonjs","rootDir":"src","outDir":"dist"},"files":["src/math.ts"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return Policy{Worktree: root, CWD: root, CodeRole: true, LeaseHeld: true}, config
}
func TestTypeScriptScopedCompilerConfiguration(t *testing.T) {
	p, config := tsPolicyFixture(t)
	for _, args := range [][]string{{"tsc", "--project", config, "--noEmit"}, {"tsc", "-p", config}, {"tsc", "-p", config, "--outDir", "build"}} {
		prepared, err := p.PrepareTypeScript(args, nil)
		if err != nil {
			t.Fatal(args, err)
		}
		if !slices.Contains(prepared, "--project") || !slices.Contains(prepared, "false") {
			t.Fatal(prepared)
		}
	}
	for _, args := range [][]string{{"tsc", "--watch", "-p", config}, {"tsc", "--build", config}, {"tsc", "-p", config, "--outDir", t.TempDir()}, {"tsc", "-p", config, "--generateTrace", "trace"}, {"node", "-e", "process.exit()"}} {
		if _, err := p.PrepareTypeScript(args, nil); err == nil {
			t.Fatal("unsupported or escaping compiler vector accepted", args)
		}
	}
	p.LeaseHeld = false
	if _, err := p.PrepareTypeScript([]string{"tsc", "-p", config}, nil); err == nil {
		t.Fatal("unleased compiler accepted")
	}
}
func TestTypeScriptConfigJSONCAndEscapingReferences(t *testing.T) {
	p, config := tsPolicyFixture(t)
	outside := t.TempDir()
	for name, body := range map[string]string{
		"extends":   `{"extends":"` + filepath.Join(outside, "tsconfig.json") + `","compilerOptions":{"noEmit":true}}`,
		"reference": `{"references":[{"path":"` + outside + `"}],"compilerOptions":{"noEmit":true}}`,
		"output":    `{"compilerOptions":{"outDir":"` + outside + `"}}`,
		"inputs":    `{"compilerOptions":{"noEmit":true},"files":["../external.ts"]}`,
		"plugin":    `{"compilerOptions":{"noEmit":true,"plugins":[{"name":"external"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(config, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := p.PrepareTypeScript([]string{"tsc", "-p", config}, nil); err == nil {
				t.Fatal("escaping/executable configuration accepted")
			}
		})
	}
	body := "{ // comment\n \"compilerOptions\": {\"noEmit\":true,}, /* block */ \"files\":[\"src/math.ts\",],}"
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareTypeScript([]string{"tsc", "-p", config}, nil); err != nil {
		t.Fatal("ordinary JSONC refused", err)
	}
}
func TestNodeTestsClampWorkersAndRejectRuntimeInjection(t *testing.T) {
	p, _ := tsPolicyFixture(t)
	file := filepath.Join(p.Worktree, "src/math.test.ts")
	if err := os.WriteFile(file, []byte("// erasable TypeScript test"), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := p.PrepareTypeScript([]string{"node", "--test", "--test-concurrency=99", "--v8-pool-size=32", "--test-name-pattern=^exact$", file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(prepared, "--test-concurrency=1") || !slices.Contains(prepared, "--v8-pool-size=1") || !slices.Contains(prepared, "--test-reporter=tap") {
		t.Fatal(prepared)
	}
	for _, args := range [][]string{{"node", "--test", "--import=tsx", file}, {"node", "--test", "--test-name-pattern=.*", file}, {"node", "--test", "--test-skip-pattern=exact", file}, {"node", "--test", "src/*.ts"}, {"node", "--test", "--test-reporter=json", file}} {
		if _, err = p.PrepareTypeScript(args, nil); err == nil {
			t.Fatal("unbounded Node vector accepted", args)
		}
	}
	for _, entry := range []string{"NODE_OPTIONS=--require=evil", "NODE_PATH=/outside", "NODE_V8_COVERAGE=/outside", "NODE_COMPILE_CACHE=/outside", "TS_NODE_PROJECT=/outside"} {
		if _, err = ClampTypeScriptEnv([]string{entry}); err == nil {
			t.Fatal("runtime injection accepted", entry)
		}
	}
	env, err := TypeScriptEnvironment([]string{"node", "--test", file}, []string{"NODE_ENV=test", "UV_THREADPOOL_SIZE=16"}, t.TempDir())
	if err != nil || !slices.Contains(env, "UV_THREADPOOL_SIZE=1") || !slices.Contains(env, "NODE_DISABLE_COMPILE_CACHE=1") {
		t.Fatal(env, err)
	}
}
func TestTypeScriptRealCompilerTypecheckAndScopedEmit(t *testing.T) {
	compiler := os.Getenv("FAE_TEST_TSC_BINARY")
	if compiler == "" {
		compiler, _ = exec.LookPath("tsc")
	}
	if compiler == "" {
		t.Skip("installed TypeScript compiler required; no package downloads")
	}
	p, config := tsPolicyFixture(t)
	env, err := TypeScriptEnvironment([]string{compiler, "-p", config}, os.Environ(), p.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{{"--noEmit"}, {}} {
		argv := append([]string{compiler, "--project", config}, extra...)
		prepared, e := p.PrepareTypeScript(argv, env)
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(prepared[0], prepared[1:]...)
		cmd.Dir, cmd.Env = p.Worktree, env
		if output, e := cmd.CombinedOutput(); e != nil {
			t.Fatal(e, string(output))
		}
	}
	if data, err := os.ReadFile(filepath.Join(p.Worktree, "dist/math.js")); err != nil || !strings.Contains(string(data), "function add") {
		t.Fatal("real compiler did not emit scoped JS", err)
	}
	if err = os.WriteFile(filepath.Join(p.Worktree, "src/math.ts"), []byte(`const wrong: number = "type error";`), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := p.PrepareTypeScript([]string{compiler, "-p", config, "--noEmit"}, env)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(prepared[0], prepared[1:]...)
	cmd.Dir, cmd.Env = p.Worktree, env
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "TS2322") {
		t.Fatal("real type error was accepted", err, string(output))
	}
}
