package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/controlplane"
)

// HostAlertSpool is the instance's private directory where brokered commands
// (role shims through `heavy-op run`, `gate run`) record hard-breaker refusals.
func HostAlertSpool(l Layout) string { return filepath.Join(l.Run, "host-alerts") }

func (d *Daemon) hostAlertSpool() string { return HostAlertSpool(d.layout) }

// ingestHostAlerts raises the hard refusals brokered commands spooled
// (FAE-HOST-003) on the daemon's single alert path, one alert per hard
// pressure episode shared with the daemon's own admissions, and removes each
// record once it is handled. A record whose raise failed stays for the next
// round. An empty spool costs one directory read and no plane access.
func (d *Daemon) ingestHostAlerts(ctx context.Context) error {
	spool := d.hostAlertSpool()
	entries, err := os.ReadDir(spool)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(spool, name)
		var a commandshim.HostAlert
		if raw, e := readSpooled(path); e != nil || json.Unmarshal(raw, &a) != nil || a.Reason == "" {
			d.logLine("admission", "host alert record unreadable path=%q; removed\n", path)
			_ = os.Remove(path)
			continue
		}
		var guard *liveAdmission
		if d.live != nil {
			guard = d.live.guard
		}
		if guard != nil && !guard.hardAlerted.CompareAndSwap(false, true) {
			d.logLine("admission", "host admission refused gate=heavy-op outcome=stop reason=%s id=%s (alert episode already open)\n", a.Reason, a.ID)
			_ = os.Remove(path)
			continue
		}
		body, _ := json.Marshal(map[string]any{"reason": a.Reason, "gate": "heavy-op", "id": a.ID, "at": a.At, "memory_available": a.MemoryAvailable, "swap_total": a.SwapTotal})
		d.logLine("admission", "host admission refused gate=heavy-op outcome=stop reason=%s id=%s\n", a.Reason, a.ID)
		if _, _, e := d.raiseAlert(ctx, controlplane.Alert{Severity: "error", Source: "host-admission", Detail: body}, false); e != nil {
			if guard != nil {
				guard.hardAlerted.Store(false)
			}
			return e
		}
		_ = os.Remove(path)
	}
	return nil
}

func readSpooled(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}
