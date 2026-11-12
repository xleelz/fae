package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/scratchfs"
	"github.com/xleelz/fae/workflow"
)

// LandPolicy names the landing gate commands of a Go target. Flakes are known,
// listed flaky tests; Frozen are the target's frozen-region checks.
type LandPolicy struct {
	Compile        [][]string      `json:"compile"`
	Suite          []string        `json:"suite"`
	Flakes         []string        `json:"flakes,omitempty"`
	Frozen         [][]string      `json:"frozen,omitempty"`
	DocumentGlobs  []string        `json:"document_globs,omitempty"`
	DocumentChecks []DocumentCheck `json:"document_checks,omitempty"`
	Timeout        time.Duration   `json:"timeout"`
}

// DefaultLandPolicy compiles every package including tests, then runs the
// whole suite with one package at a time (FAE-HOST-015).
func DefaultLandPolicy() LandPolicy {
	return LandPolicy{Compile: [][]string{{"go", "test", "-count=1", "-json", "-run", "^$", "./..."}}, Suite: []string{"go", "test", "-json", "-count=1", "-p", "1", "./..."}, Timeout: 30 * time.Minute}
}

// GoFilterCommand runs the tests the required test filter names across every
// test target of the module. filter is handed to go test -run as written
// (TestFilter): an exact name is quoted and anchored first (ExactTestFilter).
// When filter selects every top-level test, the run is a whole-suite run and
// runs one package at a time (FAE-HOST-015).
func GoFilterCommand(filter string) []string {
	if selectsEveryTest(filter) {
		return []string{"go", "test", "-json", "-count=1", "-run", filter, "-p", "1", "./..."}
	}
	return []string{"go", "test", "-json", "-count=1", "-run", filter, "./..."}
}

// LandRequest is one task landing (FAE-CTL-030) or salvage (FAE-CTL-031).
// Exactly one of Commits (the task's own commits, oldest first) or Source (the
// tree its engine runs recorded as passing, with its Base) is set.
type LandRequest struct {
	ID, Task, Workspace, Target string
	// Expected, when set, is the tip the target must still name (batch chaining).
	Expected     string
	Commits      []string
	Base, Source string
	Files          []string
	Filter         string
	QA             [][]string
	RequiredTests  [][]string
	DocumentGlobs  []string
	DocumentChecks []DocumentCheck
	Policy         LandPolicy
	Scratch      string
	Held, Gated  string
	Environment  []string
	Admit        func(context.Context) (func() error, error)
	// Advance is the durable write-ahead record taken after every gate passed
	// and before the target and gated references are written.
	Advance func(context.Context, LandResult) error
	// RedactOutput, when set, copies a gate command's captured stdout or
	// stderr into the file the landing scratch keeps for it, with every
	// secret replaced (FAE-SEC-003); gate results are parsed from that copy,
	// and must agree with the raw capture's (landGates.tests). Its error stops
	// the landing as infrastructure, keeping the attempt on the held branch.
	// Unset, the output is kept as written.
	RedactOutput func(dst io.Writer, src io.Reader) error
	// TempLeft, when set, is told of the gate commands' TMPDIR
	// (landTempDir) when the landing could not remove it.
	TempLeft func(dir string, err error)
	// CommitIdentity and ContentPolicy enforce FAE-VCS-012 and FAE-SEC-009.
	CommitIdentity *workflow.CommitIdentity
	ContentPolicy  workflow.ContentPolicy
}

// LandResult is the landed sha with every gate's counts, or the held branch
// and the failing gate.
type LandResult struct {
	ID          string       `json:"id"`
	Task        string       `json:"task"`
	Target      string       `json:"target"`
	Outcome     string       `json:"outcome"`
	Start       string       `json:"start,omitempty"`
	Tip         string       `json:"tip,omitempty"`
	Landed      string       `json:"landed,omitempty"`
	Gated       string       `json:"gated,omitempty"`
	Held        string       `json:"held,omitempty"`
	FailingGate string       `json:"failing_gate,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	Gates             []GateResult       `json:"gates,omitempty"`
	DocumentCheckRuns []DocumentCheckRun `json:"document_check_runs,omitempty"`
	Commits           []string           `json:"commits,omitempty"`
	Dropped     []string     `json:"dropped,omitempty"`
	Base        string       `json:"base,omitempty"`
	SourceTree  string       `json:"source_tree,omitempty"`
	Patch       string       `json:"patch,omitempty"`
}

const (
	LandLanded         = "landed"
	LandHeld           = "held"
	LandNothingOwn     = "nothing-own"
	LandInfrastructure = "infrastructure"
	LandNotAttempted   = "not-attempted"
)

func landGit(ctx context.Context, dir string, stdin []byte, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", dir}, args...)...)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	// A task's file names reach git as literal paths, never as pathspecs: a
	// file named ":(exclude)*.go" or "*" names only itself (FAE-SEC-005).
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_LITERAL_PATHSPECS=1")
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if e := cmd.Run(); e != nil {
		if slices.Contains(args, "-z") {
			return out.String(), fmt.Errorf("git %s: %w: %s", args[0], e, strings.TrimSpace(errOut.String()+out.String()))
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %w: %s", args[0], e, strings.TrimSpace(errOut.String()+out.String()))
	}
	if slices.Contains(args, "-z") {
		return out.String(), nil
	}
	return strings.TrimSpace(out.String()), nil
}

func validLandingRef(ref, prefix string) bool {
	return strings.HasPrefix(ref, prefix) && len(ref) > len(prefix) && !strings.ContainsAny(ref, " \t\n~^:?*[\\") && !strings.Contains(ref, "..") && !strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock")
}

// Land performs one landing. The target is written only by compare-and-swap
// after every gate passed on the exact merged tip; on any failure the target
// is untouched and the attempt is kept on the held branch.
func Land(ctx context.Context, r LandRequest) (LandResult, error) {
	res := LandResult{ID: r.ID, Task: r.Task, Target: r.Target}
	git := func(args ...string) (string, error) { return landGit(ctx, r.Workspace, nil, nil, args...) }
	if r.ID == "" || r.Task == "" || !filepath.IsAbs(r.Workspace) || !filepath.IsAbs(r.Scratch) || r.Admit == nil || !validLandingRef(r.Target, "refs/heads/") || strings.HasPrefix(r.Target, "refs/heads/fae/task/") || !validLandingRef(r.Held, "refs/heads/fae/held/") || !validLandingRef(r.Gated, "refs/fae/gated/") || (len(r.Commits) == 0) == (r.Source == "") || r.Source != "" && r.Base == "" {
		return res, errors.New("invalid landing request")
	}
	if r.Policy.Timeout <= 0 {
		r.Policy.Timeout = 30 * time.Minute
	}
	release, e := r.Admit(ctx)
	if e != nil {
		res.Outcome, res.Reason = LandInfrastructure, e.Error()
		return res, nil
	}
	defer release()
	start, e := git("rev-parse", "--verify", r.Target+"^{commit}")
	if e != nil {
		return res, e
	}
	res.Start = start
	if r.Expected != "" && start != r.Expected {
		res.Outcome, res.Reason = LandInfrastructure, "target moved before landing"
		return res, nil
	}
	hold := func(gate, reason, attempt string, gates ...GateResult) (LandResult, error) {
		res.Outcome, res.FailingGate, res.Reason = LandHeld, gate, reason
		res.Gates = append(res.Gates, gates...)
		if attempt == "" {
			attempt = start
		}
		if _, e := git("update-ref", "-m", "fae land held "+r.ID, r.Held, attempt, ""); e != nil {
			return res, e
		}
		res.Held = r.Held
		return res, nil
	}
	var tip, ownBase, ownHead string
	if r.Source != "" {
		ownBase = r.Base
		if res.SourceTree, e = git("rev-parse", "--verify", r.Source+"^{tree}"); e != nil {
			return res, e
		}
		res.Base = r.Base
		restricted, e := restrictTree(ctx, r.Workspace, r.Base, res.SourceTree, r.Files)
		if e != nil {
			return res, e
		}
		baseTree, e := git("rev-parse", r.Base+"^{tree}")
		if e != nil {
			return res, e
		}
		if restricted == baseTree {
			res.Outcome, res.Reason = LandNothingOwn, "salvage source has no change in declared files"
			return res, nil
		}
		message := r.CommitIdentity.FormatMessage(fmt.Sprintf("fae land --salvage %s\n\nSource tree %s on base %s restricted to declared files.", r.Task, res.SourceTree, r.Base)) + "\n"
		if ownHead, e = landGit(ctx, r.Workspace, []byte(message), r.CommitIdentity.GitEnv(), "commit-tree", restricted, "-p", r.Base); e != nil {
			return res, e
		}
		merged, conflicts, e := mergeTrees(ctx, r.Workspace, r.Base, start, ownHead)
		if e != nil {
			return res, e
		}
		if len(conflicts) > 0 {
			return hold("conflict", "conflicting paths: "+strings.Join(conflicts, ","), ownHead)
		}
		if tip, e = landGit(ctx, r.Workspace, []byte(message), r.CommitIdentity.GitEnv(), "commit-tree", merged, "-p", start); e != nil {
			return res, e
		}
		if res.Patch, e = git("diff", "--no-color", "--no-ext-diff", "--no-renames", "--binary", r.Base, ownHead); e != nil {
			return res, e
		}
		// Own-hunk presence compares the whole passing tree, not the declared
		// restriction, so a reviewed file left out of the declaration holds.
		ownHead = res.SourceTree
	} else {
		keep, dropped, base, e := selectOwnCommits(ctx, r.Workspace, start, r.Commits)
		if e != nil {
			res.Outcome, res.Reason = LandInfrastructure, e.Error()
			return res, nil
		}
		ownBase, ownHead, res.Base, res.Dropped = base, r.Commits[len(r.Commits)-1], base, dropped
		if len(keep) == 0 {
			res.Outcome, res.Reason = LandNothingOwn, "every own commit is already upstream"
			return res, nil
		}
		cur := start
		for _, c := range keep {
			parent, e := git("rev-parse", c+"^")
			if e != nil {
				return res, e
			}
			merged, conflicts, e := mergeTrees(ctx, r.Workspace, parent, cur, c)
			if e != nil {
				return res, e
			}
			if len(conflicts) > 0 {
				attempt := cur
				if cur == start {
					attempt = ownHead
				}
				res.Commits = append(res.Commits, c)
				return hold("conflict", "conflicting paths: "+strings.Join(conflicts, ","), attempt)
			}
			current, _ := git("rev-parse", cur+"^{tree}")
			if merged == current {
				res.Dropped = append(res.Dropped, c)
				continue
			}
			if cur, e = replayWithIdentity(ctx, r.Workspace, c, merged, cur, r.CommitIdentity); e != nil {
				return res, e
			}
			res.Commits = append(res.Commits, c)
		}
		if cur == start {
			res.Outcome, res.Reason = LandNothingOwn, "every own commit applied empty (contained)"
			return res, nil
		}
		tip = cur
	}
	res.Tip = tip
	if !r.ContentPolicy.IsEmpty() {
		if e := workflow.ScanGitRange(ctx, r.Workspace, start, tip, r.ContentPolicy); e != nil {
			return hold("content-policy", e.Error(), start)
		}
	}
	tree := filepath.Join(r.Scratch, "tree")
	if e = os.MkdirAll(r.Scratch, 0700); e != nil {
		return res, e
	}
	defer landRemoveTemp(r)
	if _, e = git("worktree", "add", "--detach", tree, tip); e != nil {
		return res, e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = landGit(cleanup, r.Workspace, nil, nil, "worktree", "remove", "--force", tree)
		_, _ = landGit(cleanup, r.Workspace, nil, nil, "worktree", "prune")
	}()
	var unkept error
	g := landGates{ctx: ctx, r: r, dir: tree, tip: tip, start: start, unkept: &unkept}
	docGlobs, docChecks := g.effectiveDocumentPolicy()
	diffFiles, diffErr := g.landedDiffFiles()
	if diffErr != nil {
		return res, diffErr
	}
	var gates []func() GateResult
	if isDocumentOnlyDiff(diffFiles, docGlobs) && len(docChecks) > 0 {
		var docRuns []DocumentCheckRun
		var reqRuns []CommandRun
		var docExecErr error
		docEvaluated := false
		var docCount int
		var docFailures []string
		evalDoc := func() (int, []string, error) {
			if docEvaluated {
				return docCount, docFailures, docExecErr
			}
			docEvaluated = true
			for _, dc := range docChecks {
				run, runErr := g.runDocumentCheck(dc, diffFiles)
				if runErr != nil {
					docExecErr = runErr
					return 0, nil, runErr
				}
				docRuns = append(docRuns, run)
			}
			for _, argv := range r.RequiredTests {
				code, _, runErr := g.command(argv, nil)
				if runErr != nil {
					docExecErr = runErr
					return 0, nil, runErr
				}
				reqRuns = append(reqRuns, CommandRun{Argv: argv, ExitCode: code, Packages: 1})
			}
			res.DocumentCheckRuns = docRuns
			docCount, docFailures = EvaluateDocumentChecks(docRuns, reqRuns)
			return docCount, docFailures, nil
		}
		docGate := func(gateName string) func() GateResult {
			return func() GateResult {
				cnt, fails, runErr := evalDoc()
				if runErr != nil {
					return failed(gateName, runErr)
				}
				return documentGateResult(gateName, cnt, fails)
			}
		}
		qaGateFn := docGate("qa")
		if len(r.QA) > 0 {
			qaGateFn = g.qa
		}
		gates = []func() GateResult{
			func() GateResult { return g.ownHunks(ownBase, ownHead) },
			docGate("format"),
			docGate("compile"),
			docGate("filter"),
			g.frozen,
			qaGateFn,
			docGate("suite"),
		}
	} else {
		gates = []func() GateResult{
			func() GateResult { return g.ownHunks(ownBase, ownHead) },
			g.format, g.compile, g.filter, g.frozen, g.qa, g.suite,
		}
	}
	for _, run := range gates {
		result := run()
		res.Gates = append(res.Gates, result)
		if unkept != nil {
			// The output could not be kept redacted (the secrets could not be
			// read, say): nothing about the change failed, so the landing
			// stops as infrastructure, keeping the attempt.
			held, e := hold("", unkept.Error(), tip)
			held.Outcome = LandInfrastructure
			return held, e
		}
		if !result.Passed {
			return hold(result.Gate, result.Reason, tip)
		}
	}
	landing := LandingGate(res.Gates)
	head, _ := landGit(ctx, tree, nil, nil, "rev-parse", "HEAD")
	current, e := git("rev-parse", "--verify", r.Target+"^{commit}")
	if e != nil {
		return res, e
	}
	if !CanAdvanceTarget(landing.Passed, head == tip, current == start, tip != start) {
		if current != start {
			return hold("target-moved", "target moved during gates; gates must re-run on the new tip", tip)
		}
		return hold(landing.Gate, "landing gates did not pass on the exact tip: "+landing.Reason, tip)
	}
	res.Outcome = "advancing"
	if r.Advance != nil {
		if e = r.Advance(ctx, res); e != nil {
			res.Outcome, res.Reason = LandInfrastructure, "landing record not durable: "+e.Error()
			return res, nil
		}
	}
	if e = fastForward(ctx, r.Workspace, r.Target, start, tip); e != nil {
		return hold("target-write", e.Error(), tip)
	}
	res.Outcome, res.Landed = LandLanded, tip
	old, _ := git("rev-parse", "--verify", "--quiet", r.Gated+"^{commit}")
	if _, e = git("update-ref", "-m", "fae land gated "+r.ID, r.Gated, tip, old); e != nil {
		res.Reason = "landed; gated reference not advanced: " + e.Error()
		return res, nil
	}
	res.Gated = r.Gated
	return res, nil
}

func faeCommitter() []string {
	return []string{"GIT_COMMITTER_NAME=Fae", "GIT_COMMITTER_EMAIL=fae@localhost", "GIT_AUTHOR_NAME=Fae", "GIT_AUTHOR_EMAIL=fae@localhost"}
}

// selectOwnCommits keeps only own commits not already upstream by patch
// identity (FAE-CTL-030 step 2). The commits must form one linear chain.
func selectOwnCommits(ctx context.Context, ws, start string, commits []string) (keep, dropped []string, base string, err error) {
	resolved := make([]string, 0, len(commits))
	for i, c := range commits {
		sha, e := landGit(ctx, ws, nil, nil, "rev-parse", "--verify", c+"^{commit}")
		if e != nil {
			return nil, nil, "", e
		}
		parent, e := landGit(ctx, ws, nil, nil, "rev-parse", "--verify", sha+"^")
		if e != nil {
			return nil, nil, "", errors.New("own commit has no parent")
		}
		if i == 0 {
			base = parent
		} else if parent != resolved[i-1] {
			return nil, nil, "", errors.New("own commits are not one linear chain")
		}
		resolved = append(resolved, sha)
	}
	out, e := landGit(ctx, ws, nil, nil, "cherry", start, resolved[len(resolved)-1], base)
	if e != nil {
		return nil, nil, "", e
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		mark, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		listed[sha] = true
		if mark == "+" {
			keep = append(keep, sha)
		} else {
			dropped = append(dropped, sha)
		}
	}
	for _, sha := range resolved {
		if !listed[sha] {
			dropped = append(dropped, sha)
		}
	}
	return keep, dropped, base, nil
}

// mergeTrees is a worktree-free three-way merge; conflicts name their paths.
func mergeTrees(ctx context.Context, ws, base, ours, theirs string) (string, []string, error) {
	out, e := landGit(ctx, ws, nil, nil, "merge-tree", "--write-tree", "--name-only", "-z", "--merge-base="+base, ours, theirs)
	fields := strings.Split(out, "\x00")
	tree := strings.TrimSpace(fields[0])
	if e == nil {
		return tree, nil, nil
	}
	conflicts := []string{}
	for _, f := range fields[1:] {
		if f == "" {
			break
		}
		conflicts = append(conflicts, f)
	}
	if len(conflicts) == 0 || tree == "" {
		return "", nil, e
	}
	sort.Strings(conflicts)
	return tree, conflicts, nil
}

// replay records commit c's merged tree on parent, keeping its author.
func replay(ctx context.Context, ws, c, tree, parent string) (string, error) {
	return replayWithIdentity(ctx, ws, c, tree, parent, nil)
}

func replayWithIdentity(ctx context.Context, ws, c, tree, parent string, id *workflow.CommitIdentity) (string, error) {
	meta, e := landGit(ctx, ws, nil, nil, "log", "-1", "--format=%an%x00%ae%x00%aI%x00%B", c)
	if e != nil {
		return "", e
	}
	parts := strings.SplitN(meta, "\x00", 4)
	if len(parts) != 4 {
		return "", errors.New("unreadable own commit metadata")
	}
	authorName, authorEmail := parts[0], parts[1]
	committerName, committerEmail := "Fae", "fae@localhost"
	if id != nil {
		if strings.TrimSpace(id.Name) != "" {
			authorName = strings.TrimSpace(id.Name)
			committerName = strings.TrimSpace(id.Name)
		}
		if strings.TrimSpace(id.Email) != "" {
			authorEmail = strings.TrimSpace(id.Email)
			committerEmail = strings.TrimSpace(id.Email)
		}
	}
	env := []string{"GIT_AUTHOR_NAME=" + authorName, "GIT_AUTHOR_EMAIL=" + authorEmail, "GIT_AUTHOR_DATE=" + parts[2], "GIT_COMMITTER_NAME=" + committerName, "GIT_COMMITTER_EMAIL=" + committerEmail}
	body := id.FormatMessage(strings.TrimSpace(parts[3]))
	message := body + "\n\n(cherry picked from commit " + c + ")\n"
	return landGit(ctx, ws, []byte(message), env, "commit-tree", tree, "-p", parent)
}

// restrictTree is base's tree with each declared file taken from source
// (FAE-CTL-031): a declared file absent from source is deleted.
func restrictTree(ctx context.Context, ws, base, source string, files []string) (string, error) {
	if len(files) == 0 {
		return "", errors.New("salvage requires declared files")
	}
	index, e := os.CreateTemp("", "fae-land-index-")
	if e != nil {
		return "", e
	}
	name := index.Name()
	index.Close()
	os.Remove(name)
	defer os.Remove(name)
	defer os.Remove(name + ".lock")
	env := []string{"GIT_INDEX_FILE=" + name}
	if _, e = landGit(ctx, ws, nil, env, "read-tree", base); e != nil {
		return "", e
	}
	for _, f := range files {
		if f == "" || filepath.IsAbs(f) || filepath.Clean(f) != f || strings.HasPrefix(f, "../") || f == ".." {
			return "", fmt.Errorf("invalid declared file %q", f)
		}
		entry, e := landGit(ctx, ws, nil, nil, "ls-tree", source, "--", f)
		if e != nil {
			return "", e
		}
		if entry == "" {
			if _, e = landGit(ctx, ws, nil, env, "update-index", "--force-remove", "--", f); e != nil {
				return "", e
			}
			continue
		}
		meta, _, _ := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" {
			return "", fmt.Errorf("declared path %q is not a file in the source tree", f)
		}
		if _, e = landGit(ctx, ws, nil, env, "update-index", "--add", "--cacheinfo", fields[0]+","+fields[2]+","+f); e != nil {
			return "", e
		}
	}
	return landGit(ctx, ws, nil, env, "write-tree")
}

// fastForward is the only target write: a compare-and-swap fast-forward. A
// target checked out in a worktree is fast-forwarded there only when that
// checkout is clean and still at the starting tip.
func fastForward(ctx context.Context, ws, target, start, tip string) error {
	if _, e := landGit(ctx, ws, nil, nil, "merge-base", "--is-ancestor", start, tip); e != nil {
		return errors.New("landing tip does not descend from the target")
	}
	list, e := landGit(ctx, ws, nil, nil, "worktree", "list", "--porcelain")
	if e != nil {
		return e
	}
	checkout := ""
	for _, block := range strings.Split(list, "\n\n") {
		path := ""
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "worktree "); ok {
				path = v
			}
			if line == "branch "+target {
				checkout = path
			}
		}
	}
	if checkout == "" {
		_, e = landGit(ctx, ws, nil, nil, "update-ref", "-m", "fae land", target, tip, start)
		return e
	}
	status, e := landGit(ctx, checkout, nil, nil, "status", "--porcelain", "--untracked-files=no")
	if e != nil {
		return e
	}
	head, e := landGit(ctx, checkout, nil, nil, "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	if status != "" || head != start {
		return errors.New("target is checked out with local changes or a moved head; landing refused")
	}
	if _, e = landGit(ctx, checkout, nil, nil, "merge", "--ff-only", "--no-stat", "--no-edit", tip); e != nil {
		return e
	}
	if now, e := landGit(ctx, ws, nil, nil, "rev-parse", target); e != nil || now != tip {
		return errors.Join(e, errors.New("target did not fast-forward to the landing tip"))
	}
	return nil
}

type landGates struct {
	ctx             context.Context
	r               LandRequest
	dir, tip, start string
	// unkept is set to the first RedactOutput error, or to a redaction that
	// changed what a go test gate counts (tests), which stops the landing as
	// infrastructure.
	unkept *error
}

// command runs one gate command on the exact tip with output kept on disk.
// A command that changes tracked content or HEAD invalidates the landing.
// The command writes to unnamed files; only their copies through
// RedactOutput are kept, as gate-output-* and gate-stderr-* in the scratch.
// raw, when set, reads the unredacted stdout before it is closed; nothing it
// reads is kept.
func (g landGates) command(argv []string, raw func(io.Reader)) (int, string, error) {
	if len(argv) == 0 {
		return 0, "", errors.New("empty gate command")
	}
	output, e := os.CreateTemp(g.r.Scratch, "gate-output-")
	if e != nil {
		return 0, "", e
	}
	defer output.Close()
	diagnostics, e := os.CreateTemp(g.r.Scratch, "gate-stderr-")
	if e != nil {
		return 0, "", e
	}
	defer diagnostics.Close()
	stdout, e := landCapture(g.r.Scratch)
	if e != nil {
		return 0, "", e
	}
	defer stdout.Close()
	stderr, e := landCapture(g.r.Scratch)
	if e != nil {
		return 0, "", e
	}
	defer stderr.Close()
	tmp := landTempDir(g.r.Scratch)
	if e = os.MkdirAll(tmp, 0700); e != nil {
		return 0, "", e
	}
	ctx, cancel := context.WithTimeout(g.ctx, g.r.Policy.Timeout)
	defer cancel()
	env := append(append([]string{}, g.r.Environment...), "TMPDIR="+tmp, "GOFLAGS=", "GOWORK=off")
	binary := lookPath(argv[0], env)
	if binary == "" {
		return 0, "", fmt.Errorf("gate command %s not found on the sanitized PATH", argv[0])
	}
	// Machine-readable results come from stdout only; stderr is kept apart.
	cmd := exec.CommandContext(ctx, binary, argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr, cmd.Env = g.dir, stdout, stderr, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	code := 0
	e = cmd.Run()
	kept := errors.Join(g.keep(output, stdout), g.keep(diagnostics, stderr))
	if e != nil {
		var exit *exec.ExitError
		if !errors.As(e, &exit) || ctx.Err() != nil {
			return 0, output.Name(), errors.Join(e, ctx.Err(), kept)
		}
		code = exit.ExitCode()
	}
	if kept != nil {
		return code, output.Name(), kept
	}
	if raw != nil {
		if _, e = stdout.Seek(0, io.SeekStart); e != nil {
			return code, output.Name(), e
		}
		raw(stdout)
	}
	head, e := landGit(g.ctx, g.dir, nil, nil, "rev-parse", "HEAD")
	if e != nil {
		return code, output.Name(), e
	}
	status, e := landGit(g.ctx, g.dir, nil, nil, "status", "--porcelain", "--untracked-files=no")
	if e != nil {
		return code, output.Name(), e
	}
	if head != g.tip || status != "" {
		return code, output.Name(), errors.New("gate command changed the landing tree")
	}
	return code, output.Name(), nil
}

// landTempDir is the gate commands' TMPDIR in a landing's scratch: test
// scratch, unlike the output the landing keeps beside it.
func landTempDir(scratch string) string { return filepath.Join(scratch, "tmp") }

// landRemoveTemp removes the gate commands' TMPDIR when the landing returns,
// however it stops, with any read-only tree a test left in it
// (FAE-HOST-020; scratchfs.RemoveAll), and reports one it cannot remove to
// TempLeft. The kept gate output stays in the scratch.
func landRemoveTemp(r LandRequest) {
	tmp := landTempDir(r.Scratch)
	if e := scratchfs.RemoveAll(tmp); e != nil && r.TempLeft != nil {
		r.TempLeft(tmp, e)
	}
}

// landCapture is a file in dir a gate command writes to, removed from dir as
// soon as it is created: its unredacted content has no name on disk, and it
// is gone once the last process holding it exits, however the landing stops.
func landCapture(dir string) (*os.File, error) {
	f, e := os.CreateTemp(dir, "gate-capture-")
	if e != nil {
		return nil, e
	}
	if e = os.Remove(f.Name()); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}

// keep copies a gate command's captured output into the file kept for it,
// through RedactOutput when that is set.
func (g landGates) keep(kept, captured *os.File) error {
	if _, e := captured.Seek(0, io.SeekStart); e != nil {
		return e
	}
	if g.r.RedactOutput == nil {
		_, e := io.Copy(kept, captured)
		return e
	}
	e := g.r.RedactOutput(kept, captured)
	if e != nil && *g.unkept == nil {
		*g.unkept = e
	}
	return e
}

func lookPath(name string, env []string) string {
	if strings.Contains(name, "/") {
		return name
	}
	for _, v := range env {
		if path, ok := strings.CutPrefix(v, "PATH="); ok {
			for _, dir := range filepath.SplitList(path) {
				candidate := filepath.Join(dir, name)
				if info, e := os.Stat(candidate); e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
					return candidate
				}
			}
		}
	}
	return ""
}

// tests runs a go test gate command and parses its results from the copy
// the landing keeps. When that copy is redacted, the raw capture is parsed
// too, in memory, and only its counts are compared: a redaction that merged
// two tests or packages into one name, hid an event or renamed a failure into
// or out of the listed flakes changes a count, and fails the gate
// (FAE-CTL-030), so a redaction cannot turn a failure into a pass. A rename
// that changes no count only changes the name shown. The reason names counts
// only. Nothing about the change failed when only the redaction changed the
// results, so such a landing stops as infrastructure (unkept), as when the
// secrets cannot be read.
func (g landGates) tests(argv []string, filter string) (TestCounts, error) {
	var raw TestCounts
	var rawErr error
	var inspect func(io.Reader)
	if g.r.RedactOutput != nil {
		inspect = func(r io.Reader) { raw, rawErr = GoTestCountsReader(r, filter) }
	}
	code, path, e := g.command(argv, inspect)
	if e != nil {
		return TestCounts{}, e
	}
	f, e := os.Open(path)
	if e != nil {
		return TestCounts{}, e
	}
	defer f.Close()
	c, e := GoTestCountsReader(f, filter)
	c.ExitCode = code
	if inspect == nil || e != nil && rawErr != nil {
		return c, e
	}
	switch {
	case e != nil:
		e = fmt.Errorf("redaction changed the go test results: the redacted copy does not parse (%w), and the unredacted output does", e)
	case rawErr != nil:
		// The raw error can name a test, and so carry a secret.
		e = errors.New("redaction changed the go test results: the unredacted output does not parse as a complete stream, and the redacted copy does")
	default:
		if got, want := landTestSummary(c, g.r.Policy.Flakes), landTestSummary(raw, g.r.Policy.Flakes); got != want {
			e = fmt.Errorf("redaction changed the go test results (%s unredacted, %s redacted): a gate is not judged on output whose redaction changed what it counts", want, got)
		}
	}
	if e != nil && *g.unkept == nil {
		*g.unkept = e
	}
	return c, e
}

// landTestSummary is every count of c, and how many of its failures are
// listed flakes, without a name.
func landTestSummary(c TestCounts, flakes []string) string {
	listed := 0
	for _, key := range c.Failed {
		if listedFlake(key, flakes) {
			listed++
		}
	}
	return fmt.Sprintf("%d executed, %d matched, %d matched passing, %d packages, %d failed of which %d listed flakes, %d build failures", c.Executed, c.Matched, c.MatchedPassed, c.Packages, len(c.Failed), listed, len(c.BuildFailed))
}

func failed(gate string, e error) GateResult {
	return GateResult{Gate: gate, Applicable: true, Reason: e.Error(), Failures: []string{e.Error()}}
}

// ownHunks reverse-applies each hunk of the task's own patch to the landed tip
// and requires every reviewed file in the landed diff.
func (g landGates) ownHunks(base, head string) GateResult {
	hunks, reviewed, e := OwnHunkPresence(g.ctx, g.r.Workspace, g.dir, base, head)
	if e != nil {
		return failed("own-hunk", e)
	}
	landed, e := landGit(g.ctx, g.r.Workspace, nil, nil, "diff", "--name-only", "-z", "--no-renames", g.start, g.tip)
	if e != nil {
		return failed("own-hunk", e)
	}
	return OwnHunkGate(hunks, reviewed, strings.Split(strings.Trim(landed, "\x00"), "\x00"))
}

// format judges every Go file the landing adds, modifies or changes the type
// of (a symlink or submodule replaced by a file), with every rename and copy
// shown as the delete and add it is (--no-renames). Only a regular file at
// the tip is examined, and its base is read only when the base is a regular
// file too: a symlink's target, in either tree, is no Go source. The names
// are git's own, and are filtered here rather than by a "*.go" pathspec,
// which a literal pathspec (landGit) would take as one file's name.
func (g landGates) format() GateResult {
	raw, e := landGit(g.ctx, g.r.Workspace, nil, nil, "diff", "--raw", "-z", "--no-renames", "--diff-filter=AMT", g.start, g.tip)
	if e != nil {
		return failed("format", e)
	}
	items := []FormatItem{}
	for _, change := range landRawChanges(raw) {
		if !strings.HasSuffix(change.path, ".go") || !landRegularFileMode(change.tipMode) {
			continue
		}
		diff, e := landGit(g.ctx, g.r.Workspace, nil, nil, "diff", "--no-color", "--no-ext-diff", "--no-renames", "-U0", g.start, g.tip, "--", change.path)
		if e != nil {
			return failed("format", e)
		}
		// The tip tree is checked out at dir, so a regular file there is
		// the file itself, and is read whole (landGit trims its output).
		tip, e := os.ReadFile(filepath.Join(g.dir, change.path))
		if e != nil {
			return failed("format", e)
		}
		var base []byte
		if landRegularFileMode(change.baseMode) {
			old, e := landGit(g.ctx, g.r.Workspace, nil, nil, "cat-file", "-p", g.start+":"+change.path)
			if e != nil {
				return failed("format", e)
			}
			base = []byte(old + "\n")
		}
		items = append(items, GoFormatItems(change.path, base, tip, ChangedLines(diff))...)
	}
	return FormatGate(items)
}

// landRawChange is one entry of git diff --raw -z: the modes of the path in
// the two trees (000000 where it is absent) and its status letter.
type landRawChange struct {
	baseMode, tipMode, status, path string
}

// landRawChanges parses git diff --raw -z --no-renames output: for each
// change, ":<base mode> <tip mode> <base sha> <tip sha> <status>", a NUL, the
// path and a NUL. An entry that does not have that shape is left out.
func landRawChanges(raw string) []landRawChange {
	var changes []landRawChange
	fields := strings.Split(strings.Trim(raw, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 || !strings.HasPrefix(fields[i], ":") || fields[i+1] == "" {
			continue
		}
		changes = append(changes, landRawChange{baseMode: meta[0], tipMode: meta[1], status: meta[4], path: fields[i+1]})
	}
	return changes
}

// landRegularFileMode reports whether a git tree entry mode is a regular
// file's (100644 or 100755), rather than a symlink's, a submodule's or absent.
func landRegularFileMode(mode string) bool { return mode == "100644" || mode == "100755" }

func (g landGates) compile() GateResult {
	runs := []CommandRun{}
	for _, argv := range g.r.Policy.Compile {
		c, e := g.tests(argv, "")
		if e != nil {
			return failed("compile", e)
		}
		run := CommandRun{Argv: argv, ExitCode: c.ExitCode, Packages: c.Packages - len(c.BuildFailed)}
		runs = append(runs, run)
	}
	return CompileGate(runs)
}

func (g landGates) filter() GateResult {
	if g.r.Filter == "" {
		return FilterGate(TestCounts{}, "")
	}
	c, e := g.tests(GoFilterCommand(g.r.Filter), g.r.Filter)
	if e != nil {
		return failed("filter", e)
	}
	return FilterGate(c, g.r.Filter)
}

func (g landGates) frozen() GateResult {
	runs := []CommandRun{}
	for _, argv := range g.r.Policy.Frozen {
		code, _, e := g.command(argv, nil)
		if e != nil {
			return failed("frozen", e)
		}
		runs = append(runs, CommandRun{Argv: argv, ExitCode: code})
	}
	return FrozenGate(runs)
}

func (g landGates) qa() GateResult {
	checks := []QACheck{}
	for _, argv := range g.r.QA {
		c, e := g.tests(argv, "")
		if e != nil {
			return failed("qa", e)
		}
		checks = append(checks, QACheck{Argv: argv, Counts: c})
	}
	return QAGate(checks)
}

// suite runs the full suite and, when only listed flakes failed it, runs it
// again without them to explain its exit (SuiteGate).
func (g landGates) suite() GateResult {
	c, e := g.tests(g.r.Policy.Suite, "")
	if e != nil {
		return failed("suite", e)
	}
	var clean *TestCounts
	if skip := SuiteFlakeSkip(c, g.r.Policy.Flakes); skip != "" {
		again, e := g.tests(append(slices.Clone(g.r.Policy.Suite), "-skip", skip), "")
		if e != nil {
			return failed("suite", e)
		}
		clean = &again
	}
	return SuiteGate(c, g.r.Policy.Flakes, clean)
}

// OwnHunkPresence splits base..head into single one-line-context hunks and
// reverse-applies each to the checkout at dir: a hunk is present only if the landed files still
// contain its post-image in context. reviewed lists every file base..head touches.
func OwnHunkPresence(ctx context.Context, ws, dir, base, head string) ([]HunkPresence, []string, error) {
	names, e := landGit(ctx, ws, nil, nil, "diff", "--name-only", "-z", "--no-renames", base, head)
	if e != nil {
		return nil, nil, e
	}
	reviewed := []string{}
	for _, n := range strings.Split(strings.Trim(names, "\x00"), "\x00") {
		if n != "" {
			reviewed = append(reviewed, n)
		}
	}
	patch, e := landGit(ctx, ws, nil, nil, "-c", "core.quotePath=true", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--full-index", "--unified=1", base, head)
	if e != nil {
		return nil, nil, e
	}
	hunks := []HunkPresence{}
	files := splitPatch(patch + "\n")
	for i := 0; i < len(files); i++ {
		file := files[i]
		if i+1 < len(files) && files[i+1].name == file.name {
			// A type change (a symlink replaced by a file, say) is two
			// sections for one path, the old entry's deletion and the new
			// one's addition, which only reverse-apply together: neither
			// half alone fits the landed tree. The change is checked as one
			// unit, each of its hunks present only if the whole is.
			other := files[i+1]
			mini := file.header + strings.Join(file.hunks, "") + other.header + strings.Join(other.hunks, "")
			_, e := landGit(ctx, dir, []byte(mini), nil, "apply", "--check", "--reverse", "--whitespace=nowarn", "-")
			for _, h := range append(append([]string{}, file.hunks...), other.hunks...) {
				label, _, _ := strings.Cut(h, "\n")
				hunks = append(hunks, HunkPresence{File: file.name, Header: label, Present: e == nil})
			}
			i++
			continue
		}
		for _, h := range file.hunks {
			mini := file.header + h
			_, e := landGit(ctx, dir, []byte(mini), nil, "apply", "--check", "--reverse", "--whitespace=nowarn", "-")
			label, _, _ := strings.Cut(h, "\n")
			hunks = append(hunks, HunkPresence{File: file.name, Header: label, Present: e == nil})
		}
	}
	return hunks, reviewed, nil
}

type patchFile struct {
	name, header string
	hunks        []string
}

func splitPatch(patch string) []patchFile {
	files := []patchFile{}
	var cur *patchFile
	var hunk strings.Builder
	inHunk := false
	flush := func() {
		if cur != nil && inHunk {
			cur.hunks = append(cur.hunks, hunk.String())
		}
		hunk.Reset()
		inHunk = false
	}
	lines := strings.SplitAfter(patch, "\n")
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			if cur != nil {
				files = append(files, *cur)
			}
			name := strings.TrimSuffix(line, "\n")
			if i := strings.LastIndex(name, " b/"); i >= 0 {
				name = name[i+3:]
			}
			cur = &patchFile{name: name, header: line}
		case cur == nil:
		case strings.HasPrefix(line, "@@ "):
			flush()
			inHunk = true
			hunk.WriteString(line)
		case inHunk:
			hunk.WriteString(line)
		default:
			cur.header += line
		}
	}
	flush()
	if cur != nil {
		files = append(files, *cur)
	}
	// A file section with no textual hunk (binary, mode-only, empty file) is
	// checked as one unit.
	for i := range files {
		if len(files[i].hunks) == 0 {
			files[i].hunks = []string{""}
		}
	}
	return files
}

// LandBatch lands tasks in the given (dependency) order, each on the tip the
// previous landing produced, stopping at the first that does not land
// (FAE-CTL-032). Later tasks are reported as not attempted.
func LandBatch(ctx context.Context, requests []LandRequest, done func(LandResult)) ([]LandResult, error) {
	results := []LandResult{}
	expected := ""
	for i, r := range requests {
		r.Expected = expected
		res, e := Land(ctx, r)
		if e != nil && res.Outcome == "" {
			res.Outcome, res.Reason = LandInfrastructure, e.Error()
		}
		if done != nil {
			done(res)
		}
		results = append(results, res)
		if e != nil || res.Outcome != LandLanded {
			for _, rest := range requests[i+1:] {
				skipped := LandResult{ID: rest.ID, Task: rest.Task, Target: rest.Target, Outcome: LandNotAttempted, Reason: "batch stopped at " + r.Task}
				if done != nil {
					done(skipped)
				}
				results = append(results, skipped)
			}
			return results, e
		}
		expected = res.Landed
	}
	return results, nil
}

// DependencyOrder sorts a batch so each blocking prerequisite (edge[0]) in the
// batch lands before its dependent (edge[1]); ties keep the requested order.
func DependencyOrder(tasks []string, edges [][2]string) ([]string, error) {
	index := map[string]int{}
	for i, t := range tasks {
		if _, dup := index[t]; dup || t == "" {
			return nil, fmt.Errorf("invalid or duplicate batch task %q", t)
		}
		index[t] = i
	}
	pending := map[string]int{}
	for _, e := range edges {
		if _, from := index[e[0]]; from {
			if _, to := index[e[1]]; to {
				pending[e[1]]++
			}
		}
	}
	order, used := []string{}, map[string]bool{}
	for len(order) < len(tasks) {
		next := ""
		for _, t := range tasks {
			if !used[t] && pending[t] == 0 {
				next = t
				break
			}
		}
		if next == "" {
			return nil, errors.New("batch dependency cycle")
		}
		used[next] = true
		order = append(order, next)
		for _, e := range edges {
			if e[0] == next {
				if _, to := index[e[1]]; to {
					pending[e[1]]--
				}
			}
		}
	}
	return order, nil
}
