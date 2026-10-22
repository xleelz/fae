package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
)

func scopedEnv(root string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		key := strings.SplitN(entry, "=", 2)[0]
		if strings.HasPrefix(key, "DOLT_") || strings.HasPrefix(key, "FAE_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "DOLT_ROOT_PATH="+filepath.Join(root, "dolt-config"), "DOLT_DISABLE_EVENT_FLUSH=1")
}
func (d *Daemon) startPlane(ctx context.Context) error {
	recovered, err := processowner.Recover(ctx, filepath.Join(d.layout.Run, "plane-processes"), d.layout.Data)
	if err != nil {
		return fmt.Errorf("recover owned Dolt: %w", err)
	}
	binary := d.config.DoltBinary
	if binary == "" {
		binary = "dolt"
	}
	env := scopedEnv(d.layout.Root)
	fresh := false
	if _, err := os.Stat(filepath.Join(d.layout.Data, ".dolt")); os.IsNotExist(err) {
		fresh = true
		cmd := exec.CommandContext(ctx, binary, "init", "--name", "Fae", "--email", "fae@localhost")
		cmd.Dir = d.layout.Data
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("initialize Dolt: %w: %s", err, output)
		}
	} else if err != nil {
		return err
	}
	token, err := api.Credential(filepath.Join(d.layout.Run, "plane-credential"))
	if err != nil {
		return err
	}
	password := hex.EncodeToString(token)
	if fresh {
		cmd := exec.CommandContext(ctx, binary, "sql")
		cmd.Dir = d.layout.Data
		cmd.Env = env
		cmd.Stdin = strings.NewReader("CREATE USER 'fae'@'%' IDENTIFIED BY '" + password + "'; GRANT ALL ON *.* TO 'fae'@'%'; DROP USER IF EXISTS 'root'@'%'; DROP USER IF EXISTS 'root'@'localhost';")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("initialize private Dolt account: %w: %s", err, strings.ReplaceAll(string(output), password, "[redacted]"))
		}
	}
	port, err := controlplane.Port(d.layout.Data, d.config.Port)
	if err != nil {
		return err
	}
	cfg := fmt.Sprintf("log_level: warning\nlistener:\n  host: 127.0.0.1\n  port: %d\ndata_dir: %q\n", port, d.layout.Data)
	configFile := filepath.Join(d.layout.Run, "dolt.yaml")
	if err = writePrivate(configFile, []byte(cfg)); err != nil {
		return err
	}
	d.child, err = controlplane.StartServer(d.childCtx, controlplane.ServerConfig{Binary: binary, DataDir: d.layout.Data, Port: port, Log: d.log, StopWait: 5 * time.Second, ConfigFile: configFile, Env: env, OwnerRegistry: filepath.Join(d.layout.Run, "plane-processes")})
	if err != nil {
		return err
	}
	sqlcfg := mysql.NewConfig()
	sqlcfg.User = "fae"
	sqlcfg.Passwd = password
	sqlcfg.Net = "tcp"
	sqlcfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	sqlcfg.DBName = "plane"
	sqlcfg.ParseTime = true
	sqlcfg.Timeout = time.Second
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		d.store, err = openPlaneAttempt(ctx, sqlcfg.FormatDSN(), time.Second, d.config.MessageMaxAge)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.child.Done():
			return fmt.Errorf("Dolt child exited: %w", d.child.Err())
		case <-deadline.C:
			return fmt.Errorf("Dolt readiness timed out: %w", err)
		case <-ticker.C:
		}
	}
	if err = d.store.Migrate(ctx); err != nil {
		return err
	}
	if len(recovered) > 0 {
		raw, _ := json.Marshal(recovered)
		return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "owned Dolt orphan recovered", Outcome: "ok"}, "plane-orphan-recovery", raw)
	}
	return nil
}

// Bound the entire handshake/Ping, not just TCP dialing. The MySQL connector
// observes this attempt context; cancelling it after Ping does not impose a
// timeout on later statements executed through the returned database pool.
func openPlaneAttempt(ctx context.Context, dsn string, timeout, messageMaxAge time.Duration) (*controlplane.Store, error) {
	attempt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return controlplane.Open(attempt, dsn, controlplane.Options{VersionCommits: true, MessageMaxAge: messageMaxAge})
}
