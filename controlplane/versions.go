package controlplane

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// Commit is one Dolt commit of the plane's history (FAE-CP-008).
type Commit struct {
	Hash    string    `json:"hash"`
	Message string    `json:"message"`
	Date    time.Time `json:"date"`
}

// Commits reads the newest Dolt commits, newest first.
func (s *Store) Commits(ctx context.Context, limit int) ([]Commit, error) {
	if limit < 1 || limit > 10000 {
		return nil, errors.New("commit limit must be 1..10000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT commit_hash,message,date FROM dolt_log ORDER BY date DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Commit{}
	for rows.Next() {
		var c Commit
		if err = rows.Scan(&c.Hash, &c.Message, &c.Date); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

var diffIdentifier = regexp.MustCompile(`^[a-z_]{1,64}$`)
var commitHash = regexp.MustCompile(`^[0-9a-v]{32}$`)

// ChangedKeys reads the distinct values of one key column among the rows a
// commit changed in one table, against its first parent.
func (s *Store) ChangedKeys(ctx context.Context, commit, table, column string) ([]string, error) {
	if !commitHash.MatchString(commit) || !diffIdentifier.MatchString(table) || !diffIdentifier.MatchString(column) {
		return nil, errors.New("commit hash, table and column required")
	}
	// Dolt's table functions take literal arguments; all three are validated.
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT COALESCE(to_`+column+`,from_`+column+`) FROM DOLT_DIFF('`+commit+`~1', '`+commit+`', '`+table+`') LIMIT 100001`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}
