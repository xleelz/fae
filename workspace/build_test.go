package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func plant(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if body == "/" {
			if err := os.MkdirAll(p, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// FAE-ARC-023 acceptance: a planted marker selects the matching provider and
// toolchain; a marker that does not list the package, or lives inside the
// workspace itself, leaves the git-worktree provider.
func TestDetectBuildWorkspacePlantedMarkers(t *testing.T) {
	for _, c := range []struct {
		name, ws, kind, toolchain string
		files                     map[string]string
	}{
		{"standalone", "repo", "", "", map[string]string{"repo/go.mod": "module x\n"}},
		{"brazil", "src/Pkg", "brazil", "brazil-build", map[string]string{"packageInfo": "{}\n", "src/Pkg/Config": "x"}},
		{"brazil nested dir is not a package", "src/Pkg/sub", "", "", map[string]string{"packageInfo": "{}\n", "src/Pkg/sub/x": "x"}},
		{"go.work block", "b/c", "go-work", "go", map[string]string{"go.work": "go 1.22\n\nuse (\n\t./a\n\t./b/c // module c\n)\n"}},
		{"go.work single", "a", "go-work", "go", map[string]string{"go.work": "go 1.22\nuse ./a\n"}},
		{"go.work unlisted", "d", "", "", map[string]string{"go.work": "go 1.22\nuse ./a\n", "d/x": "x"}},
		{"cargo glob", "crates/y", "cargo", "cargo", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\n  \"crates/*\",\n]\nexclude = [\"crates/x\"]\n"}},
		{"cargo excluded", "crates/x", "", "", map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"crates/*\"]\nexclude = [\"crates/x\"]\n"}},
		{"cargo single crate", "sub", "", "", map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n", "sub/x": "x"}},
		{"npm list", "packages/app", "npm", "npm", map[string]string{"package.json": `{"name":"mono","workspaces":["packages/*"]}`}},
		{"npm object", "libs/core", "npm", "npm", map[string]string{"package.json": `{"workspaces":{"packages":["libs/*"]}}`}},
		{"npm single package", "app", "", "", map[string]string{"package.json": `{"name":"one"}`, "app/x": "x"}},
		{"pnpm", "apps/web", "pnpm", "pnpm", map[string]string{"pnpm-workspace.yaml": "packages:\n  - 'apps/*'\n  - '!apps/legacy'\n"}},
		{"pnpm negated", "apps/legacy", "", "", map[string]string{"pnpm-workspace.yaml": "packages:\n  - 'apps/*'\n  - '!apps/legacy'\n"}},
		{"bazel", "third_party/lib", "bazel", "bazel", map[string]string{"MODULE.bazel": "module(name = \"m\")\n"}},
		{"marker inside the workspace", "repo", "", "", map[string]string{"repo/go.work": "go 1.22\nuse .\n"}},
		{"nearest of one kind wins", "outer/inner/pkg", "go-work", "go", map[string]string{"go.work": "go 1.22\nuse ./outer/inner/pkg\n", "outer/inner/go.work": "go 1.22\nuse ./pkg\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := canonicalTemp(t)
			plant(t, root, c.files)
			ws := filepath.Join(root, c.ws)
			os.MkdirAll(ws, 0700)
			got, err := DetectBuildWorkspace(ws)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != c.kind || got.Toolchain != c.toolchain {
				t.Fatalf("%+v", got)
			}
			want := ProviderGitWorktree
			if c.kind != "" {
				want = ProviderBuildBinding
				if got.Digest == "" || filepath.Dir(got.Marker) != got.Root {
					t.Fatalf("unpinned identity %+v", got)
				}
			}
			if got.Provider != want {
				t.Fatalf("provider %s want %s", got.Provider, want)
			}
			if c.name == "nearest of one kind wins" && got.Root != filepath.Join(root, "outer/inner") {
				t.Fatalf("not the nearest marker: %+v", got)
			}
		})
	}
}

// Markers that disagree, or that are present but cannot be validated, fail
// closed rather than guessing a provider.
func TestDetectBuildWorkspaceAmbiguousMarkersFailClosed(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"two kinds claim":        {"go.work": "go 1.22\nuse ./pkg\n", "Cargo.toml": "[workspace]\nmembers = [\"pkg\"]\n"},
		"two kinds at levels":    {"MODULE.bazel": "module()\n", "mid/package.json": `{"workspaces":["pkg"]}`},
		"go.work without go":     {"go.work": "use ./pkg\n"},
		"malformed package.json": {"package.json": `{"workspaces": [`},
		"bad workspaces shape":   {"package.json": `{"workspaces": 3}`},
		"cargo members invalid":  {"Cargo.toml": "[workspace]\nmembers = \"pkg\"\n"},
		"empty pnpm":             {"pnpm-workspace.yaml": "packages:\n"},
		"marker is a directory":  {"go.work/": "/"},
	} {
		t.Run(name, func(t *testing.T) {
			root := canonicalTemp(t)
			plant(t, root, files)
			ws := filepath.Join(root, "pkg")
			if name == "two kinds at levels" {
				ws = filepath.Join(root, "mid", "pkg")
			}
			os.MkdirAll(ws, 0700)
			got, err := DetectBuildWorkspace(ws)
			if !errors.Is(err, ErrAmbiguousBuildWorkspace) || got.Provider != ProviderGitWorktree || got.Kind != "" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	if _, err := DetectBuildWorkspace("relative/path"); err == nil {
		t.Fatal("relative workspace accepted")
	}
}

// The pinned identity is re-verified at time of use: an edited, added or
// removed marker refuses.
func TestVerifyBuildWorkspacePinnedIdentity(t *testing.T) {
	root := canonicalTemp(t)
	plant(t, root, map[string]string{"go.work": "go 1.22\nuse ./pkg\n", "pkg/go.mod": "module p\n"})
	ws := filepath.Join(root, "pkg")
	pinned, err := DetectBuildWorkspace(ws)
	if err != nil || pinned.Kind != "go-work" {
		t.Fatal(pinned, err)
	}
	if _, err = VerifyBuildWorkspace(ws, pinned); err != nil {
		t.Fatal(err)
	}
	// An edit that keeps the package a member keeps the identity (new digest).
	os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.23\nuse (\n\t./pkg\n\t./other\n)\n"), 0600)
	if current, err := VerifyBuildWorkspace(ws, pinned); err != nil || current.Digest == pinned.Digest {
		t.Fatal("member-preserving edit", current, err)
	}
	os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.23\nuse ./other\n"), 0600)
	if _, err = VerifyBuildWorkspace(ws, pinned); err == nil {
		t.Fatal("marker no longer listing the package accepted")
	}
	os.Remove(filepath.Join(root, "go.work"))
	if _, err = VerifyBuildWorkspace(ws, pinned); err == nil {
		t.Fatal("removed marker accepted")
	}
	standalone, _ := DetectBuildWorkspace(ws)
	plant(t, root, map[string]string{"MODULE.bazel": "module()\n"})
	if _, err = VerifyBuildWorkspace(ws, standalone); err == nil {
		t.Fatal("added marker accepted")
	}
}
