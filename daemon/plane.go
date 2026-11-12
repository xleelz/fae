package daemon

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
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
	// An orphan of this data directory gets the same terminate-then-kill
	// sequence as a shutdown (FAE-CP-003).
	recovered, cleared, err := processowner.RecoverClearing(ctx, filepath.Join(d.layout.Run, "plane-processes"), d.layout.Data, d.doltStopWait())
	d.logClearedProcessRecords("plane", cleared)
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
	d.child, err = controlplane.StartServer(d.childCtx, controlplane.ServerConfig{Binary: binary, DataDir: d.layout.Data, Port: port, Log: componentStream(d.log, "dolt"), StopWait: d.doltStopWait(), ConfigFile: configFile, Env: env, OwnerRegistry: filepath.Join(d.layout.Run, "plane-processes")})
	if err != nil {
		return err
	}
	if safe, probeErr := controller.Unsandboxed(d.child.PID()); probeErr != nil || !safe {
		return controller.SandboxRefusal("Dolt", d.child.PID(), probeErr)
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
		d.store, d.planeDB, err = openPlaneDB(ctx, sqlcfg.FormatDSN(), time.Second, d.config.MessageMaxAge, d.config.InlineLimit)
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
	// Stored workflow revisions and the event clock are checked before
	// Migrate or any lifecycle event or marker touches the plane, so a
	// refused start leaves an older schema at its version for the binary
	// that wrote it (FAE-WF-093, FAE-CP-006).
	schemaVersion, err := d.preMigrationChecks(ctx)
	if err != nil {
		return err
	}
	if err = d.store.Migrate(ctx); err != nil {
		return err
	}
	// The plane's own consistency is checked before this start writes any
	// event, so a damaged event clock is named rather than failing the first
	// event insert (FAE-CP-006); startupChecks quarantines diverged tasks.
	plane, err := d.startupPlaneConsistency(ctx)
	if err != nil {
		return err
	}
	d.updateStartupReport(func(r *StartupChecks) { r.Plane = plane })
	if schemaVersion == 0 {
		if err = d.startupRevisions(ctx, nil); err != nil {
			return err
		}
	}
	if err = d.auditClearedProcessRecords(ctx, cleared); err != nil {
		return err
	}
	if len(recovered) > 0 {
		raw, _ := json.Marshal(recovered)
		return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "owned Dolt orphan recovered", Inputs: raw, Outcome: "ok"}, "plane-orphan-recovery", raw)
	}
	return nil
}

// preMigrationChecks verifies the stored schema bound, event clock and stored
// workflow revisions on an existing plane before Migrate upgrades the schema
// or any startup marker/event is written (FAE-WF-093).
func (d *Daemon) preMigrationChecks(ctx context.Context) (int, error) {
	if d.planeDB == nil {
		return 0, nil
	}
	read, cancel := context.WithTimeout(ctx, startupReadBound)
	var migrationsTable int
	err := d.planeDB.QueryRowContext(read, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='migrations'`).Scan(&migrationsTable)
	cancel()
	if err != nil {
		return 0, err
	}
	if migrationsTable == 0 {
		return 0, nil
	}
	read, cancel = context.WithTimeout(ctx, startupReadBound)
	var version int
	err = d.planeDB.QueryRowContext(read, `SELECT COALESCE(MAX(version),0) FROM migrations`).Scan(&version)
	cancel()
	if err != nil {
		return 0, err
	}
	if version > controlplane.SchemaVersion {
		return version, fmt.Errorf("schema %d is newer than supported %d", version, controlplane.SchemaVersion)
	}
	if version == 0 {
		return 0, nil
	}
	checkCtx := ctx
	if version < controlplane.SchemaVersion {
		checkCtx = withPreMigrationSchema(ctx)
	}
	read, cancel = context.WithTimeout(ctx, startupReadBound)
	var eventTables int
	err = d.planeDB.QueryRowContext(read, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('event_clock','events')`).Scan(&eventTables)
	cancel()
	if err != nil {
		return version, err
	}
	if eventTables == 2 {
		read, cancel = context.WithTimeout(ctx, startupReadBound)
		var eventClock, lastEvent int64
		if err = d.planeDB.QueryRowContext(read, `SELECT sequence FROM event_clock WHERE id=1`).Scan(&eventClock); err != nil && !errors.Is(err, sql.ErrNoRows) {
			cancel()
			return version, fmt.Errorf("startup consistency check of the control plane: %w", err)
		}
		if err = d.planeDB.QueryRowContext(read, `SELECT COALESCE(MAX(sequence),0) FROM events`).Scan(&lastEvent); err != nil {
			cancel()
			return version, fmt.Errorf("startup consistency check of the control plane: %w", err)
		}
		cancel()
		if eventClock < lastEvent {
			return version, fmt.Errorf("startup consistency check: the control plane's event clock (%d) is behind its last event (%d), so its events cannot be ordered; the plane is damaged: restore it from a backup", eventClock, lastEvent)
		}
	}
	read, cancel = context.WithTimeout(ctx, startupReadBound)
	var jobsTable int
	err = d.planeDB.QueryRowContext(read, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='jobs'`).Scan(&jobsTable)
	cancel()
	if err != nil {
		return version, err
	}
	var snapshots []controlplane.JobSnapshot
	if jobsTable > 0 {
		snapshots, err = d.preMigrationActiveJobs(ctx)
		if err != nil {
			return version, err
		}
	}
	return version, d.startupRevisions(checkCtx, snapshots)
}

// preMigrationActiveJobs reads non-terminal job snapshots using only the
// columns present since schema 2, so startupRevisions can check active task
// pins before Migrate adds newer schema columns (FAE-WF-093).
func (d *Daemon) preMigrationActiveJobs(ctx context.Context) ([]controlplane.JobSnapshot, error) {
	read, cancel := context.WithTimeout(ctx, startupReadBound)
	rows, err := d.planeDB.QueryContext(read, `SELECT job_id,version,workflow_id,workflow_revision,state,payload_digest,payload_size FROM jobs WHERE state NOT IN ('Done','Retired','Cancelled','Obsolete') ORDER BY job_id LIMIT 100001`)
	if err != nil {
		cancel()
		return nil, err
	}
	type rawJob struct {
		s      controlplane.JobSnapshot
		digest string
		size   int64
	}
	var list []rawJob
	for rows.Next() {
		var r rawJob
		if err = rows.Scan(&r.s.ID, &r.s.Version, &r.s.WorkflowID, &r.s.WorkflowRevision, &r.s.State, &r.digest, &r.size); err != nil {
			rows.Close()
			cancel()
			return nil, err
		}
		list = append(list, r)
	}
	err = rows.Err()
	rows.Close()
	cancel()
	if err != nil {
		return nil, err
	}
	out := make([]controlplane.JobSnapshot, 0, len(list))
	for _, r := range list {
		read, cancel := context.WithTimeout(ctx, startupReadBound)
		r.s.Payload, err = d.store.GetBlob(read, controlplane.BlobRef{Digest: r.digest, Size: r.size})
		cancel()
		if err != nil {
			return nil, err
		}
		out = append(out, r.s)
	}
	return out, nil
}

// Bound the entire handshake/Ping, not just TCP dialing. The MySQL connector
// observes this attempt context; cancelling it after Ping does not impose a
// timeout on later statements executed through the returned database pool.
func openPlaneAttempt(ctx context.Context, dsn string, timeout, messageMaxAge time.Duration, inlineLimit int) (*controlplane.Store, error) {
	store, _, err := openPlaneDB(ctx, dsn, timeout, messageMaxAge, inlineLimit)
	return store, err
}

func openPlaneDB(ctx context.Context, dsn string, timeout, messageMaxAge time.Duration, inlineLimit int) (*controlplane.Store, *sql.DB, error) {
	attempt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start, _ := processowner.Birth(os.Getpid())
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(16)
	if err = db.PingContext(attempt); err != nil {
		db.Close()
		return nil, nil, err
	}
	return controlplane.New(db, controlplane.Options{VersionCommits: true, MessageMaxAge: messageMaxAge, InlineLimit: inlineLimit, OwnerPID: os.Getpid(), OwnerStart: start}), db, nil
}

// doltStopWait is the bounded graceful-stop wait (Appendix A: 30 s).
func (d *Daemon) doltStopWait() time.Duration {
	if d.config.DoltStopWait > 0 {
		return d.config.DoltStopWait
	}
	return 30 * time.Second
}

// doltRestartAttempts is the restart bound of one outage (Appendix A: 3).
func (d *Daemon) doltRestartAttempts() int {
	if d.config.DoltRestartAttempts > 0 {
		return d.config.DoltRestartAttempts
	}
	return 3
}

// stopPlane stops the Dolt child at shutdown and logs its termination and
// duration (FAE-CP-002).
func (d *Daemon) stopPlane() {
	d.childMu.Lock()
	child := d.child
	d.childMu.Unlock()
	if child == nil {
		return
	}
	r, err := child.Terminate()
	fmt.Fprintf(componentLog(d.log, "plane"), "Dolt termination pid=%d duration=%s wait=%s forced=%t already_exited=%t error=%v\n", r.PID, r.Duration.Round(time.Millisecond), r.Wait, r.Forced, r.AlreadyExited, err)
}
