package controller

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xleelz/fae/workflow"
)

// DocumentCheck configures a structured check command for document-only
// landings (FAE-CTL-052, FAE-WF-095).
type DocumentCheck struct {
	ID         string   `json:"id"`
	Command    []string `json:"command"`
	Format     string   `json:"format"`           // "json" | "lines"
	Output     string   `json:"output,omitempty"` // "stdout" or relative file path
	CountField string   `json:"count_field"`
	Writes     []string `json:"writes,omitempty"`
}

// DocumentCheckRun is the execution receipt of one DocumentCheck on the
// landing tip (FAE-CTL-052).
type DocumentCheckRun struct {
	ID         string         `json:"id"`
	Command    []string       `json:"command"`
	Format     string         `json:"format,omitempty"`
	CountField string         `json:"count_field"`
	ExitCode   int            `json:"exit_code"`
	Count      int            `json:"count"`
	Payload    map[string]any `json:"payload,omitempty"`
	ParseError string         `json:"parse_error,omitempty"`
}

// LandingEvaluationInput describes the inputs to EvaluateLanding for testing or
// evaluating code vs. document-only landing gates (FAE-CTL-052).
type LandingEvaluationInput struct {
	DiffFiles      []string
	DocumentGlobs  []string
	DocumentChecks []DocumentCheckRun
	RequiredTests  []CommandRun
	FormatItems    []FormatItem
	CompileRuns    []CommandRun
	FilterCounts   TestCounts
	Filter         string
	FrozenRuns     []CommandRun
	QAChecks       []QACheck
	SuiteCounts    TestCounts
	Flakes         []string
	CleanSuite     *TestCounts
	Hunks          []HunkPresence
	ReviewedFiles  []string
	LandedFiles    []string
}

func makeGateResult(gate string, passed, applicable bool, count int, failures []string, reason string) GateResult {
	return GateResult{
		Gate:       gate,
		Passed:     passed,
		Applicable: applicable,
		Count:      count,
		Failures:   failures,
		Reason:     reason,
	}
}

// EvaluateDocumentChecks validates that every DocumentCheckRun and RequiredTest
// succeeded and reported a positive count (count_field >= 1) (FAE-CTL-052).
func EvaluateDocumentChecks(checks []DocumentCheckRun, requiredTests []CommandRun) (int, []string) {
	if len(checks) == 0 {
		return 0, []string{"no document_checks declared"}
	}
	total := 0
	var failures []string
	for _, dc := range checks {
		name := dc.ID
		if name == "" {
			name = strings.Join(dc.Command, " ")
		}
		if dc.ParseError != "" {
			failures = append(failures, fmt.Sprintf("document check %s invalid output: %s", name, dc.ParseError))
			continue
		}
		if dc.ExitCode != 0 {
			failures = append(failures, fmt.Sprintf("document check %s exited %d", name, dc.ExitCode))
		}
		if dc.CountField == "" {
			failures = append(failures, fmt.Sprintf("document check %s missing count_field", name))
		} else if dc.Count < 1 {
			failures = append(failures, fmt.Sprintf("document check %s reported %s=%d (must be >= 1)", name, dc.CountField, dc.Count))
		} else {
			total += dc.Count
		}
	}
	for _, rt := range requiredTests {
		name := strings.Join(rt.Argv, " ")
		if rt.ExitCode != 0 {
			failures = append(failures, fmt.Sprintf("required test %s exited %d", name, rt.ExitCode))
		} else {
			total++
		}
	}
	return total, failures
}

func documentGateResult(gate string, count int, failures []string) GateResult {
	passed := len(failures) == 0 && count >= 1
	reason := fmt.Sprintf("satisfied by document checks (%d items)", count)
	if !passed {
		if len(failures) > 0 {
			reason = strings.Join(failures, "; ")
		} else {
			reason = "document checks examined zero items"
		}
	}
	return makeGateResult(gate, passed, true, count, failures, reason)
}

func (g landGates) effectiveDocumentPolicy() ([]string, []DocumentCheck) {
	globs := g.r.DocumentGlobs
	if len(globs) == 0 {
		globs = g.r.Policy.DocumentGlobs
	}
	checks := g.r.DocumentChecks
	if len(checks) == 0 {
		checks = g.r.Policy.DocumentChecks
	}
	return globs, checks
}

func (g landGates) landedDiffFiles() ([]string, error) {
	landed, e := landGit(g.ctx, g.r.Workspace, nil, nil, "diff", "--name-only", "-z", "--no-renames", g.start, g.tip)
	if e != nil {
		return nil, e
	}
	var out []string
	for _, f := range strings.Split(strings.Trim(landed, "\x00"), "\x00") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

func (g landGates) runDocumentCheck(dc DocumentCheck, diffFiles []string) (DocumentCheckRun, error) {
	run := DocumentCheckRun{
		ID:         dc.ID,
		Command:    slices.Clone(dc.Command),
		Format:     dc.Format,
		CountField: dc.CountField,
	}
	spec := workflow.CheckSpec{
		Format:     dc.Format,
		Output:     dc.Output,
		CountField: dc.CountField,
		Writes:     slices.Clone(dc.Writes),
	}
	if err := workflow.ValidateCheckSpec(spec); err != nil {
		run.ParseError = err.Error()
		return run, nil
	}
	declared := append(slices.Clone(g.r.Files), diffFiles...)
	if err := workflow.CheckWritesDisjoint(dc.Writes, declared); err != nil {
		run.ParseError = err.Error()
		return run, nil
	}
	var fileRaw []byte
	var fileErr error
	var rawBuf []byte
	capture := func(r io.Reader) {
		rawBuf, _ = io.ReadAll(r)
		if dc.Output != "" && dc.Output != "stdout" {
			fileRaw, fileErr = os.ReadFile(filepath.Join(g.dir, dc.Output))
		}
		for _, w := range dc.Writes {
			// Restore or clean any check-written files before git status checks.
			cleanCheckWriteGlob(g, w)
		}
	}
	code, path, err := g.commandAllowingWrites(dc.Command, dc.Writes, capture)
	if err != nil {
		return run, err
	}
	run.ExitCode = code
	var parseBytes []byte
	if dc.Output != "" && dc.Output != "stdout" {
		if fileErr != nil {
			run.ParseError = fileErr.Error()
			return run, nil
		}
		parseBytes = fileRaw
	} else {
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			parseBytes = rawBuf
		} else {
			parseBytes = b
		}
	}
	payload, count, parseErr := workflow.ParseCheckOutput(spec, string(parseBytes), code, false, false, "")
	if parseErr != nil {
		run.ParseError = parseErr.Error()
		return run, nil
	}
	run.Payload = payload
	run.Count = int(count)
	return run, nil
}

func cleanCheckWriteGlob(g landGates, pattern string) {
	status, err := landGit(g.ctx, g.dir, nil, nil, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return
	}
	for _, entry := range strings.Split(status, "\x00") {
		if len(entry) < 4 {
			continue
		}
		rel := entry[3:]
		if !workflow.MatchGlob(pattern, rel) {
			continue
		}
		if strings.HasPrefix(entry, "?? ") {
			_ = os.RemoveAll(filepath.Join(g.dir, rel))
		} else {
			_, _ = landGit(g.ctx, g.dir, nil, nil, "checkout", "--", rel)
		}
	}
}

func (g landGates) commandAllowingWrites(argv []string, writes []string, beforeStatus func(io.Reader)) (int, string, error) {
	if len(writes) == 0 {
		return g.command(argv, beforeStatus)
	}
	return g.command(argv, beforeStatus)
}
