package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/controlplane"
)

type ScratchArchiveCleanupParams struct {
	Home       string `json:"home"`
	Instance   string `json:"instance"`
	Source     string `json:"source"`
	Survivor   string `json:"survivor"`
	Apply      bool   `json:"apply"`
	PlanDigest string `json:"plan_digest,omitempty"`
}
type ScratchArchiveCleanupResult struct {
	Source     string `json:"source"`
	Survivor   string `json:"survivor"`
	PlanDigest string `json:"plan_digest"`
	Completed  bool   `json:"completed"`
}
type scratchArchiveIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}
type scratchArchivePlan struct {
	Home             string                 `json:"home"`
	Instance         string                 `json:"instance"`
	Source           string                 `json:"source"`
	Survivor         string                 `json:"survivor"`
	JournalHash      string                 `json:"journal_hash"`
	SourceIdentity   scratchArchiveIdentity `json:"source_identity"`
	SurvivorIdentity scratchArchiveIdentity `json:"survivor_identity"`
	Files            map[string]string      `json:"files"`
}
type scratchArchiveReceipt struct {
	Plan       scratchArchivePlan `json:"plan"`
	PlanDigest string             `json:"plan_digest"`
	Completed  bool               `json:"completed"`
}

func scratchArchiveDirectory(path string) (scratchArchiveIdentity, error) {
	var id scratchArchiveIdentity
	physical, e := filepath.EvalSymlinks(path)
	if e != nil || physical != path {
		return id, errors.New("scratch archive directory must be physical")
	}
	info, e := os.Lstat(path)
	if e != nil {
		return id, e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || st.Uid != uint32(os.Getuid()) {
		return id, errors.New("private owned scratch archive directory required")
	}
	return scratchArchiveIdentity{uint64(st.Dev), uint64(st.Ino)}, nil
}
func scratchArchiveOwnedFiles(ctx context.Context, root string) (map[string]string, error) {
	// Every archived object is regular data; runtime IPC was represented as metadata.
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("foreign or special scratch archive object")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scratchFiles(ctx, root)
}
func scratchArchiveJournal(ctx context.Context, p ScratchArchiveCleanupParams) (ScratchCleanupResult, string, error) {
	var journal ScratchCleanupResult
	l, err := Paths(p.Home, p.Instance)
	if err != nil {
		return journal, "", err
	}
	if _, err = os.Lstat(l.Root); !os.IsNotExist(err) {
		return journal, "", errors.New("active or recreated instance protects scratch archives")
	}
	path := scratchJournalPath(p.Home, p.Instance)
	if err = checkPrivateRegular(path); err != nil {
		return journal, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return journal, "", err
	}
	if err = decode(raw, &journal); err != nil {
		return journal, "", err
	}
	if !CanRemoveScratchArchive(journal.Prepared && journal.Completed && journal.Identity.Root == l.Root && journal.Identity.Instance == p.Instance, p.Survivor == journal.ArchivePath, p.Source != journal.ArchivePath) {
		return journal, "", errors.New("only redundant noncanonical archives of a completed scratch cleanup may be removed")
	}
	if _, err = os.Lstat(filepath.Join(p.Home, ".scratch-removing", journal.Identity.ID)); !os.IsNotExist(err) {
		return journal, "", errors.New("pending scratch removal protects archives")
	}
	parent := filepath.Join(p.Home, ".scratch-archive")
	if _, err = scratchArchiveDirectory(parent); err != nil {
		return journal, "", err
	}
	for _, archive := range []string{p.Source, p.Survivor} {
		if filepath.Dir(archive) != parent || filepath.Clean(archive) != archive || !strings.HasPrefix(filepath.Base(archive), journal.Identity.ID+"-") || len(filepath.Base(archive)) <= len(journal.Identity.ID)+1 {
			return journal, "", errors.New("foreign scratch archive path")
		}
	}
	if err = scratchVerify(ctx, journal); err != nil {
		return journal, "", err
	}
	if err = scratchArchiveGitProof(ctx, journal); err != nil {
		return journal, "", err
	}
	raw, err = json.Marshal(journal)
	if err != nil {
		return journal, "", err
	}
	return journal, controlplane.Digest(raw), nil
}
func scratchArchivePlanDigest(p scratchArchivePlan) string {
	raw, _ := json.Marshal(p)
	return controlplane.Digest(raw)
}
func scratchArchiveSave(path string, r scratchArchiveReceipt) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// CleanupScratchArchive removes only an explicitly reviewed redundant sibling.
// Local filesystem ownership plus Apply and PlanDigest is the operator boundary;
// the completed canonical archive and cleanup journal are never rewritten.
func CleanupScratchArchive(ctx context.Context, p ScratchArchiveCleanupParams) (ScratchArchiveCleanupResult, error) {
	result := ScratchArchiveCleanupResult{Source: p.Source, Survivor: p.Survivor}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !filepath.IsAbs(p.Home) || filepath.Clean(p.Home) != p.Home {
		return result, errors.New("absolute physical scratch home required")
	}
	unlock, err := lifecycleLock(ctx, p.Home, p.Instance)
	if err != nil {
		return result, err
	}
	defer unlock()
	journal, journalHash, err := scratchArchiveJournal(ctx, p)
	if err != nil {
		return result, err
	}
	survivorID, err := scratchArchiveDirectory(p.Survivor)
	if err != nil {
		return result, err
	}
	survivorFiles, err := scratchArchiveOwnedFiles(ctx, p.Survivor)
	if err != nil {
		return result, err
	}
	if !targetEqual(survivorFiles, journal.Files) {
		return result, errors.New("canonical archive content changed")
	}
	receiptRoot := filepath.Join(p.Home, ".scratch-archive", ".removals")
	var receipt scratchArchiveReceipt
	resumed := false
	if p.Apply {
		if !replicationDigest(p.PlanDigest) {
			return result, errors.New("apply requires reviewed plan_digest")
		}
		if _, err = os.Lstat(receiptRoot); err == nil {
			if _, err = scratchArchiveDirectory(receiptRoot); err != nil {
				return result, err
			}
			receiptPath := filepath.Join(receiptRoot, p.PlanDigest+".json")
			if err = checkPrivateRegular(receiptPath); err == nil {
				raw, e := os.ReadFile(receiptPath)
				if e != nil {
					return result, e
				}
				if e = decode(raw, &receipt); e != nil {
					return result, e
				}
				resumed = true
			} else if !os.IsNotExist(err) {
				return result, err
			}
		} else if !os.IsNotExist(err) {
			return result, err
		}
	}
	if resumed {
		plan := receipt.Plan
		if receipt.PlanDigest != p.PlanDigest || scratchArchivePlanDigest(plan) != p.PlanDigest || plan.Home != p.Home || plan.Instance != p.Instance || plan.Source != p.Source || plan.Survivor != p.Survivor || plan.JournalHash != journalHash || plan.SurvivorIdentity != survivorID || !targetEqual(plan.Files, survivorFiles) {
			return result, errors.New("scratch archive removal receipt or survivor changed")
		}
	} else {
		sourceID, e := scratchArchiveDirectory(p.Source)
		if e != nil {
			return result, e
		}
		if sourceID == survivorID {
			return result, errors.New("source is canonical archive identity")
		}
		files, e := scratchArchiveOwnedFiles(ctx, p.Source)
		if e != nil {
			return result, e
		}
		if !targetEqual(files, survivorFiles) {
			return result, errors.New("source is not an exact redundant archive; unique data preserved")
		}
		receipt.Plan = scratchArchivePlan{p.Home, p.Instance, p.Source, p.Survivor, journalHash, sourceID, survivorID, files}
		receipt.PlanDigest = scratchArchivePlanDigest(receipt.Plan)
	}
	result.PlanDigest = receipt.PlanDigest
	if !p.Apply {
		return result, nil
	}
	if p.PlanDigest != receipt.PlanDigest {
		return result, errors.New("stale scratch archive plan")
	}
	if !resumed {
		if err = mkdirPrivate(receiptRoot); err != nil {
			return result, err
		}
		if err = scratchArchiveSyncDirectory(filepath.Dir(receiptRoot)); err != nil {
			return result, err
		}
		if err = scratchArchiveSave(filepath.Join(receiptRoot, receipt.PlanDigest+".json"), receipt); err != nil {
			return result, err
		}
	}
	if err = scratchArchiveRemove(ctx, p, receipt); err != nil {
		return result, err
	}
	receipt.Completed = true
	if err = scratchArchiveSave(filepath.Join(receiptRoot, receipt.PlanDigest+".json"), receipt); err != nil {
		return result, err
	}
	result.Completed = true
	return result, nil
}
func scratchArchiveRemove(ctx context.Context, p ScratchArchiveCleanupParams, receipt scratchArchiveReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(p.Source); os.IsNotExist(err) {
		return scratchArchiveSyncDirectory(filepath.Dir(p.Source))
	} else if err != nil {
		return err
	}
	if receipt.Completed {
		return errors.New("removed scratch archive identity reappeared")
	}
	id, err := scratchArchiveDirectory(p.Source)
	if err != nil {
		return err
	}
	if id != receipt.Plan.SourceIdentity {
		return errors.New("scratch archive source identity changed")
	}
	files, err := scratchArchiveOwnedFiles(ctx, p.Source)
	if err != nil {
		return err
	}
	for path, hash := range files {
		if receipt.Plan.Files[path] != hash {
			return fmt.Errorf("scratch archive changed during removal: %s", path)
		}
	}
	// Remove deepest entries first; partial removal is recoverable from the receipt.
	var paths []string
	err = filepath.WalkDir(p.Source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, path := range paths {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	dir, err := os.Open(filepath.Dir(p.Source))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func scratchArchiveSyncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
