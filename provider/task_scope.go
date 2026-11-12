package provider

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/xleelz/fae/workflow"
)

// Default inline brief bounds (FAE-RT-017, Appendix A).
const (
	DefaultInlineFileBytes = 1 << 20
	DefaultInlineTurnBytes = 4 << 20
)

// maxProviderFilesBytes bounds the total size of the task files one turn
// sends to the provider. Each file is also bounded to 1 MiB by regularRead.
const maxProviderFilesBytes = DefaultInlineTurnBytes

// ScopeFileMetadata records metadata for an in-scope or read-only file whose
// full content exceeds InlineFileBytes or InlineTurnBytes in an agentic turn
// (FAE-RT-017).
type ScopeFileMetadata struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Lines  int    `json:"lines"`
	SHA256 string `json:"sha256"`
	Scope  string `json:"scope"`
}

// UnavailableScopeEntry records a file that could not be inlined in a
// tool-less turn because it exceeded InlineFileBytes or InlineTurnBytes
// (FAE-RT-017).
type UnavailableScopeEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes,omitempty"`
	Reason string `json:"reason"`
}

// TaskScope captures a turn's separated editable write scope, read-only
// inputs, inline file contents, large-file metadata (for agentic turns), and
// unavailable scope entries (for tool-less turns) under FAE-RT-017.
type TaskScope struct {
	Session          string                  `json:"session,omitempty"`
	Files            []string                `json:"files,omitempty"`
	ReadOnlyFiles    []string                `json:"read_only_files,omitempty"`
	TestFile         string                  `json:"test_file,omitempty"`
	InlineFileBytes  int                     `json:"inline_file_bytes,omitempty"`
	InlineTurnBytes  int                     `json:"inline_turn_bytes,omitempty"`
	WriteScope       map[string]string       `json:"write_scope,omitempty"`
	ReadOnlyInputs   map[string]string       `json:"read_only_inputs,omitempty"`
	Metadata         []ScopeFileMetadata     `json:"metadata,omitempty"`
	UnavailableScope []UnavailableScopeEntry `json:"unavailable_scope,omitempty"`
}

// briefTaskScope is the executor's task_scope brief block: the task's current
// files (approved scope widens included), read-only input files, and its RED
// test file once the task has a RED report.
type briefTaskScope struct {
	Files         []string                `json:"files"`
	ReadOnlyFiles []string                `json:"read_only_files,omitempty"`
	TestFile      string                  `json:"test_file"`
	Reads         *workflow.ReadPolicy    `json:"reads,omitempty"`
	DenyPaths     []string                `json:"deny_paths,omitempty"`
	ReadRoots     map[string]ReadRootView `json:"read_roots,omitempty"`
}

// taskScopeFromBrief returns the brief's task_scope block, or nil when the
// brief has none. More than one block is refused.
func taskScopeFromBrief(raw json.RawMessage) (*briefTaskScope, error) {
	var blocks []json.RawMessage
	if e := json.Unmarshal(raw, &blocks); e != nil {
		return nil, e
	}
	var found *briefTaskScope
	for _, b := range blocks {
		var kind struct {
			Kind string `json:"kind"`
		}
		if e := json.Unmarshal(b, &kind); e != nil {
			return nil, e
		}
		if kind.Kind != "task_scope" {
			continue
		}
		if found != nil {
			return nil, errors.New("brief has more than one task_scope block")
		}
		found = &briefTaskScope{}
		if e := json.Unmarshal(b, found); e != nil {
			return nil, fmt.Errorf("task_scope block: %w", e)
		}
	}
	return found, nil
}

// settleTaskScope returns the turn's files and RED test file. A pinned scope
// (a --task-scopes entry or a non-empty --files) wins over the brief's
// task_scope block, never a union, but a test file the block names must equal
// the pinned one: a mismatch means the daemon and the bridge disagree. A pin
// without a test file disagrees only on a change turn (editing); a turn that
// cannot edit doesn't use the test file, as before blocks existed. Without a
// pin, the block is the scope. Every pinned path (--files, --test-file or a
// --task-scopes entry) and every block path follows the one declared-path
// rule (workflow.DeclaredPathSafe: never in .git at any depth), and a block
// path must also be a safe worktree path, except an approved scope-widen
// file, the RED test file included: loadScopeFiles lists one it can't load as
// unavailable instead.
func settleTaskScope(root string, brief json.RawMessage, files []string, testFile string, pinned, editing bool, approved []string) ([]string, string, error) {
	if !pinned && testFile != "" {
		return nil, "", errors.New("pinned test file requires pinned files")
	}
	if pinned {
		paths := slices.Clone(files)
		if testFile != "" {
			paths = append(paths, testFile)
		}
		for _, name := range paths {
			if !workflow.DeclaredPathSafe(name) {
				return nil, "", fmt.Errorf("pinned scope path %q is unsafe: a pinned file is a clean relative path, never in .git at any depth", name)
			}
		}
	}
	block, e := taskScopeFromBrief(brief)
	if e != nil || block == nil {
		return files, testFile, e
	}
	paths := block.Files
	if block.TestFile != "" {
		paths = append(slices.Clone(paths), block.TestFile)
	}
	for _, name := range paths {
		if !workflow.DeclaredPathSafe(name) {
			return nil, "", fmt.Errorf("task_scope path %q: unsafe proposed path", name)
		}
		if slices.Contains(approved, name) {
			continue
		}
		if _, e := safePath(root, name); e != nil {
			return nil, "", fmt.Errorf("task_scope path %q: %w", name, e)
		}
	}
	if pinned {
		if block.TestFile != "" && block.TestFile != testFile && (testFile != "" || editing) {
			return nil, "", fmt.Errorf("task_scope test_file %q differs from the pinned test file %q", block.TestFile, testFile)
		}
		return files, testFile, nil
	}
	if block.TestFile != "" && !slices.Contains(block.Files, block.TestFile) {
		return nil, "", fmt.Errorf("task_scope test_file %q is not in its files", block.TestFile)
	}
	return slices.Clone(block.Files), block.TestFile, nil
}

// briefApprovedScope returns the daemon-approved scope-widen files the
// brief's stage block carries (FAE-WF-078). They follow the one declared-path
// rule like every other scope path: the daemon never sends an unsafe one (the
// engine drops a stored one from the job before the next dispatch), and the
// bridge refuses one anyway.
func briefApprovedScope(raw json.RawMessage) ([]string, error) {
	var blocks []struct {
		Kind  string   `json:"kind"`
		Scope []string `json:"approved_scope"`
	}
	if e := json.Unmarshal(raw, &blocks); e != nil {
		return nil, e
	}
	var out []string
	for _, b := range blocks {
		if b.Kind != "stage" {
			continue
		}
		for _, name := range b.Scope {
			if !workflow.DeclaredPathSafe(name) {
				return nil, fmt.Errorf("approved_scope path %q: unsafe proposed path", name)
			}
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// unavailableScopeFile is an approved scope-widen file the bridge could not
// load into a turn, and why.
type unavailableScopeFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// scopeFiles is what loadScopeFiles read for one turn.
type scopeFiles struct {
	files       map[string]string
	original    map[string][]byte
	exists      map[string]bool
	unavailable []unavailableScopeFile
}

// loadScopeFiles reads the turn's files, at most 1 MiB each and
// maxProviderFilesBytes in total. A file the bridge can't load (a symlink on
// its path, a non-regular file, one over 1 MiB, or more than the total
// allows) refuses the turn before the provider runs. An approved scope-widen
// file is the exception (FAE-WF-078): nothing removes approved scope, so
// refusing it would refuse every later turn of the task, intake included.
// Such a file is listed as unavailable with the reason instead; it is never
// read (a symlink is not followed), not sent and not editable in the turn.
// That holds for the turn's RED test file too when a widen brought it in: no
// turn but test-first may edit it, the engine runs the RED test from the
// worktree, not from the bridge, and the RED identity stays as frozen, so a
// test-first turn can only report its RED as it stands. Declared files and
// the RED test file are read first, so other approved files never crowd them
// out of the total.
func loadScopeFiles(root string, names, approved []string, testFile string) (scopeFiles, error) {
	s := scopeFiles{files: map[string]string{}, original: map[string][]byte{}, exists: map[string]bool{}}
	tolerated := func(name string) bool { return slices.Contains(approved, name) }
	first := func(name string) bool { return !tolerated(name) || name == testFile }
	declared := slices.DeleteFunc(slices.Clone(names), func(name string) bool { return !first(name) })
	widened := slices.DeleteFunc(slices.Clone(names), first)
	unavailable := func(name, reason string) {
		if !slices.ContainsFunc(s.unavailable, func(u unavailableScopeFile) bool { return u.Path == name }) {
			s.unavailable = append(s.unavailable, unavailableScopeFile{Path: name, Reason: reason})
		}
	}
	total := 0
	for _, name := range append(declared, widened...) {
		path, e := safePath(root, name)
		if e != nil {
			if tolerated(name) {
				unavailable(name, e.Error())
				continue
			}
			return s, e
		}
		if _, read := s.files[name]; read {
			continue
		}
		if total > maxProviderFilesBytes {
			// Already refused: only measure the rest of the declared files to
			// name the total.
			if st, e := os.Lstat(path); e == nil && !tolerated(name) {
				total += int(st.Size())
			}
			s.files[name] = ""
			continue
		}
		data, found, e := readScopeFile(path)
		if e != nil {
			if tolerated(name) {
				unavailable(name, e.Error())
				continue
			}
			return s, e
		}
		if tolerated(name) && total+len(data) > maxProviderFilesBytes {
			unavailable(name, fmt.Sprintf("its %d bytes would take the turn's files over the %d-byte provider input bound", len(data), maxProviderFilesBytes))
			continue
		}
		total += len(data)
		s.files[name] = string(data)
		s.original[name] = data
		s.exists[name] = found
	}
	if total > maxProviderFilesBytes {
		return s, fmt.Errorf("task files total %d bytes, over the %d-byte provider input bound", total, maxProviderFilesBytes)
	}
	return s, nil
}

// editable is names without the files that could not be loaded.
func (s scopeFiles) editable(names []string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(name string) bool {
		return slices.ContainsFunc(s.unavailable, func(u unavailableScopeFile) bool { return u.Path == name })
	})
}

// withUnavailableScope adds a brief block that lists the approved files the
// turn could not load, so the model knows they exist, why they are missing
// and that it cannot edit them in this turn.
func withUnavailableScope(brief json.RawMessage, unavailable []unavailableScopeFile) (json.RawMessage, error) {
	var blocks []json.RawMessage
	if e := json.Unmarshal(brief, &blocks); e != nil {
		return nil, e
	}
	block, e := json.Marshal(map[string]any{"kind": "unavailable_scope", "files": unavailable, "note": "These approved scope-widen files could not be loaded into this turn, for the reason given. They are not in the files block and cannot be edited in this turn. When the task's RED test_file is among them, the engine still runs the RED test from the worktree; a test-first turn can only report the RED as it stands."})
	if e != nil {
		return nil, e
	}
	return json.Marshal(append(blocks, block))
}

// readScopeFile reads one declared file, reporting whether it exists. A
// missing file reads as empty content.
func readScopeFile(path string) ([]byte, bool, error) {
	if _, e := os.Lstat(path); os.IsNotExist(e) {
		return nil, false, nil
	}
	data, e := regularRead(path, false)
	return data, e == nil, e
}

func computeFileMetadata(path, rel, scope string) (ScopeFileMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return ScopeFileMetadata{}, err
	}
	defer f.Close()
	h := sha256.New()
	r := bufio.NewReaderSize(io.TeeReader(f, h), 64*1024)
	var bytesCount int64
	lines := 0
	for {
		buf, err := r.ReadSlice('\n')
		bytesCount += int64(len(buf))
		if len(buf) > 0 && buf[len(buf)-1] == '\n' {
			lines++
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			if len(buf) > 0 && buf[len(buf)-1] != '\n' {
				lines++
			}
			break
		}
		if err != nil {
			return ScopeFileMetadata{}, err
		}
	}
	return ScopeFileMetadata{
		Path:   rel,
		Bytes:  bytesCount,
		Lines:  lines,
		SHA256: hex.EncodeToString(h.Sum(nil)),
		Scope:  scope,
	}, nil
}

// CaptureTaskScope enforces InlineFileBytes (default 1 MiB) and
// InlineTurnBytes (default 4 MiB) and separates editable write_scope from
// read_only_inputs (FAE-RT-017).
//
// In a tool-less session (spec.Session == ""), any file exceeding
// InlineFileBytes or pushing total inline bytes over InlineTurnBytes is not
// inlined and is reported in UnavailableScope with reason
// "exceeds_inline_bound".
//
// In an agentic session (spec.Session == "agentic"), files exceeding
// InlineFileBytes or InlineTurnBytes have their metadata (path, byte size,
// line count, SHA-256) listed in Metadata without full inline content so the
// session can read line ranges and apply targeted replacement edits via tools.
func CaptureTaskScope(root string, spec TaskScope) (TaskScope, error) {
	out := spec
	if out.InlineFileBytes <= 0 {
		out.InlineFileBytes = DefaultInlineFileBytes
	}
	if out.InlineTurnBytes <= 0 {
		out.InlineTurnBytes = DefaultInlineTurnBytes
	}
	out.WriteScope = map[string]string{}
	out.ReadOnlyInputs = map[string]string{}
	out.Metadata = nil
	out.UnavailableScope = nil

	totalInline := 0
	loadOne := func(name, scopeKind string, target map[string]string) error {
		if !workflow.DeclaredPathSafe(name) {
			return fmt.Errorf("%s path %q: unsafe proposed path", scopeKind, name)
		}
		path, err := safePath(root, name)
		if err != nil {
			return fmt.Errorf("%s path %q: %w", scopeKind, name, err)
		}
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			target[name] = ""
			return nil
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s path %q: input must be a regular file", scopeKind, name)
		}
		sz := st.Size()
		if sz > int64(out.InlineFileBytes) || totalInline+int(sz) > out.InlineTurnBytes {
			if out.Session == "agentic" {
				meta, err := computeFileMetadata(path, name, scopeKind)
				if err != nil {
					return err
				}
				out.Metadata = append(out.Metadata, meta)
				return nil
			}
			out.UnavailableScope = append(out.UnavailableScope, UnavailableScopeEntry{
				Path:   name,
				Bytes:  sz,
				Reason: "exceeds_inline_bound",
			})
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			// Keep binary check consistent if needed, though regular files are allowed.
		}
		totalInline += len(data)
		target[name] = string(data)
		return nil
	}

	seenWrite := map[string]bool{}
	var cleanFiles []string
	for _, f := range spec.Files {
		if f == "" || seenWrite[f] || slices.Contains(spec.ReadOnlyFiles, f) {
			continue
		}
		seenWrite[f] = true
		cleanFiles = append(cleanFiles, f)
		if err := loadOne(f, "write_scope", out.WriteScope); err != nil {
			return out, err
		}
	}
	out.Files = cleanFiles

	seenRO := map[string]bool{}
	var cleanRO []string
	for _, f := range spec.ReadOnlyFiles {
		if f == "" || seenRO[f] {
			continue
		}
		seenRO[f] = true
		cleanRO = append(cleanRO, f)
		if err := loadOne(f, "read_only_inputs", out.ReadOnlyInputs); err != nil {
			return out, err
		}
	}
	out.ReadOnlyFiles = cleanRO
	return out, nil
}

// withBoundedTaskScope appends separated read_only_inputs, scope_metadata, and
// unavailable_scope brief blocks captured under FAE-RT-017.
func withBoundedTaskScope(brief json.RawMessage, captured TaskScope) (json.RawMessage, error) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(brief, &blocks); err != nil {
		return nil, err
	}
	if len(captured.ReadOnlyFiles) > 0 || len(captured.ReadOnlyInputs) > 0 {
		b, err := json.Marshal(map[string]any{
			"kind":             "read_only_inputs",
			"files":            captured.ReadOnlyFiles,
			"read_only_inputs": captured.ReadOnlyInputs,
		})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	if len(captured.Metadata) > 0 {
		b, err := json.Marshal(map[string]any{
			"kind":     "scope_metadata",
			"metadata": captured.Metadata,
		})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	if len(captured.UnavailableScope) > 0 {
		b, err := json.Marshal(map[string]any{
			"kind":  "unavailable_scope",
			"files": captured.UnavailableScope,
			"note":  "These files exceeded the inline brief bounds (InlineFileBytes or InlineTurnBytes) in a tool-less turn and were omitted from the inline brief.",
		})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return json.Marshal(blocks)
}
