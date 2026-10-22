package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// Recovery only relaunches the recorded child with the same private config. SQL
// connections reconnect through database/sql; durable snapshots remain authority.
func (d *Daemon) restartPlane() error {
	binary := d.config.DoltBinary
	if binary == "" {
		binary = "dolt"
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-d.ctx.Done():
			return d.ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
		port, err := controlplane.Port(d.layout.Data, d.config.Port)
		if err != nil {
			return err
		}
		child, err := controlplane.StartServer(d.childCtx, controlplane.ServerConfig{Binary: binary, DataDir: d.layout.Data, Port: port, Log: d.log, StopWait: 5 * time.Second, ConfigFile: filepath.Join(d.layout.Run, "dolt.yaml"), Env: scopedEnv(d.layout.Root), OwnerRegistry: filepath.Join(d.layout.Run, "plane-processes")})
		if err != nil {
			last = err
			continue
		}
		d.childMu.Lock()
		d.child = child
		d.childMu.Unlock()
		for probe := 0; probe < 100; probe++ {
			ctx, cancel := context.WithTimeout(d.ctx, time.Second)
			err = d.store.Migrate(ctx)
			cancel()
			if err == nil {
				raw, _ := json.Marshal(map[string]any{"attempt": attempt + 1, "pid": child.PID()})
				return d.store.PutMarker(d.ctx, controlplane.Audit{Actor: "daemon", Action: "Dolt recovered", Outcome: "ok"}, "plane-recovery", raw)
			}
			select {
			case <-d.ctx.Done():
				return d.ctx.Err()
			case <-child.Done():
				probe = 100
			case <-time.After(50 * time.Millisecond):
			}
		}
		_ = child.Stop()
		last = err
	}
	return fmt.Errorf("Dolt restart exhausted: %w", last)
}
