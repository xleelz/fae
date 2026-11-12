package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type BlobRef struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// PutBlob stores arbitrarily large content in bounded chunks in the authoritative SQL store.
func (s *Store) PutBlob(ctx context.Context, a Audit, content []byte) (BlobRef, error) {
	ref := BlobRef{Digest: Digest(content), Size: int64(len(content))}
	err := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error { return s.putBlob(ctx, tx, content) })
	return ref, err
}
func (s *Store) putBlob(ctx context.Context, tx *sql.Tx, content []byte) error {
	digest := Digest(content)
	var size int64
	e := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE digest=?`, digest).Scan(&size)
	if e == nil {
		if size != int64(len(content)) {
			return errors.New("blob digest/size mismatch")
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	chunkSize := min(s.options.InlineLimit, 65535)
	chunks := (len(content) + chunkSize - 1) / chunkSize
	for i := 0; i < chunks; i++ {
		if _, e = tx.ExecContext(ctx, `INSERT INTO blob_chunks (digest,chunk_index,data) VALUES (?,?,?)`, digest, i, content[i*chunkSize:min((i+1)*chunkSize, len(content))]); e != nil {
			return e
		}
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO blobs (digest,size,chunks) VALUES (?,?,?)`, digest, len(content), chunks)
	return e
}
func (s *Store) GetBlob(ctx context.Context, ref BlobRef) ([]byte, error) {
	return getBlob(ctx, s.db, ref)
}

// queryer is a connection or a transaction.
type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getBlob(ctx context.Context, q queryer, ref BlobRef) ([]byte, error) {
	if ref.Size < 0 {
		return nil, errors.New("invalid blob size")
	}
	var size int64
	var chunks int
	if e := q.QueryRowContext(ctx, `SELECT size,chunks FROM blobs WHERE digest=?`, ref.Digest).Scan(&size, &chunks); e != nil {
		return nil, e
	}
	if size != ref.Size {
		return nil, errors.New("blob reference size mismatch")
	}
	rows, e := q.QueryContext(ctx, `SELECT chunk_index,data FROM blob_chunks WHERE digest=? ORDER BY chunk_index`, ref.Digest)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var content []byte
	n := 0
	for rows.Next() {
		var i int
		var data []byte
		if e = rows.Scan(&i, &data); e != nil {
			return nil, e
		}
		if i != n {
			return nil, fmt.Errorf("missing blob chunk %d", n)
		}
		content = append(content, data...)
		n++
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if n != chunks || int64(len(content)) != size || Digest(content) != ref.Digest {
		return nil, errors.New("blob integrity failure")
	}
	return content, nil
}
