package controlplane

import "context"

// HeldDependents reports existing blocking edges. Cancel never deletes or
// releases them. Backlog admission/edge editing is a separate capability.
func (s *Store) HeldDependents(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT to_id FROM backlog_edges WHERE from_id=? AND kind='blocking' ORDER BY to_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
