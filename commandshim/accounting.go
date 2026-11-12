package commandshim

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// NestedCall is one nested tool call an accounting wrapper recorded: its argv
// (the tool name, then its arguments), working directory and time. Argv and
// Dir are bounded summaries; Truncated marks one that was shortened.
type NestedCall struct {
	Argv      []string  `json:"argv"`
	Dir       string    `json:"dir"`
	At        time.Time `json:"at"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Bounds of one NestedCall summary, and of the record bytes read for it.
const (
	nestedCallArgs        = 16
	nestedCallArgBytes    = 256
	nestedCallDirBytes    = 512
	nestedCallRecordBytes = 64 << 10
)

// InstallAccounting is the opt-in alternative to InstallDenials for an
// engine run's nested calls (FAE-HOST-014: a nested build or test passes
// through the same shim and is accounted under the parent's lease, or is
// refused). It installs wrappers for the same tools in a private directory
// under parent, with the run's policy beside them: s names the fae broker
// (s.Broker), the run's worktree and on-disk TMPDIR (s.Scratch) and whether
// its task is Local; Accounted is set here, and the real tools are resolved
// now from searchPath, never from the wrapper directory. Each wrapper records
// its call (working directory, tool name and arguments, NUL-separated) in a
// file of its own under the returned records directory, then execs
// `fae heavy-op run --shim-policy`, which applies PrepareAccounted and execs
// the real tool in its place (RunShim). The call is therefore clamped and
// policy-checked like a top-level engine command, takes no lease of its own,
// and stays in the brokered command's process group, task cap and lease
// window; its exit code is the tool's, or the policy's refusal (77, 78 for a
// Local push or remote change). A tool missing from searchPath is recorded and
// then fails with 127 and a message; a call whose record cannot be written is
// refused with 77, so no call runs unrecorded. Records and policy are
// evidence and guidance, not containment: the brokered command runs as the
// same user and can write to them, and PATH wrappers are not a sandbox.
func InstallAccounting(parent string, s RoleShims, searchPath string) (shims, records string, err error) {
	if !filepath.IsAbs(s.Broker) || !filepath.IsAbs(s.Worktree) || !filepath.IsAbs(s.Scratch) {
		return "", "", errors.New("accounted nested commands require an absolute fae broker, worktree and scratch directory")
	}
	shims = filepath.Join(parent, "command-shims")
	records = filepath.Join(parent, "nested-calls")
	if err = os.Mkdir(records, 0700); err != nil {
		return "", "", err
	}
	if err = os.Mkdir(shims, 0700); err != nil {
		return "", "", err
	}
	s.Accounted, s.Ledger, s.Tools = true, "", map[string]string{}
	for _, name := range ShimNames {
		if p := resolveShimTool(name, shims, searchPath); p != "" {
			s.Tools[name] = p
		}
	}
	if s.RAMRoots == nil {
		s.RAMRoots = DefaultRAMRoots
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return "", "", err
	}
	policy := filepath.Join(shims, ".policy.json")
	if err = os.WriteFile(policy, raw, 0400); err != nil {
		return "", "", err
	}
	for _, name := range ShimNames {
		script := accountingScript(name, records, s.Tools[name] != "", s.Broker, parent, policy)
		if err = os.WriteFile(filepath.Join(shims, name), []byte(script), 0500); err != nil {
			return "", "", err
		}
	}
	return shims, records, nil
}

// resolveShimTool is the first executable regular file name on searchPath,
// skipping relative entries and the wrapper directory itself, or "".
func resolveShimTool(name, shims, searchPath string) string {
	for _, d := range filepath.SplitList(searchPath) {
		if !filepath.IsAbs(d) || filepath.Clean(d) == shims {
			continue
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
			return p
		}
	}
	return ""
}

// accountingScript is one accounting wrapper. Every value the broker supplies
// is single-quoted and every expansion double-quoted; the record file name is
// the wrapper's PID and a sequence number, created exclusively (noclobber) in
// the broker's records directory. Only shell builtins run before the exec of
// the broker, which gets --home so it never needs the caller's $HOME.
func accountingScript(name, records string, found bool, broker, home, policy string) string {
	refused := shellQuote("fae: nested " + name + " refused (exit 77): its accounting record could not be written")
	var s strings.Builder
	s.WriteString("#!/bin/sh\n")
	s.WriteString("# Fae accounting wrapper: records this call, then runs it through the nested-command policy under the brokered command's lease (FAE-HOST-014).\n")
	s.WriteString("r=" + shellQuote(records) + "\n")
	s.WriteString("n=0\n")
	s.WriteString("until ( set -C; : >\"$r/$$.$n\" ) 2>/dev/null; do\n")
	s.WriteString("\tif [ ! -e \"$r/$$.$n\" ] || [ \"$n\" -ge 64 ]; then printf '%s\\n' " + refused + " >&2; exit 77; fi\n")
	s.WriteString("\tn=$((n + 1))\n")
	s.WriteString("done\n")
	s.WriteString("printf '%s\\0' \"$PWD\" " + shellQuote(name) + " \"$@\" >\"$r/$$.$n\" || { printf '%s\\n' " + refused + " >&2; exit 77; }\n")
	if !found {
		s.WriteString("printf '%s\\n' " + shellQuote("fae: nested "+name+" not found on the base PATH (exit 127); install it there or remove the call") + " >&2\n")
		s.WriteString("exit 127\n")
		return s.String()
	}
	s.WriteString("exec " + shellQuote(broker) + " --home " + shellQuote(home) + " heavy-op run --shim-policy " + shellQuote(policy) + " -- " + shellQuote(name) + " \"$@\"\n")
	return s.String()
}

// ReadAccounted counts the calls recorded under records and returns the first
// limit of them in call order (record time, then wrapper PID and sequence).
// Record times have the file system's timestamp granularity. Entries that are
// not wrapper records are ignored.
func ReadAccounted(records string, limit int) (int, []NestedCall, error) {
	type entry struct {
		name     string
		at       time.Time
		pid, seq int
	}
	before := func(a, b entry) bool {
		if !a.at.Equal(b.at) {
			return a.at.Before(b.at)
		}
		if a.pid != b.pid {
			return a.pid < b.pid
		}
		return a.seq < b.seq
	}
	dir, err := os.Open(records)
	if err != nil {
		return 0, nil, err
	}
	defer dir.Close()
	count := 0
	var first []entry
	for {
		batch, readErr := dir.ReadDir(1024)
		for _, d := range batch {
			pidText, seqText, ok := strings.Cut(d.Name(), ".")
			pid, pidErr := strconv.Atoi(pidText)
			seq, seqErr := strconv.Atoi(seqText)
			if !ok || pidErr != nil || seqErr != nil || pid < 0 || seq < 0 || !d.Type().IsRegular() {
				continue
			}
			info, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return 0, nil, err
			}
			count++
			e := entry{d.Name(), info.ModTime().UTC(), pid, seq}
			at := sort.Search(len(first), func(i int) bool { return before(e, first[i]) })
			if at < limit {
				first = append(first[:at], append([]entry{e}, first[at:]...)...)
				if len(first) > limit {
					first = first[:limit]
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, nil, readErr
		}
	}
	calls := make([]NestedCall, 0, len(first))
	for _, e := range first {
		call, err := readNestedCall(filepath.Join(records, e.name))
		if err != nil {
			return 0, nil, err
		}
		call.At = e.at
		calls = append(calls, call)
	}
	return count, calls, nil
}

// readNestedCall parses one record, bounding the summary.
func readNestedCall(path string) (NestedCall, error) {
	var c NestedCall
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, nestedCallRecordBytes+1))
	if err != nil {
		return c, err
	}
	if len(raw) > nestedCallRecordBytes {
		raw, c.Truncated = raw[:nestedCallRecordBytes], true
	}
	text := string(raw)
	if text == "" {
		return c, nil // the wrapper stopped between creating and writing it
	}
	text = strings.TrimSuffix(text, "\x00")
	fields := strings.Split(text, "\x00")
	c.Dir = boundNestedField(fields[0], nestedCallDirBytes, &c.Truncated)
	args := fields[1:]
	if len(args) > nestedCallArgs {
		args, c.Truncated = args[:nestedCallArgs], true
	}
	c.Argv = make([]string, len(args))
	for i, a := range args {
		c.Argv[i] = boundNestedField(a, nestedCallArgBytes, &c.Truncated)
	}
	return c, nil
}

// boundNestedField cuts s to at most n bytes on a rune boundary.
func boundNestedField(s string, n int, truncated *bool) string {
	if len(s) <= n {
		return s
	}
	*truncated = true
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
