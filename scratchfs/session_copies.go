package scratchfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xleelz/fae/scheduler"
)

const copyOwnerSuffix = ".fae-copy-owner.json"

type copyOwner struct {
	Directory  string          `json:"directory"`
	SessionDir string          `json:"session_dir"`
	Owner      scheduler.Owner `json:"owner"`
}

func RegisterSessionCopy(directory, sessionDir string) error {
	if filepath.Dir(directory) != filepath.Clean(sessionDir) || !copyName(filepath.Base(directory)) {
		return fmt.Errorf("private copy must be a direct owned session child")
	}
	owner, err := scheduler.CurrentOwner()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(copyOwner{directory, sessionDir, owner})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(directory+copyOwnerSuffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
func copyName(name string) bool {
	return strings.HasPrefix(name, "filesystem-view-") || strings.HasPrefix(name, "directory-check-")
}
func sessionName(name string) bool {
	return strings.HasPrefix(name, "session-") || strings.HasPrefix(name, "command-policy-")
}

// ReapSessionCopies visits at most 128 launch directories per sweep, advancing
// a cursor across retained logs. Only marked copies with proven-dead host birth
// identities are removed. Task outputs, provider state and foreign paths stay.
func ReapSessionCopies(root string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return fmt.Errorf("copy reaper requires an absolute owned root")
	}
	physical, err := filepath.EvalSymlinks(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if physical != filepath.Clean(root) {
		return fmt.Errorf("copy reaper root is not physical")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	sessions := []string{}
	for _, entry := range entries {
		if entry.IsDir() && sessionName(entry.Name()) {
			sessions = append(sessions, entry.Name())
		}
	}
	sort.Strings(sessions)
	if len(sessions) == 0 {
		return nil
	}
	cursorPath := filepath.Join(root, ".copy-reaper-cursor")
	raw, _ := os.ReadFile(cursorPath)
	cursor := string(raw)
	start := sort.SearchStrings(sessions, cursor)
	for start < len(sessions) && sessions[start] <= cursor {
		start++
	}
	if start == len(sessions) {
		start = 0
	}
	count := len(sessions)
	if count > 128 {
		count = 128
	}
	for i := 0; i < count; i++ {
		name := sessions[(start+i)%len(sessions)]
		session := filepath.Join(root, name)
		children, e := os.ReadDir(session)
		if e != nil {
			return e
		}
		for _, child := range children {
			if !child.IsDir() || !copyName(child.Name()) {
				continue
			}
			directory := filepath.Join(session, child.Name())
			marker := directory + copyOwnerSuffix
			info, e := os.Lstat(marker)
			if e != nil {
				continue
			}
			if !info.Mode().IsRegular() || info.Size() > 4096 {
				continue
			}
			raw, e := os.ReadFile(marker)
			if e != nil {
				continue
			}
			var owner copyOwner
			if json.Unmarshal(raw, &owner) != nil || owner.Directory != directory || owner.SessionDir != session {
				continue
			}
			if scheduler.ProbeOwner(owner.Owner) != scheduler.Dead {
				continue
			}
			if e = RemoveSessionCopy(directory); e != nil {
				return e
			}
		}
		if e = os.WriteFile(cursorPath, []byte(name), 0600); e != nil {
			return e
		}
	}
	return nil
}

// RemoveSessionCopy also removes its sibling birth marker. Keeping that marker
// outside the copy lets dead-owner cleanup handle mode-0000 private directories.
func RemoveSessionCopy(directory string) error {
	if err := RemoveAll(directory); err != nil {
		return err
	}
	if err := os.Remove(directory + copyOwnerSuffix); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
