package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/daemon"
)

func goTool(t *testing.T, args ...string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	gobin, err := exec.LookPath("go")
	if err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(gobin, args...)
	cmd.Dir = filepath.Dir(file)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, raw)
	}
	return string(raw)
}

// buildUIChild builds the separate fae-ui executable into dir.
func buildUIChild(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, daemon.UIBinaryName)
	goTool(t, "build", "-o", out, "github.com/xleelz/fae/cmd/fae-ui")
	return out
}

// FAE-GEN-010 non-goal 6: the core fae binary carries the generic UI child
// supervision (FAE-API-033) but no operator UI: the page server and its
// embedded assets live only in the separate fae-ui binary, and the terminal
// UI and its charm modules only in fae-tui.
func TestCoreBinaryExcludesTheOperatorUI(t *testing.T) {
	core := strings.Fields(goTool(t, "list", "-deps", "github.com/xleelz/fae/cmd/fae"))
	for _, p := range core {
		if p == "github.com/xleelz/fae/tui" || strings.HasPrefix(p, "github.com/xleelz/fae/tui/") || strings.HasPrefix(p, "charm.land/") || strings.HasPrefix(p, "github.com/charmbracelet/") {
			t.Fatalf("the core binary links the terminal UI: %s", p)
		}
	}
	if tui := strings.Fields(goTool(t, "list", "-deps", "github.com/xleelz/fae/cmd/fae-tui")); !slices.Contains(tui, "github.com/xleelz/fae/tui") || !slices.Contains(tui, "charm.land/bubbletea/v2") || slices.Contains(tui, "github.com/xleelz/fae/daemon") {
		t.Fatal("fae-tui must hold the terminal UI and reach the daemon only over RPC")
	}
	if !slices.Contains(core, "github.com/xleelz/fae/ui") || slices.Contains(core, "github.com/xleelz/fae/ui/server") {
		t.Fatalf("core binary UI packages: %v", slices.DeleteFunc(core, func(p string) bool { return !strings.Contains(p, "/ui") }))
	}
	if imports := strings.Fields(goTool(t, "list", "-f", `{{join .Imports " "}} {{join .EmbedPatterns " "}}`, "github.com/xleelz/fae/ui")); slices.Contains(imports, "net/http") || slices.Contains(imports, "embed") || slices.Contains(imports, "assets") {
		t.Fatalf("the generic UI contract package serves pages: %v", imports)
	}
	if child := strings.Fields(goTool(t, "list", "-deps", "github.com/xleelz/fae/cmd/fae-ui")); !slices.Contains(child, "github.com/xleelz/fae/ui/server") || slices.Contains(child, "github.com/xleelz/fae/daemon") {
		t.Fatal("fae-ui must hold the UI server and reach the daemon only over RPC")
	}
}
