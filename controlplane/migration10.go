package controlplane

import (
	"context"
	"database/sql"
)

// Migration10 bounds the lease ledger's row (FAE-CP-012). resource_pools.payload
// was a LONGTEXT holding every holder and waiter of a pool, so a role pool's
// row grew with its queue. Pools now spill to blobs past the inline limit
// (encodePool); migration10 spills any pool that outgrew it before, then
// makes the column a TEXT, like every other row column.
var Migration10 = []string{}

func (s *Store) migration10(ctx context.Context, tx *sql.Tx) error {
	pools, err := collect(ctx, tx, `SELECT pool_key,payload FROM resource_pools ORDER BY pool_key LIMIT 100001`, func(r *sql.Rows) ([2]string, error) {
		var p [2]string
		return p, r.Scan(&p[0], &p[1])
	})
	if err != nil {
		return err
	}
	for _, p := range pools {
		stored, err := s.inlineOrRef(ctx, tx, []byte(p[1]))
		if err != nil {
			return err
		}
		if stored == p[1] {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE resource_pools SET payload=? WHERE pool_key=?`, stored, p[0]); err != nil {
			return err
		}
	}
	var unbounded int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='resource_pools' AND column_name='payload' AND LOWER(data_type)<>'text'`).Scan(&unbounded); err != nil || unbounded == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `ALTER TABLE resource_pools MODIFY COLUMN payload TEXT NOT NULL`)
	return err
}
