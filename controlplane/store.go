// Package controlplane provides the daemon's single SQL transaction seam for Dolt.
package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/go-sql-driver/mysql"
)

type Options struct {
	MessageMaxAge time.Duration
	Attempts      int
	Backoff       time.Duration
	// InlineLimit bounds every row column in bytes (FAE-CP-012): a longer
	// payload is stored as a content-addressed blob, or refused where the
	// record must stay inline. Zero selects DefaultInlineLimit.
	InlineLimit    int
	VersionCommits bool
	Sleep          func(context.Context, time.Duration) error
	// OwnerPID and OwnerStart identify the daemon process that owns the
	// sessions the plane records.
	OwnerPID   int
	OwnerStart string
}
type Store struct {
	db      *sql.DB
	options Options
	// fault, when set by tests, interrupts a checkpoint between two writes.
	fault func(step string) error
}

func New(db *sql.DB, o Options) *Store {
	if o.MessageMaxAge <= 0 {
		o.MessageMaxAge = 24 * time.Hour
	}
	if o.Attempts <= 0 {
		o.Attempts = 5
	}
	if o.Backoff <= 0 {
		o.Backoff = 10 * time.Millisecond
	}
	if o.InlineLimit <= 0 || o.InlineLimit > DefaultInlineLimit {
		o.InlineLimit = DefaultInlineLimit
	}
	o.InlineLimit = max(o.InlineLimit, MinInlineLimit)
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	return &Store{db: db, options: o}
}

// The configurable inline limit (FAE-CP-012): 64 KiB by default, the size
// of a TEXT column, and at least 4 KiB, which still fits a blob reference
// and the small records that are refused rather than spilled.
const (
	DefaultInlineLimit = 65536
	MinInlineLimit     = 4096
)

// ValidInlineLimit reports whether a configured inline limit is in range.
func ValidInlineLimit(n int) bool { return n >= MinInlineLimit && n <= DefaultInlineLimit }

// InlineLimit is the store's effective inline limit in bytes.
func (s *Store) InlineLimit() int { return s.options.InlineLimit }
func Open(ctx context.Context, dsn string, o Options) (*Store, error) {
	db, e := sql.Open("mysql", dsn)
	if e != nil {
		return nil, e
	}
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, e
	}
	return New(db, o), nil
}
func (s *Store) Close() error { return s.db.Close() }
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryableError means the entire logical mutation must be deferred to a later round.
type RetryableError struct {
	Attempts int
	Cause    error
}

func (e *RetryableError) Error() string {
	return fmt.Sprintf("control plane transient after %d attempts: %v", e.Attempts, e.Cause)
}
func (e *RetryableError) Unwrap() error { return e.Cause }
func IsSerialization(err error) bool {
	var e *mysql.MySQLError
	return errors.As(err, &e) && (e.Number == 1213 || string(e.SQLState[:]) == "40001")
}

// Audit is persisted atomically with the mutation. Inputs are canonicalized and hashed.
type Audit struct {
	Actor     string
	Action    string
	MessageID string
	Inputs    json.RawMessage
	Outcome   string
}

func (s *Store) Mutate(ctx context.Context, a Audit, body func(context.Context, *sql.Tx) error) error {
	// Internal writes reached through an authorized RPC inherit its citation.
	if authority, ok := ctx.Value(actionAuthorityKey{}).(actionAuthority); ok && a.MessageID == "" {
		a.MessageID = authority.Message
	}
	if a.Actor == "" || a.Action == "" || a.Outcome == "" {
		return errors.New("audit actor, action and outcome required")
	}
	if len(a.Inputs) == 0 {
		a.Inputs = json.RawMessage(`{}`)
	}
	hash, inputs, e := ContentHash(a.Inputs)
	if e != nil {
		return e
	}
	return s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if e := consumeActionAuthority(ctx, tx, a); e != nil {
			return e
		}
		if e := body(ctx, tx); e != nil {
			return e
		}
		// Oversized inputs are stored by reference; the digest still covers
		// the whole canonical input (FAE-CP-012).
		stored, e := s.inlineOrRef(ctx, tx, inputs)
		if e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO audit (actor,action,message_id,inputs,argument_digest,outcome,created_at) VALUES (?,?,?,?,?,?,?)`, a.Actor, a.Action, a.MessageID, stored, hash, a.Outcome, time.Now().UTC()); e != nil {
			return e
		}
		return s.versionCommit(ctx, tx, a.Action+" by "+a.Actor)
	})
}

// A callback must contain SQL only: retries re-execute it, so external side effects are unsafe.
func (s *Store) transaction(ctx context.Context, body func(context.Context, *sql.Tx) error) error {
	var last error
	for i := 0; i < s.options.Attempts; i++ {
		tx, e := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if e == nil {
			e = func() error {
				defer tx.Rollback()
				if e := body(ctx, tx); e != nil {
					return e
				}
				return tx.Commit()
			}()
		}
		if e == nil {
			return nil
		}
		if !IsSerialization(e) {
			return e
		}
		last = e
		if i+1 < s.options.Attempts {
			backoff := s.options.Backoff * time.Duration(1<<min(i, 10))
			if e = s.options.Sleep(ctx, backoff/2+time.Duration(rand.Int64N(int64(backoff/2)+1))); e != nil {
				return e
			}
		}
	}
	return &RetryableError{Attempts: s.options.Attempts, Cause: last}
}
