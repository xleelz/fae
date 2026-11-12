package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/daemon"
)

func TestImportLegacyFlagsAndValidation(t *testing.T) {
	dir := shortHome(t)

	// 1. Help output lists --source, --from, and --dry-run.
	var helpOut bytes.Buffer
	if err := execute(context.Background(), []string{"import-legacy", "--help", "--json"}, &helpOut); err != nil {
		t.Fatalf("import-legacy --help --json failed: %v", err)
	}
	if !strings.Contains(helpOut.String(), `"name": "source"`) || !strings.Contains(helpOut.String(), `"name": "from"`) || !strings.Contains(helpOut.String(), `"name": "dry-run"`) {
		t.Fatalf("import-legacy --help --json missing flags: %s", helpOut.String())
	}

	// 2. Missing --source/--from returns usage exit code 2.
	var out bytes.Buffer
	err := execute(context.Background(), []string{"import-legacy", "--home", dir}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("missing --source/--from exitCode = %d (%v), want 2", exitCode(err), err)
	}

	// 2b. Conflicting --source and --from returns usage exit code 2.
	err = execute(context.Background(), []string{"import-legacy", "--home", dir, "--source", filepath.Join(dir, "a.json"), "--from", filepath.Join(dir, "b.json")}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("conflicting --source/--from exitCode = %d (%v), want 2", exitCode(err), err)
	}

	// 3. Unknown flag returns usage exit code 2.
	err = execute(context.Background(), []string{"import-legacy", "--home", dir, "--bogus"}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("unknown flag exitCode = %d (%v), want 2", exitCode(err), err)
	}

	// 4. Nonexistent --source file returns exit code 1.
	err = execute(context.Background(), []string{"import-legacy", "--home", dir, "--source", filepath.Join(dir, "missing.json"), "--dry-run"}, &out)
	if exitCode(err) != 1 {
		t.Fatalf("missing file exitCode = %d (%v), want 1", exitCode(err), err)
	}

	// 5. Invalid entries are rejected before importing (including explicit priority < 10).
	invalidCases := []struct {
		name string
		body string
	}{
		{"empty_file", ""},
		{"empty_tasks", `{"tasks":[]}`},
		{"missing_title", `[{"id":"t1","goal":"do something"}]`},
		{"missing_goal", `[{"id":"t1","title":"Task 1"}]`},
		{"negative_priority", `[{"id":"t1","title":"Task 1","goal":"do it","priority":-5}]`},
		{"zero_priority", `[{"id":"t1","title":"Task 1","goal":"do it","priority":0}]`},
		{"low_priority_5", `[{"id":"t1","title":"Task 1","goal":"do it","priority":5}]`},
		{"unsafe_scope", `[{"id":"t1","title":"Task 1","goal":"do it","scope":["../outside.go"]}]`},
		{"duplicate_id", `[{"id":"dup","title":"Task 1","goal":"g1"},{"id":"dup","title":"Task 2","goal":"g2"}]`},
	}
	for _, tc := range invalidCases {
		p := filepath.Join(dir, tc.name+".json")
		if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		err := execute(context.Background(), []string{"import-legacy", "--home", dir, "--source", p, "--dry-run"}, &out)
		if exitCode(err) != 1 {
			t.Fatalf("case %s: exitCode = %d (%v), want 1", tc.name, exitCode(err), err)
		}
	}

	// 6. All three supported formats (JSON object, JSON array, JSONL) parse and dry-run cleanly via --source and --from.
	formats := []struct {
		name string
		flag string
		body string
		want int
	}{
		{
			name: "object.json",
			flag: "--source",
			body: `{"tasks":[{"id":"obj-1","title":"Object Task 1","goal":"Migrate service","workflow":"default","scope":["pkg/svc.go"],"priority":20},{"title":"Object Task 2","description":"Add tests","files":["pkg/svc_test.go"]}]}`,
			want: 2,
		},
		{
			name: "array.json",
			flag: "--from",
			body: `[{"id":"arr-1","title":"Array Task 1","goal":"Fix handler","scope":["api/handler.go"],"priority":15}]`,
			want: 1,
		},
		{
			name: "lines.jsonl",
			flag: "--source",
			body: "{\"id\":\"jl-1\",\"title\":\"JSONL 1\",\"goal\":\"Step 1\",\"priority\":10}\n{\"id\":\"jl-2\",\"title\":\"JSONL 2\",\"description\":\"Step 2\",\"files\":[\"cmd/main.go\"],\"priority\":30}\n",
			want: 2,
		},
	}
	for _, tc := range formats {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := execute(context.Background(), []string{"import-legacy", "--home", dir, tc.flag, p, "--dry-run"}, &out); err != nil {
			t.Fatalf("format %s dry-run failed: %v", tc.name, err)
		}
		var res daemon.ImportLegacyResult
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("format %s decode output: %v (%s)", tc.name, err, out.String())
		}
		if !res.DryRun || res.Count != tc.want || len(res.Tasks) != tc.want {
			t.Fatalf("format %s result = %+v, want DryRun=true Count=%d", tc.name, res, tc.want)
		}
	}

	// 7. Non-dry-run when no instance plane exists fails closed (never provisions a plane).
	validPath := filepath.Join(dir, "array.json")
	out.Reset()
	err = execute(context.Background(), []string{"import-legacy", "--home", dir, "--instance", "noplane", "--source", validPath}, &out)
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "instance plane does not exist") {
		t.Fatalf("no-plane import exitCode = %d (%v), want 1 containing 'instance plane does not exist'", exitCode(err), err)
	}
}

func TestImportLegacyDaemonEndToEndAndIdempotency(t *testing.T) {
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for daemon import-legacy integration test")
	}
	root := planeRoot(t, "fae-import-legacy-", "imp")
	t.Cleanup(func() { reapProcesses(t, root); os.RemoveAll(root) })
	ws := filepath.Join(root, "workspace")
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@localhost", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	home := filepath.Join(root, "instances")
	d, err := daemon.Start(ctx, daemon.Config{
		Root:        home,
		Workspace:   ws,
		Instance:    "imp",
		DoltBinary:  binary,
		MaxInFlight: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.Close(closeCtx)
	}()

	fae := func(args ...string) ([]byte, error) {
		var out bytes.Buffer
		err := execute(ctx, append(args, "--home", home, "--instance", "imp"), &out)
		return out.Bytes(), err
	}

	// Pause drive so imported tasks stay in Pending for deterministic inspection.
	if _, err := fae("message", "--id", "auth-stop", "--text", "Pause drive for import test.", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := fae("drive", "stop", "--authorizing-message", "auth-stop", "--json"); err != nil {
		t.Fatal(err)
	}

	exportPath := filepath.Join(root, "legacy-export.json")
	exportBody := `{
  "tasks": [
    {
      "id": "legacy-auth-01",
      "title": "Port legacy session store",
      "goal": "Migrate session tokens to Dolt controlplane",
      "workflow": "default",
      "scope": ["auth/session.go", "auth/session_test.go"],
      "priority": 25
    },
    {
      "id": "legacy-Backup-02",
      "title": "Add off-host replication",
      "description": "Replicate encrypted age bundles over ssh",
      "files": ["daemon/backup_replication.go"],
      "priority": 15
    }
  ]
}`
	if err := os.WriteFile(exportPath, []byte(exportBody), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Dry-run via daemon RPC returns preview and writes nothing to Dolt.
	rawDry, err := fae("import-legacy", "--from", exportPath, "--dry-run")
	if err != nil {
		t.Fatalf("import-legacy --dry-run failed: %v", err)
	}
	var dryRes daemon.ImportLegacyResult
	if err := json.Unmarshal(rawDry, &dryRes); err != nil {
		t.Fatalf("unmarshal dry-run result: %v (%s)", err, rawDry)
	}
	if !dryRes.DryRun || dryRes.Count != 2 || len(dryRes.Tasks) != 2 {
		t.Fatalf("dry-run result = %+v, want DryRun=true Count=2", dryRes)
	}

	rawBacklog, err := fae("backlog", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawBacklog), "legacy-auth-01") {
		t.Fatalf("dry-run unexpectedly wrote task to Dolt: %s", rawBacklog)
	}

	// 2. Live import via --source admits both tasks, records audit event, and writes import-legacy-audit.json.
	rawImport1, err := fae("import-legacy", "--source", exportPath)
	if err != nil {
		t.Fatalf("import-legacy live failed: %v", err)
	}
	var impRes1 daemon.ImportLegacyResult
	if err := json.Unmarshal(rawImport1, &impRes1); err != nil {
		t.Fatalf("unmarshal import result 1: %v (%s)", err, rawImport1)
	}
	wantAuditPath := filepath.Join(home, "imp", "import-legacy-audit.json")
	if impRes1.DryRun || impRes1.Imported != 2 || impRes1.Skipped != 0 || len(impRes1.TaskIDs) != 2 || impRes1.AuditFile != wantAuditPath {
		t.Fatalf("import result 1 = %+v, want DryRun=false Imported=2 Skipped=0 TaskIDs=2 AuditFile=%s", impRes1, wantAuditPath)
	}
	rawAuditFile, err := os.ReadFile(wantAuditPath)
	if err != nil {
		t.Fatalf("read import-legacy-audit.json: %v", err)
	}
	var auditRec daemon.ImportLegacyAuditRecord
	if err := json.Unmarshal(rawAuditFile, &auditRec); err != nil {
		t.Fatalf("unmarshal import-legacy-audit.json: %v (%s)", err, rawAuditFile)
	}
	if auditRec.Imported != 2 || auditRec.Skipped != 0 || auditRec.Source != exportPath || auditRec.ImportedAt.IsZero() {
		t.Fatalf("unexpected import-legacy-audit.json: %+v", auditRec)
	}

	rawTask1, err := fae("task", "show", "--id", "legacy-auth-01", "--json")
	if err != nil {
		t.Fatalf("task show legacy-auth-01 failed: %v", err)
	}
	if !strings.Contains(string(rawTask1), "Port legacy session store") || !strings.Contains(string(rawTask1), "auth/session.go") {
		t.Fatalf("unexpected task show output: %s", rawTask1)
	}

	// 3. Re-running the same import is idempotent: imported=0, skipped=2.
	rawImport2, err := fae("import-legacy", "--from", exportPath)
	if err != nil {
		t.Fatalf("second import-legacy failed: %v", err)
	}
	var impRes2 daemon.ImportLegacyResult
	if err := json.Unmarshal(rawImport2, &impRes2); err != nil {
		t.Fatalf("unmarshal import result 2: %v (%s)", err, rawImport2)
	}
	if impRes2.DryRun || impRes2.Imported != 0 || impRes2.Skipped != 2 || len(impRes2.TaskIDs) != 0 {
		t.Fatalf("import result 2 = %+v, want DryRun=false Imported=0 Skipped=2 TaskIDs=[]", impRes2)
	}

	// 4. Stop the daemon and perform an offline import while the daemon is stopped (FAE-CLI-051).
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := d.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("close daemon before offline import: %v", err)
	}
	closeCancel()

	offlineExportPath := filepath.Join(root, "legacy-offline.json")
	offlineBody := `[
  {"id":"legacy-auth-01","title":"Port legacy session store","goal":"Migrate session tokens to Dolt controlplane","priority":25},
  {"id":"legacy-offline-03","title":"Offline imported task","goal":"Imported while daemon stopped","scope":["daemon/import_legacy.go"],"priority":20}
]`
	if err := os.WriteFile(offlineExportPath, []byte(offlineBody), 0600); err != nil {
		t.Fatal(err)
	}
	rawOffline, err := fae("import-legacy", "--source", offlineExportPath)
	if err != nil {
		t.Fatalf("offline import-legacy failed: %v", err)
	}
	var offlineRes daemon.ImportLegacyResult
	if err := json.Unmarshal(rawOffline, &offlineRes); err != nil {
		t.Fatalf("unmarshal offline import result: %v (%s)", err, rawOffline)
	}
	if offlineRes.DryRun || offlineRes.Imported != 1 || offlineRes.Skipped != 1 || len(offlineRes.TaskIDs) != 1 || offlineRes.TaskIDs[0] != "legacy-offline-03" || offlineRes.AuditFile != wantAuditPath {
		t.Fatalf("offline import result = %+v, want Imported=1 Skipped=1 TaskIDs=[legacy-offline-03]", offlineRes)
	}
}
