package daemon

import "os"

// writeReady publishes a crash helper's ready file atomically: the parent
// polls for it and decodes what it reads, so a partial write under host load
// must never be visible.
func writeReady(path string, data []byte) error {
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
