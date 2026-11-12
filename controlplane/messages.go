package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Migration7 links operator messages into threads with an optional task scope
// and relay provenance. Rows written before it read as unscoped thread roots.
var Migration7 = []string{
	`CREATE TABLE IF NOT EXISTS message_threads (message_id VARCHAR(255) PRIMARY KEY, reply_to VARCHAR(255), task_id VARCHAR(255), channel VARCHAR(32) NOT NULL, recorded_by VARCHAR(255) NOT NULL, source TEXT, INDEX message_task(task_id,message_id), INDEX message_reply(reply_to))`,
	`CREATE INDEX IF NOT EXISTS operator_messages_thread ON operator_messages (thread_root,created_at)`,
}

// MessageInput records an operator message, a reply (ReplyTo names the parent;
// the thread root is inherited) or a controller relay. A relay (non-empty Source)
// is stored with author "relay": it is threaded and visible, but it can never
// satisfy an operator-authority check, because a controller has no authority.
type MessageInput struct {
	ID, Text, ReplyTo, Task string
	Channel, RecordedBy     string
	Source                  string
}
type Message struct {
	ID         string    `json:"id"`
	ThreadRoot string    `json:"thread_root"`
	ReplyTo    string    `json:"reply_to,omitempty"`
	Task       string    `json:"task,omitempty"`
	Author     string    `json:"author"`
	RecordedBy string    `json:"recorded_by"`
	Relayed    bool      `json:"relayed"`
	Source     string    `json:"source,omitempty"`
	Channel    string    `json:"channel"`
	Created    time.Time `json:"created_at"`
	Consumed   []string  `json:"consumed_by,omitempty"`
	Text       string    `json:"text"`
	TextBytes  int64     `json:"text_bytes"`
	Truncated  bool      `json:"truncated,omitempty"`
}
type Thread struct {
	Root     string    `json:"root"`
	Task     string    `json:"task,omitempty"`
	Last     time.Time `json:"last_at"`
	Messages []Message `json:"messages"`
	More     bool      `json:"more_messages,omitempty"`
}

func (s *Store) PostMessage(ctx context.Context, in MessageInput) (Message, error) {
	if in.Channel == "" {
		in.Channel = "operator"
	}
	if in.RecordedBy == "" {
		in.RecordedBy = "operator"
	}
	if in.ID == "" || len(in.ID) > 255 || in.Text == "" || len(in.Text) > 1<<20 || len(in.ReplyTo) > 255 || len(in.Task) > MaxTaskIDBytes || len(in.Source) > 4096 {
		return Message{}, errors.New("message id and text required within bounds")
	}
	if in.Channel != "operator" && in.Channel != "prophet" {
		return Message{}, errors.New("unknown message channel")
	}
	relayed := in.Source != ""
	if (in.RecordedBy == "controller") != relayed || (in.RecordedBy != "operator" && in.RecordedBy != "controller") || (relayed && in.Channel != "operator") {
		return Message{}, errors.New("operator records messages directly; a controller may only relay verbatim operator words with their source")
	}
	m := Message{ID: in.ID, ThreadRoot: in.ID, ReplyTo: in.ReplyTo, Task: in.Task, Author: "operator", RecordedBy: in.RecordedBy, Relayed: relayed, Source: in.Source, Channel: in.Channel, Text: in.Text, TextBytes: int64(len(in.Text))}
	if relayed {
		m.Author = "relay"
	}
	action := "message"
	if in.ReplyTo != "" {
		action = "reply"
	}
	inputs, _ := json.Marshal(map[string]any{"reply_to": in.ReplyTo, "task": in.Task, "channel": in.Channel, "relayed": relayed, "source": in.Source, "text_digest": Digest([]byte(in.Text))})
	err := s.Mutate(ctx, Audit{Actor: in.RecordedBy, Action: action, MessageID: in.ID, Inputs: inputs, Outcome: "recorded"}, func(ctx context.Context, tx *sql.Tx) error {
		m.ThreadRoot, m.Task = in.ID, in.Task
		if in.ReplyTo != "" {
			var root string
			var channel, task sql.NullString
			e := tx.QueryRowContext(ctx, `SELECT m.thread_root,t.channel,t.task_id FROM operator_messages m LEFT JOIN message_threads t ON t.message_id=m.message_id WHERE m.message_id=?`, in.ReplyTo).Scan(&root, &channel, &task)
			if errors.Is(e, sql.ErrNoRows) {
				return fmt.Errorf("reply target %q not found", in.ReplyTo)
			}
			if e != nil {
				return e
			}
			if channel.String == "prophet" || in.Channel != "operator" {
				return errors.New("Prophet chat entries are answered through prophet chat, not reply")
			}
			if in.Task != "" && task.String != "" && in.Task != task.String {
				return errors.New("reply task differs from its thread task")
			}
			m.ThreadRoot = root
			if m.Task == "" {
				m.Task = task.String
			}
		}
		if m.Task != "" {
			var n int
			if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE job_id=?`, m.Task).Scan(&n); e != nil {
				return e
			}
			if n != 1 {
				return fmt.Errorf("message task %q not found", m.Task)
			}
		}
		if e := s.putBlob(ctx, tx, []byte(in.Text)); e != nil {
			return e
		}
		m.Created = time.Now().UTC()
		if _, e := tx.ExecContext(ctx, `INSERT INTO operator_messages (message_id,thread_root,author,text_digest,created_at) VALUES (?,?,?,?,?)`, in.ID, m.ThreadRoot, m.Author, Digest([]byte(in.Text)), m.Created); e != nil {
			return e
		}
		var source any
		if relayed {
			source = in.Source
		}
		_, e := tx.ExecContext(ctx, `REPLACE INTO message_threads (message_id,reply_to,task_id,channel,recorded_by,source) VALUES (?,NULLIF(?,''),NULLIF(?,''),?,?,?)`, in.ID, in.ReplyTo, m.Task, in.Channel, in.RecordedBy, source)
		return e
	})
	return m, err
}

const messageColumns = `m.message_id,m.thread_root,m.author,m.text_digest,b.size,m.created_at,m.consumed_by,t.reply_to,t.task_id,t.channel,t.recorded_by,t.source`
const messageJoin = ` FROM operator_messages m JOIN blobs b ON b.digest=m.text_digest LEFT JOIN message_threads t ON t.message_id=m.message_id`

func (s *Store) scanMessages(ctx context.Context, rows *sql.Rows, textLimit int) ([]Message, error) {
	type row struct {
		m   Message
		ref BlobRef
	}
	var found []row
	for rows.Next() {
		var r row
		var consumed, reply, task, channel, recorded, source sql.NullString
		if e := rows.Scan(&r.m.ID, &r.m.ThreadRoot, &r.m.Author, &r.ref.Digest, &r.ref.Size, &r.m.Created, &consumed, &reply, &task, &channel, &recorded, &source); e != nil {
			rows.Close()
			return nil, e
		}
		r.m.ReplyTo, r.m.Task, r.m.Source = reply.String, task.String, source.String
		r.m.Channel, r.m.RecordedBy = "operator", "operator"
		if channel.Valid {
			r.m.Channel, r.m.RecordedBy = channel.String, recorded.String
		}
		r.m.Relayed = r.m.Author == "relay"
		if consumed.Valid && consumed.String != "" {
			if e := json.Unmarshal([]byte(consumed.String), &r.m.Consumed); e != nil {
				rows.Close()
				return nil, errors.New("invalid message consumption record")
			}
		}
		found = append(found, r)
	}
	e := rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := make([]Message, 0, len(found))
	for _, r := range found {
		r.m.TextBytes = r.ref.Size
		raw, e := s.GetBlob(ctx, r.ref)
		if e != nil {
			return nil, e
		}
		if textLimit > 0 && len(raw) > textLimit {
			raw, r.m.Truncated = []byte(strings.ToValidUTF8(string(raw[:textLimit]), "")), true
		}
		r.m.Text = string(raw)
		out = append(out, r.m)
	}
	return out, nil
}

// MessageThreads lists operator threads by latest activity. A task filter
// selects threads whose root names that task; Prophet chat records are excluded.
func (s *Store) MessageThreads(ctx context.Context, task string, limit, perThread, textLimit int) ([]Thread, error) {
	if limit < 1 || limit > 500 || perThread < 1 || perThread > 500 {
		return nil, errors.New("thread limit and per-thread limit must be 1..500")
	}
	query := `SELECT m.thread_root,MAX(m.created_at) AS last FROM operator_messages m LEFT JOIN message_threads t ON t.message_id=m.message_id LEFT JOIN message_threads r ON r.message_id=m.thread_root WHERE COALESCE(t.channel,'operator')='operator'`
	args := []any{}
	if task != "" {
		query += ` AND r.task_id=?`
		args = append(args, task)
	}
	query += ` GROUP BY m.thread_root ORDER BY last DESC,m.thread_root LIMIT ?`
	args = append(args, limit)
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	threads := []Thread{}
	for rows.Next() {
		var t Thread
		if e = rows.Scan(&t.Root, &t.Last); e != nil {
			rows.Close()
			return nil, e
		}
		threads = append(threads, t)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for i := range threads {
		rows, e := s.db.QueryContext(ctx, `SELECT `+messageColumns+messageJoin+` WHERE m.thread_root=? ORDER BY m.created_at,m.message_id LIMIT ?`, threads[i].Root, perThread+1)
		if e != nil {
			return nil, e
		}
		if threads[i].Messages, e = s.scanMessages(ctx, rows, textLimit); e != nil {
			return nil, e
		}
		if len(threads[i].Messages) > perThread {
			threads[i].Messages, threads[i].More = threads[i].Messages[:perThread], true
		}
		for _, m := range threads[i].Messages {
			if m.ID == threads[i].Root {
				threads[i].Task = m.Task
			}
		}
	}
	return threads, nil
}

// TaskMessages returns the latest operator-channel messages explicitly scoped to
// one task, oldest first. Unscoped messages are never enumerated for a task.
func (s *Store) TaskMessages(ctx context.Context, task string, limit, textLimit int) ([]Message, int, error) {
	if task == "" || limit < 1 || limit > 500 {
		return nil, 0, errors.New("task and limit 1..500 required")
	}
	var total int
	if e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_threads WHERE task_id=? AND channel='operator'`, task).Scan(&total); e != nil {
		return nil, 0, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT `+messageColumns+messageJoin+` WHERE t.task_id=? AND t.channel='operator' ORDER BY m.created_at DESC,m.message_id DESC LIMIT ?`, task, limit)
	if e != nil {
		return nil, 0, e
	}
	out, e := s.scanMessages(ctx, rows, textLimit)
	if e != nil {
		return nil, 0, e
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, total, nil
}

// MessageDirection returns the explicit structured direction of a message.
func (s *Store) MessageDirection(ctx context.Context, id string) (Direction, error) {
	text, e := s.MessageText(ctx, id)
	if e != nil {
		return Direction{}, e
	}
	var msg DirectedMessage
	_ = json.Unmarshal([]byte(text), &msg)
	return msg.Authorize, nil
}

// ListOpenJobs reads every non-terminal job snapshot from the plane.
func (s *Store) ListOpenJobs(ctx context.Context, limit int) ([]JobSnapshot, error) {
	if limit < 1 || limit > 100000 {
		return nil, errors.New("job limit must be 1..100000")
	}
	rows, e := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE state NOT IN ('Done','Retired','Cancelled') ORDER BY job_id LIMIT ?`, limit+1)
	if e != nil {
		return nil, e
	}
	var jobs []JobSnapshot
	var refs []BlobRef
	for rows.Next() {
		j, r, e := scanJob(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		jobs, refs = append(jobs, j), append(refs, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(jobs) > limit {
		return nil, errors.New("open job listing exceeds limit; refusing incomplete snapshot")
	}
	for i := range jobs {
		if jobs[i].Payload, e = s.GetBlob(ctx, refs[i]); e != nil {
			return nil, e
		}
	}
	return jobs, nil
}
