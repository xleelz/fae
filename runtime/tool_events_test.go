package runtime

import (
	"context"
	"github.com/xleelz/fae/domain"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolMonitorBoundedLinesAndFinalDrain(t *testing.T) {
	dir := t.TempDir()
	raw := strings.Repeat("x", 20000) + "\n" + `{"tool":"read","allowed":true}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "tool-events.jsonl"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var events []domain.ToolEvent
	stop := monitorToolEvents(context.Background(), dir, func(e domain.ToolEvent) { events = append(events, e) })
	stop()
	if len(events) != 1 || events[0].Tool != "read" {
		t.Fatalf("events: %#v", events)
	}
}
