package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

type contextRecorder struct {
	seen     chan string
	delegate *engine.Scripted
}

func (x *contextRecorder) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	select {
	case x.seen <- d.OperatorContext:
	default:
	}
	return x.delegate.Execute(ctx, d)
}

func TestAttachDesignDocContainmentAuditAndNextBrief(t *testing.T) {
	cfg, ctx := fixture(t)
	recorder := &contextRecorder{seen: make(chan string, 1), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = recorder
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "alpha", Content: "alpha", Tree: "tree", AuthorizingMessage: "message"}, "alpha"); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(cfg.Workspace, "docs"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(cfg.Workspace, "docs", "design.md"), []byte("# design"), 0600); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(filepath.Dir(cfg.Workspace), "outside.md")
	if e = os.WriteFile(outside, []byte("# outside"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, filepath.Join(cfg.Workspace, "docs", "escape.md")); e != nil {
		t.Fatal(e)
	}
	for name, p := range map[string]DesignDocParams{
		"outside root":    {Task: "alpha", Reference: outside, AuthorizingMessage: "message"},
		"dot-dot escape":  {Task: "alpha", Reference: "docs/../../outside.md", AuthorizingMessage: "message"},
		"symlink escape":  {Task: "alpha", Reference: "docs/escape.md", AuthorizingMessage: "message"},
		"missing path":    {Task: "alpha", Reference: "docs/absent.md", AuthorizingMessage: "message"},
		"non-http scheme": {Task: "alpha", Reference: "file:///etc/passwd", AuthorizingMessage: "message"},
		"no authority":    {Task: "alpha", Reference: "docs/design.md"},
		"unknown task":    {Task: "missing", Reference: "docs/design.md", AuthorizingMessage: "message"},
		"task and item":   {Task: "alpha", Item: "alpha", Reference: "docs/design.md", AuthorizingMessage: "message"},
		"empty reference": {Task: "alpha", AuthorizingMessage: "message"},
		"control newline": {Task: "alpha", Reference: "https://example.test/a\nb", AuthorizingMessage: "message"},
	} {
		if _, e = call(t, d, "AttachDesignDoc", p, ""); e == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if j, e := d.store.GetJob(ctx, "alpha"); e != nil || strings.Contains(string(j.Payload), "design-doc") {
		t.Fatal("refusal changed the task", e)
	}
	var first, second, url struct {
		ID        string `json:"attachment_id"`
		Reference string `json:"reference"`
		Duplicate bool   `json:"duplicate"`
	}
	r, e := call(t, d, "AttachDesignDoc", DesignDocParams{Task: "alpha", Reference: "docs/design.md", AuthorizingMessage: "message"}, "")
	decodeResult(t, r, e, &first)
	if !strings.HasPrefix(first.ID, "design-") || first.Reference != filepath.Join(cfg.Workspace, "docs", "design.md") {
		t.Fatalf("attachment %s", r.Result)
	}
	r, e = call(t, d, "AttachDesignDoc", DesignDocParams{Item: "alpha", Reference: filepath.Join(cfg.Workspace, "docs", "design.md"), AuthorizingMessage: "message"}, "")
	decodeResult(t, r, e, &second)
	if second.ID != first.ID || !second.Duplicate {
		t.Fatalf("idempotent attachment %s", r.Result)
	}
	r, e = call(t, d, "AttachDesignDoc", DesignDocParams{Task: "alpha", Reference: "https://docs.example.test/design#v2", AuthorizingMessage: "message"}, "")
	decodeResult(t, r, e, &url)
	if url.ID == first.ID || url.Reference != "https://docs.example.test/design#v2" {
		t.Fatalf("url attachment %s", r.Result)
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect attachment audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='operator' AND action='AttachDesignDoc' AND message_id='message'`).Scan(&n); e != nil {
			return e
		}
		if n != 2 {
			return fmt.Errorf("attachment audit rows %d", n)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	// The next brief carries both attachments in its bound operator context.
	d.driveRearm <- struct{}{}
	select {
	case got := <-recorder.seen:
		if !strings.Contains(got, first.ID) || !strings.Contains(got, filepath.Join(cfg.Workspace, "docs", "design.md")) || !strings.Contains(got, "https://docs.example.test/design#v2") {
			t.Fatalf("brief context %q", got)
		}
	case <-ctx.Done():
		t.Fatal("no dispatch")
	}
	awaitState(t, d, "alpha", "Done")
	if _, e = call(t, d, "AttachDesignDoc", DesignDocParams{Task: "alpha", Reference: "https://docs.example.test/late", AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("terminal task accepted an attachment")
	}
}
