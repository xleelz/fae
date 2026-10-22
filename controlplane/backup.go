package controlplane

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
)

// BackupPlane copies the Dolt database to a fresh local backup URL without
// changing application rows, branches, or the configured backup remotes.
func (s *Store) BackupPlane(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("absolute backup path required")
	}
	destination := (&url.URL{Scheme: "file", Path: path}).String()
	var success int
	if err := s.db.QueryRowContext(ctx, `CALL DOLT_BACKUP('sync-url', ?)`, destination).Scan(&success); err != nil {
		return err
	}
	if success != 0 && success != 1 {
		return fmt.Errorf("unexpected backup result %d", success)
	}
	return nil
}
