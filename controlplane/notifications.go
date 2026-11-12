package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
)

const createTaskNotificationsTableSQL = `CREATE TABLE IF NOT EXISTS task_notifications (notification_id VARCHAR(255) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, from_task_id VARCHAR(255) NOT NULL, kind VARCHAR(64) NOT NULL, delivery VARCHAR(32) NOT NULL, body TEXT NOT NULL, relayed_by VARCHAR(64) NOT NULL, created_at DATETIME(6) NOT NULL, delivered_at DATETIME(6) NULL, INDEX task_notifications_task (task_id, created_at), INDEX task_notifications_from (from_task_id, created_at))`

const createProviderSessionsTableSQL = `CREATE TABLE IF NOT EXISTS provider_sessions (session_key VARCHAR(255) PRIMARY KEY, scope VARCHAR(64) NOT NULL, task_id VARCHAR(255) NOT NULL, workflow_id VARCHAR(255) NOT NULL, workflow_revision VARCHAR(255) NOT NULL, role VARCHAR(255) NOT NULL, provider VARCHAR(255) NOT NULL, model VARCHAR(255) NOT NULL, mode VARCHAR(64) NOT NULL, thread_id VARCHAR(255) NOT NULL, fresh BOOLEAN NOT NULL, reused BOOLEAN NOT NULL, turn_count BIGINT NOT NULL, created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, INDEX provider_sessions_task (task_id))`

// RecordTaskNotificationTx persists a TaskNotification row and records the
// outbound event in from_task's event stream when FromTaskID is non-empty (FAE-OPS-033).
func (s *Store) RecordTaskNotificationTx(ctx context.Context, tx *sql.Tx, n engine.TaskNotification) error {
	if _, err := tx.ExecContext(ctx, createTaskNotificationsTableSQL); err != nil {
		return err
	}
	createdAt := n.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var delivered any
	if !n.DeliveredAt.IsZero() {
		delivered = n.DeliveredAt.UTC()
	}
	if _, err := tx.ExecContext(ctx, `REPLACE INTO task_notifications (notification_id,task_id,from_task_id,kind,delivery,body,relayed_by,created_at,delivered_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		n.ID, n.TaskID, n.FromTaskID, n.Kind, n.Delivery, n.Body, n.RelayedBy, createdAt.UTC(), delivered); err != nil {
		return err
	}
	if n.FromTaskID != "" {
		outboundPayload, _ := json.Marshal(map[string]any{
			"kind":         "task-notification",
			"direction":    "outbound",
			"id":           n.ID,
			"task_id":      n.TaskID,
			"from_task_id": n.FromTaskID,
			"notify_kind":  n.Kind,
			"delivery":     n.Delivery,
			"body":         n.Body,
			"relayed_by":   n.RelayedBy,
			"created_at":   createdAt.UTC(),
		})
		if err := s.appendEvents(ctx, tx, n.FromTaskID, []Event{{Kind: "task-notification", JobID: n.FromTaskID, Payload: outboundPayload}}, ""); err != nil {
			return err
		}
	}
	return nil
}

// MarkTaskNotificationsDeliveredTx updates delivered_at in task_notifications for
// notifications that have been bound into a dispatch.
func (s *Store) MarkTaskNotificationsDeliveredTx(ctx context.Context, tx *sql.Tx, notifications []engine.TaskNotification) error {
	if len(notifications) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, createTaskNotificationsTableSQL); err != nil {
		return err
	}
	for _, n := range notifications {
		if n.ID == "" || n.DeliveredAt.IsZero() {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_notifications SET delivered_at=? WHERE notification_id=?`, n.DeliveredAt.UTC(), n.ID); err != nil {
			return err
		}
	}
	return nil
}

// TaskNotifications returns all notifications targeting taskID ordered by creation time.
func (s *Store) TaskNotifications(ctx context.Context, taskID string) ([]engine.TaskNotification, error) {
	return s.queryTaskNotifications(ctx, `SELECT notification_id,task_id,from_task_id,kind,delivery,body,relayed_by,created_at,delivered_at FROM task_notifications WHERE task_id=? ORDER BY created_at,notification_id`, taskID)
}

// OutboundTaskNotifications returns all notifications sent from fromTaskID ordered by creation time.
func (s *Store) OutboundTaskNotifications(ctx context.Context, fromTaskID string) ([]engine.TaskNotification, error) {
	return s.queryTaskNotifications(ctx, `SELECT notification_id,task_id,from_task_id,kind,delivery,body,relayed_by,created_at,delivered_at FROM task_notifications WHERE from_task_id=? ORDER BY created_at,notification_id`, fromTaskID)
}

func (s *Store) queryTaskNotifications(ctx context.Context, query string, arg string) ([]engine.TaskNotification, error) {
	rows, err := s.db.QueryContext(ctx, query, arg)
	if err != nil {
		if strings.Contains(err.Error(), "doesn't exist") || strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []engine.TaskNotification
	for rows.Next() {
		var n engine.TaskNotification
		var delivered sql.NullTime
		if err := rows.Scan(&n.ID, &n.TaskID, &n.FromTaskID, &n.Kind, &n.Delivery, &n.Body, &n.RelayedBy, &n.CreatedAt, &delivered); err != nil {
			return nil, err
		}
		if delivered.Valid {
			n.DeliveredAt = delivered.Time.UTC()
		}
		n.CreatedAt = n.CreatedAt.UTC()
		out = append(out, n)
	}
	return out, rows.Err()
}

// SaveProviderSessionTx persists a provider.SessionRecord in provider_sessions.
func (s *Store) SaveProviderSessionTx(ctx context.Context, tx *sql.Tx, rec provider.SessionRecord) error {
	if rec.Key == "" || rec.TaskID == "" || rec.ThreadID == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, createProviderSessionsTableSQL); err != nil {
		return err
	}
	created := rec.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	updated := rec.UpdatedAt
	if updated.IsZero() {
		updated = created
	}
	_, err := tx.ExecContext(ctx, `REPLACE INTO provider_sessions (session_key,scope,task_id,workflow_id,workflow_revision,role,provider,model,mode,thread_id,fresh,reused,turn_count,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.Key, rec.Scope, rec.TaskID, rec.WorkflowID, rec.WorkflowRevision, rec.Role, rec.Provider, rec.Model, rec.Mode, rec.ThreadID, rec.Fresh, rec.Reused, rec.TurnCount, created.UTC(), updated.UTC())
	return err
}

// GetTaskProviderSession returns the provider.SessionRecord bound to taskID in provider_sessions.
func (s *Store) GetTaskProviderSession(ctx context.Context, taskID string) (provider.SessionRecord, bool, error) {
	var rec provider.SessionRecord
	err := s.db.QueryRowContext(ctx, `SELECT session_key,scope,task_id,workflow_id,workflow_revision,role,provider,model,mode,thread_id,fresh,reused,turn_count,created_at,updated_at FROM provider_sessions WHERE task_id=? ORDER BY updated_at DESC LIMIT 1`, taskID).
		Scan(&rec.Key, &rec.Scope, &rec.TaskID, &rec.WorkflowID, &rec.WorkflowRevision, &rec.Role, &rec.Provider, &rec.Model, &rec.Mode, &rec.ThreadID, &rec.Fresh, &rec.Reused, &rec.TurnCount, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "doesn't exist") || strings.Contains(err.Error(), "not found") {
			return rec, false, nil
		}
		return rec, false, err
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	rec.UpdatedAt = rec.UpdatedAt.UTC()
	return rec, true, nil
}

// TransferTaskSessionTx moves the task-bound provider session row in provider_sessions
// from fromTaskID to toTaskID without touching any job or task history rows of fromTaskID (FAE-OPS-034).
func (s *Store) TransferTaskSessionTx(ctx context.Context, tx *sql.Tx, fromTaskID, toTaskID string, now time.Time) (provider.SessionRecord, bool, error) {
	if _, err := tx.ExecContext(ctx, createProviderSessionsTableSQL); err != nil {
		return provider.SessionRecord{}, false, err
	}
	var rec provider.SessionRecord
	err := tx.QueryRowContext(ctx, `SELECT session_key,scope,task_id,workflow_id,workflow_revision,role,provider,model,mode,thread_id,fresh,reused,turn_count,created_at,updated_at FROM provider_sessions WHERE task_id=? ORDER BY updated_at DESC LIMIT 1 FOR UPDATE`, fromTaskID).
		Scan(&rec.Key, &rec.Scope, &rec.TaskID, &rec.WorkflowID, &rec.WorkflowRevision, &rec.Role, &rec.Provider, &rec.Model, &rec.Mode, &rec.ThreadID, &rec.Fresh, &rec.Reused, &rec.TurnCount, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rec, false, nil
		}
		return rec, false, err
	}
	if rec.Fresh {
		return rec, false, fmt.Errorf("follow-up-refused: role %s has fresh: true", rec.Role)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_sessions WHERE session_key=?`, rec.Key); err != nil {
		return rec, false, err
	}
	rec.Key = fmt.Sprintf("task-bound:%s:%s:%s:%s", rec.WorkflowID, rec.WorkflowRevision, rec.Role, toTaskID)
	rec.Scope = provider.ScopeTask
	rec.TaskID = toTaskID
	rec.Reused = true
	rec.UpdatedAt = now.UTC()
	if err := s.SaveProviderSessionTx(ctx, tx, rec); err != nil {
		return rec, false, err
	}
	return rec, true, nil
}
