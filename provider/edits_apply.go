package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	agentruntime "github.com/xleelz/fae/runtime"
)

// stagedEdit is one edit of a turn on its way into the worktree.
type stagedEdit struct {
	path    string
	mode    os.FileMode
	existed bool
	before  []byte
	// staged is the new content's file beside path until it is renamed into
	// place; written reports that it was.
	staged  string
	written bool
}

// editWritable is access(2)'s W_OK.
const editWritable = 2

// editApplyFault lets a test fail one step of applyEdits ("stage" or
// "rename" of the edit at an index, or "commit" with index -1) to show what a
// failed write leaves. It is nil outside tests.
var editApplyFault func(step string, index int) error

// applyEdits writes a completed turn's edits all or nothing (FAE-ROB-012,
// FAE-ROB-016, FAE-VCS-003), then runs commit, the handoff write. Every
// target is checked first, and every new content is written and synced to a
// staged file beside its target (a hidden .fae-edit-* file in the same
// directory, so the rename never crosses a file system) before anything in
// the worktree changes. Only then is each staged file renamed over its
// target, keeping an existing file's mode. If any step fails, the commit
// included, every file already renamed gets its content from before the turn
// back, staged files and the directories this call created are removed, and
// the result is a typed agentruntime.EditsNotApplied refusal: the worktree is
// as the turn found it, so the turn can be retried. A target the bridge may
// not write (not a regular file, or without write permission) is refused the
// same way before anything is written. A failure to restore is an untyped
// error that names what may be left.
//
// A crash of the bridge inside applyEdits is not covered: before the first
// rename it leaves only staged .fae-edit-* files (and directories created for
// new files) in the worktree; between renames it leaves the edits renamed so
// far in place, the rest staged, and no handoff. The daemon then sees a
// changed tree with no handoff, as before.
func applyEdits(root string, edits []Edit, contents [][]byte, original map[string][]byte, commit func() error) error {
	plan := make([]stagedEdit, len(edits))
	for i, edit := range edits {
		path, e := safePath(root, edit.Path)
		if e != nil {
			return editsNotApplied(fmt.Errorf("edit %q: %w", edit.Path, e))
		}
		plan[i] = stagedEdit{path: path, mode: 0600, before: original[edit.Path]}
		st, e := os.Lstat(path)
		switch {
		case e == nil && !st.Mode().IsRegular():
			return editsNotApplied(fmt.Errorf("edit %q: the target is not a regular file", edit.Path))
		case e == nil && syscall.Access(path, editWritable) != nil:
			// The rename below could replace a file the bridge may not
			// write; such a file stays refused, as an in-place write was.
			return editsNotApplied(fmt.Errorf("edit %q: the target is not writable", edit.Path))
		case e == nil:
			plan[i].existed = true
			plan[i].mode = st.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		case !os.IsNotExist(e):
			return editsNotApplied(fmt.Errorf("edit %q: %w", edit.Path, e))
		}
		for _, other := range edits {
			if strings.HasPrefix(other.Path, edit.Path+"/") {
				return editsNotApplied(fmt.Errorf("edit %q would need %q to be a directory", other.Path, edit.Path))
			}
		}
		for dir := filepath.Dir(path); dir != root; dir = filepath.Dir(dir) {
			st, e := os.Lstat(dir)
			if e == nil && !st.IsDir() {
				return editsNotApplied(fmt.Errorf("edit %q: %s is not a directory", edit.Path, dir))
			}
			if e == nil || !os.IsNotExist(e) {
				break
			}
		}
	}
	var created []string
	undo := func(cause error) error {
		var left []error
		for i := len(plan) - 1; i >= 0; i-- {
			p := &plan[i]
			switch {
			case p.written && p.existed:
				if e := restoreEditTarget(p); e != nil {
					left = append(left, fmt.Errorf("%s keeps the turn's content (%v)", p.path, e))
				}
			case p.written:
				if e := os.Remove(p.path); e != nil && !os.IsNotExist(e) {
					left = append(left, fmt.Errorf("%s, created by the turn, was not removed (%v)", p.path, e))
				}
			case p.staged != "":
				if e := os.Remove(p.staged); e != nil && !os.IsNotExist(e) {
					left = append(left, fmt.Errorf("staged file %s was not removed (%v)", p.staged, e))
				}
			}
		}
		// An empty directory is no change to the task's tree, so one that
		// cannot be removed leaves nothing behind that the engine sees.
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i])
		}
		if len(left) > 0 {
			return fmt.Errorf("provider edits not applied (%v) and the worktree not restored: %w", cause, errors.Join(left...))
		}
		return editsNotApplied(cause)
	}
	for i := range plan {
		p := &plan[i]
		var missing []string
		for dir := filepath.Dir(p.path); dir != root; dir = filepath.Dir(dir) {
			if _, e := os.Lstat(dir); e == nil {
				break
			}
			missing = append(missing, dir)
		}
		for j := len(missing) - 1; j >= 0; j-- {
			if e := os.Mkdir(missing[j], 0755); e == nil {
				created = append(created, missing[j])
			} else if !os.IsExist(e) {
				return undo(fmt.Errorf("edit %q: %w", edits[i].Path, e))
			}
		}
		staged, e := stageEditFile(filepath.Dir(p.path), contents[i], p.mode)
		if staged != "" {
			p.staged = staged
		}
		if e == nil && editApplyFault != nil {
			e = editApplyFault("stage", i)
		}
		if e != nil {
			return undo(fmt.Errorf("edit %q: %w", edits[i].Path, e))
		}
	}
	for i := range plan {
		p := &plan[i]
		var e error
		if editApplyFault != nil {
			e = editApplyFault("rename", i)
		}
		if e == nil {
			e = os.Rename(p.staged, p.path)
		}
		if e != nil {
			return undo(fmt.Errorf("edit %q: %w", edits[i].Path, e))
		}
		p.staged, p.written = "", true
	}
	var e error
	if editApplyFault != nil {
		e = editApplyFault("commit", -1)
	}
	if e == nil {
		e = commit()
	}
	if e != nil {
		return undo(fmt.Errorf("handoff: %w", e))
	}
	return nil
}

// stageEditFile writes content, with mode, to a new hidden file in dir and
// syncs it. It returns the file's name whenever the file was created, so a
// failed write's file can be removed.
func stageEditFile(dir string, content []byte, mode os.FileMode) (string, error) {
	f, e := os.CreateTemp(dir, ".fae-edit-*")
	if e != nil {
		return "", e
	}
	_, e = f.Write(content)
	if e == nil {
		e = f.Chmod(mode)
	}
	if e == nil {
		e = f.Sync()
	}
	return f.Name(), errors.Join(e, f.Close())
}

// restoreEditTarget puts back the content and mode a renamed edit's target had
// before the turn, staged and renamed like the edit itself.
func restoreEditTarget(p *stagedEdit) error {
	staged, e := stageEditFile(filepath.Dir(p.path), p.before, p.mode)
	if e == nil {
		e = os.Rename(staged, p.path)
	}
	if e != nil && staged != "" {
		_ = os.Remove(staged)
	}
	return e
}

// editsNotApplied is the typed refusal of edits that were not written.
func editsNotApplied(cause error) error {
	return &Refusal{Class: agentruntime.EditsNotApplied, Err: fmt.Errorf("the provider's edits were not applied and the worktree is as the turn found it: %w", cause)}
}
