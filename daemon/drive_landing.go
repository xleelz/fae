package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// Landing test shape modes consumed during landing verification.
const (
	LandingTestShapeRun    = "run"
	LandingTestShapePkg    = "pkg"
	LandingTestShapeFile   = "file"
	LandingTestShapeCustom = "custom"
)

// LandingTestShapes lists the valid landing test_shape modes.
var LandingTestShapes = []string{
	LandingTestShapeRun,
	LandingTestShapePkg,
	LandingTestShapeFile,
	LandingTestShapeCustom,
}

// unsafeShellMetachars are shell metacharacters forbidden in custom landing test arguments.
const unsafeShellMetachars = ";|&`$><()\n\r\x00"

// LandingJob wraps an engine.Job with the resolved landing scope (Files,
// TestFilter, and TestShape) used to construct landing verification test commands.
type LandingJob struct {
	*engine.Job
	TaskID     string   `json:"task_id,omitempty"`
	Files      []string `json:"files,omitempty"`
	TestFilter string   `json:"test_filter,omitempty"`
	TestShape  string   `json:"test_shape,omitempty"`
}

// LandingCheckReceipt records how test_shape and test_filter shaped a task's
// landing verification test command.
type LandingCheckReceipt struct {
	TaskID        string    `json:"task_id"`
	DeclaredShape string    `json:"declared_test_shape,omitempty"`
	ResolvedShape string    `json:"resolved_test_shape"`
	TestFilter    string    `json:"test_filter,omitempty"`
	Files         []string  `json:"files,omitempty"`
	BaseArgs      []string  `json:"base_args"`
	EffectiveArgs []string  `json:"effective_args"`
	CommandLine   string    `json:"command_line"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// FormatLogLine formats a human-readable operator log line showing the resolved
// test_shape and effective test arguments for landing verification.
func (r LandingCheckReceipt) FormatLogLine() string {
	task := r.TaskID
	if task == "" {
		task = "unknown"
	}
	return fmt.Sprintf("landing test check task=%s test_shape=%s (declared=%q) filter=%q args=%q",
		task, r.ResolvedShape, r.DeclaredShape, r.TestFilter, r.CommandLine)
}

// landingJobFromEnvelope extracts a LandingJob from a task envelope and resolved liveTaskScope.
func landingJobFromEnvelope(env envelope, scope liveTaskScope) *LandingJob {
	adm := env.Admission
	if env.Job.InFlight != nil {
		adm = dispatchAdmission(env, *env.Job.InFlight)
	}
	files := slices.Clone(scope.Files)
	if len(adm.DeclaredFiles) > 0 {
		files = slices.Clone(adm.DeclaredFiles)
	}
	filter := ""
	if scope.TestID != "" {
		filter = controller.ExactTestFilter(scope.TestID)
	}
	if adm.TestFilter != "" {
		filter = adm.TestFilter
	}
	shape := adm.TestShape
	if shape == "" {
		if red := latestLiveEvidence(env.Job, "RedReport"); red != nil {
			if s, ok := red.Fields["test_shape"].(string); ok && s != "" {
				shape = s
			}
		}
	}
	j := env.Job
	return &LandingJob{
		Job:        &j,
		TaskID:     env.Job.TaskID,
		Files:      files,
		TestFilter: filter,
		TestShape:  shape,
	}
}

// landingJobFromEngineJob constructs a LandingJob from an *engine.Job by inspecting
// its ApprovedScope and recorded Evidence when explicit fields are not set.
func landingJobFromEngineJob(job *engine.Job) *LandingJob {
	if job == nil {
		return &LandingJob{}
	}
	lj := &LandingJob{
		Job:    job,
		TaskID: job.TaskID,
		Files:  slices.Clone(job.ApprovedScope),
	}
	if len(lj.Files) == 0 {
		if spec := latestLiveEvidence(*job, "Spec"); spec != nil {
			if declared, err := liveEvidencePaths(spec.Fields["declared_files"]); err == nil {
				lj.Files = declared
			}
		}
	}
	if red := latestLiveEvidence(*job, "RedReport"); red != nil {
		if len(lj.Files) == 0 {
			if declared, err := liveEvidencePaths(red.Fields["declared_files"]); err == nil {
				lj.Files = declared
			}
		}
		if s, ok := red.Fields["test_shape"].(string); ok && s != "" {
			lj.TestShape = s
		}
		if f, ok := red.Fields["test_filter"].(string); ok && f != "" {
			lj.TestFilter = f
		} else if id, ok := red.Fields["test_id"].(string); ok && id != "" {
			lj.TestFilter = id
		}
	}
	return lj
}

// normalizeLandingTestShape resolves a declared test_shape to one of the canonical
// landing shapes ("run", "pkg", "file", "custom"). Empty defaults to "run".
func normalizeLandingTestShape(shape string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(shape))
	switch s {
	case "rust-unit", "python-pytest", "node-test":
		return s, nil
	case "", LandingTestShapeRun, "go-unit":
		return LandingTestShapeRun, nil
	case LandingTestShapePkg, "go-integration":
		return LandingTestShapePkg, nil
	case LandingTestShapeFile, "script":
		return LandingTestShapeFile, nil
	case LandingTestShapeCustom, "runner":
		return LandingTestShapeCustom, nil
	default:
		return "", fmt.Errorf("unsupported landing test_shape %q: must be one of %v or %v", shape, workflow.TestShapes, LandingTestShapes)
	}
}

// buildLandingTestArgs translates job.TestShape, job.TestFilter, and job.Files
// into concrete `go test` arguments starting from baseArgs. If unsafe arguments
// or an invalid shape are encountered, it returns nil.
func buildLandingTestArgs(job *LandingJob, baseArgs []string) []string {
	args, err := buildLandingTestArgsChecked(job, baseArgs)
	if err != nil {
		return nil
	}
	return args
}

// buildLandingTestArgsChecked translates job.TestShape, job.TestFilter, and
// job.Files into concrete `go test` arguments and returns an error if any
// argument or test_shape is unsafe or invalid.
func buildLandingTestArgsChecked(job *LandingJob, baseArgs []string) ([]string, error) {
	base := slices.Clone(baseArgs)
	if len(base) == 0 {
		base = []string{"go", "test", "./..."}
	}
	if job == nil {
		return base, nil
	}
	effective := *job
	if effective.Job != nil && effective.TestShape == "" && effective.TestFilter == "" && len(effective.Files) == 0 {
		derived := landingJobFromEngineJob(effective.Job)
		effective = *derived
	}

	rawShape := strings.ToLower(strings.TrimSpace(effective.TestShape))
	shape, err := normalizeLandingTestShape(effective.TestShape)
	if err != nil {
		return nil, err
	}
	filter := strings.TrimSpace(effective.TestFilter)

	switch shape {
	case "rust-unit", "python-pytest", "node-test":
		return effects.ShapeTestCommand(base, shape, filter)
	case LandingTestShapeRun:
		if filter == "" {
			return base, nil
		}
		if strings.ContainsRune(filter, '\x00') {
			return nil, errors.New("test_filter contains NUL byte")
		}
		return appendRunFlag(base, filter), nil

	case LandingTestShapePkg:
		if (rawShape == "go-integration" && filter != "" && !strings.HasPrefix(filter, "./")) || (rawShape == "pkg" && strings.HasPrefix(filter, "^")) {
			if strings.ContainsRune(filter, '\x00') {
				return nil, errors.New("test_filter contains NUL byte")
			}
			pkgs, err := derivePackagePathsFromFiles(effective.Files)
			if err != nil {
				return nil, err
			}
			out := base
			if len(pkgs) > 0 {
				out = replaceDefaultPkgTarget(out, pkgs)
			}
			return appendRunFlag(out, filter), nil
		}
		var pkgs []string
		if filter != "" {
			pkgs, err = parseLandingPkgFilter(filter)
			if err != nil {
				return nil, err
			}
		} else {
			pkgs, err = derivePackagePathsFromFiles(effective.Files)
			if err != nil {
				return nil, err
			}
		}
		if len(pkgs) == 0 {
			return base, nil
		}
		return replaceDefaultPkgTarget(base, pkgs), nil

	case LandingTestShapeFile:
		if filter != "" && (rawShape == "script" || isLandingTestNameOrRegex(filter)) {
			if strings.ContainsRune(filter, '\x00') {
				return nil, errors.New("test_filter contains NUL byte")
			}
			pkgs, err := derivePackagePathsFromFiles(effective.Files)
			if err != nil {
				return nil, err
			}
			out := base
			if len(pkgs) > 0 {
				out = replaceDefaultPkgTarget(out, pkgs)
			}
			return appendRunFlag(out, filter), nil
		}
		var fileSources []string
		if filter != "" {
			tokens := splitFilterList(filter)
			fileSources = append(fileSources, tokens...)
		}
		fileSources = append(fileSources, effective.Files...)
		pkgs, err := derivePackagePathsFromFiles(fileSources)
		if err != nil {
			return nil, err
		}
		if len(pkgs) == 0 {
			return base, nil
		}
		return replaceDefaultPkgTarget(base, pkgs), nil

	case LandingTestShapeCustom:
		if filter == "" {
			return base, nil
		}
		if rawShape == "runner" {
			if strings.ContainsRune(filter, '\x00') {
				return nil, errors.New("test_filter contains NUL byte")
			}
			return appendRunFlag(base, filter), nil
		}
		customArgs, err := parseLandingCustomArgs(filter)
		if err != nil {
			return nil, err
		}
		return append(base, customArgs...), nil

	default:
		return nil, fmt.Errorf("unsupported landing test_shape %q", effective.TestShape)
	}
}

// appendRunFlag appends `-run <filter>` to args, or updates an existing `-run` flag if present.
func appendRunFlag(args []string, filter string) []string {
	out := slices.Clone(args)
	for i := 0; i < len(out); i++ {
		if out[i] == "-run" || out[i] == "--run" {
			if i+1 < len(out) {
				out[i+1] = filter
				return out
			}
		}
		if strings.HasPrefix(out[i], "-run=") || strings.HasPrefix(out[i], "--run=") {
			prefix, _, _ := strings.Cut(out[i], "=")
			out[i] = prefix + "=" + filter
			return out
		}
	}
	return append(out, "-run", filter)
}

// replaceDefaultPkgTarget replaces the default `./...` (or `.`) package target in
// args with pkgs, or appends pkgs if no default package target is present.
func replaceDefaultPkgTarget(args []string, pkgs []string) []string {
	if len(pkgs) == 0 {
		return slices.Clone(args)
	}
	out := make([]string, 0, len(args)+len(pkgs))
	replaced := false
	for _, a := range args {
		if !replaced && (a == "./..." || a == ".") {
			out = append(out, pkgs...)
			replaced = true
			continue
		}
		out = append(out, a)
	}
	if !replaced {
		out = append(out, pkgs...)
	}
	return out
}

// splitFilterList splits a filter string on whitespace and commas.
func splitFilterList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == ','
	})
}

// parseLandingPkgFilter splits a non-empty pkg test_filter on whitespace/commas
// and normalizes each relative package path to have a `./` prefix.
func parseLandingPkgFilter(filter string) ([]string, error) {
	tokens := splitFilterList(filter)
	var pkgs []string
	seen := make(map[string]bool, len(tokens))
	for _, raw := range tokens {
		norm, err := normalizeLandingPkgPattern(raw)
		if err != nil {
			return nil, err
		}
		if !seen[norm] {
			seen[norm] = true
			pkgs = append(pkgs, norm)
		}
	}
	return pkgs, nil
}

// normalizeLandingPkgPattern validates and normalizes a single package pattern.
func normalizeLandingPkgPattern(raw string) (string, error) {
	p := filepath.ToSlash(strings.TrimSpace(raw))
	if p == "" {
		return "", errors.New("empty package pattern")
	}
	if strings.ContainsAny(p, unsafeShellMetachars) {
		return "", fmt.Errorf("package pattern %q contains unsafe shell metacharacters", raw)
	}
	if strings.HasPrefix(p, "-") {
		return "", fmt.Errorf("package pattern %q must not start with '-'", raw)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("package pattern %q must be relative, not absolute", raw)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("package pattern %q must not traverse parent directories", raw)
		}
	}
	switch {
	case p == "." || p == "./":
		return ".", nil
	case p == "...":
		return "./...", nil
	case strings.HasPrefix(p, "./"):
		return cleanPkgPattern(p), nil
	default:
		return cleanPkgPattern("./" + p), nil
	}
}

// cleanPkgPattern cleans a `./`-prefixed package pattern while preserving `./` and `/...`.
func cleanPkgPattern(p string) string {
	wildcard := false
	if strings.HasSuffix(p, "/...") {
		wildcard = true
		p = strings.TrimSuffix(p, "/...")
	} else if p == "./..." {
		return "./..."
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		if wildcard {
			return "./..."
		}
		return "."
	}
	if !strings.HasPrefix(cleaned, "./") {
		cleaned = "./" + cleaned
	}
	if wildcard {
		cleaned += "/..."
	}
	return cleaned
}

// derivePackagePathsFromFiles derives unique `./<dir>` parent package paths from a list of files.
func derivePackagePathsFromFiles(files []string) ([]string, error) {
	var pkgs []string
	seen := make(map[string]bool, len(files))
	for _, raw := range files {
		f := filepath.ToSlash(strings.TrimSpace(raw))
		if f == "" {
			continue
		}
		if strings.ContainsAny(f, unsafeShellMetachars) {
			return nil, fmt.Errorf("file path %q contains unsafe shell metacharacters", raw)
		}
		if strings.HasPrefix(f, "-") {
			return nil, fmt.Errorf("file path %q must not start with '-'", raw)
		}
		if strings.HasPrefix(f, "/") {
			return nil, fmt.Errorf("file path %q must be relative, not absolute", raw)
		}
		for _, seg := range strings.Split(f, "/") {
			if seg == ".." {
				return nil, fmt.Errorf("file path %q must not traverse parent directories", raw)
			}
		}
		trimmed := strings.TrimPrefix(f, "./")
		cleaned := path.Clean(trimmed)
		if cleaned == "." || cleaned == "" {
			if !seen["."] {
				seen["."] = true
				pkgs = append(pkgs, ".")
			}
			continue
		}
		dir := path.Dir(cleaned)
		var pkg string
		if dir == "." || dir == "" {
			pkg = "."
		} else {
			pkg = "./" + dir
		}
		if !seen[pkg] {
			seen[pkg] = true
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs, nil
}

// isLandingTestNameOrRegex reports whether filter in "file" mode is a test name or
// regex rather than a file path (i.e. does not end in ".go" and does not contain "/").
func isLandingTestNameOrRegex(filter string) bool {
	f := strings.TrimSpace(filter)
	if f == "" || strings.HasSuffix(strings.ToLower(f), ".go") || strings.Contains(f, "/") {
		return false
	}
	for _, tok := range splitFilterList(f) {
		if strings.HasSuffix(strings.ToLower(tok), ".go") {
			return false
		}
	}
	return true
}

// parseLandingCustomArgs splits a custom test_filter into whitespace-separated
// arguments while rejecting unsafe shell metacharacters and forbidden execution
// flags (-exec, -toolexec).
func parseLandingCustomArgs(filter string) ([]string, error) {
	if strings.ContainsAny(filter, unsafeShellMetachars) {
		return nil, fmt.Errorf("custom test_filter %q contains unsafe shell metacharacters", filter)
	}
	tokens := strings.Fields(filter)
	for _, tok := range tokens {
		if isForbiddenCustomTestFlag(tok) {
			return nil, fmt.Errorf("custom test_filter argument %q is forbidden: overriding -exec or -toolexec is not allowed", tok)
		}
	}
	return tokens, nil
}

// isForbiddenCustomTestFlag reports whether arg is or sets -exec or -toolexec
// (including --exec, --toolexec, -test.exec, -test.toolexec).
func isForbiddenCustomTestFlag(arg string) bool {
	lower := strings.ToLower(strings.TrimSpace(arg))
	name, _, _ := strings.Cut(lower, "=")
	name = strings.TrimPrefix(name, "-")
	name = strings.TrimPrefix(name, "-")
	switch name {
	case "exec", "toolexec", "test.exec", "test.toolexec":
		return true
	default:
		return false
	}
}

// resolveLandingTestReceipt resolves the landing test arguments for job and constructs
// a LandingCheckReceipt recording the resolved test_shape and effective command.
func resolveLandingTestReceipt(job *LandingJob, baseArgs []string) (LandingCheckReceipt, error) {
	base := slices.Clone(baseArgs)
	if len(base) == 0 {
		base = []string{"go", "test", "./..."}
	}
	var declaredShape, filter, taskID string
	var files []string
	if job != nil {
		declaredShape = job.TestShape
		filter = job.TestFilter
		taskID = job.TaskID
		files = slices.Clone(job.Files)
		if taskID == "" && job.Job != nil {
			taskID = job.Job.TaskID
		}
	}
	resolvedShape, err := normalizeLandingTestShape(declaredShape)
	if err != nil {
		return LandingCheckReceipt{}, err
	}
	effectiveArgs, err := buildLandingTestArgsChecked(job, base)
	if err != nil {
		return LandingCheckReceipt{}, err
	}
	return LandingCheckReceipt{
		TaskID:        taskID,
		DeclaredShape: declaredShape,
		ResolvedShape: resolvedShape,
		TestFilter:    filter,
		Files:         files,
		BaseArgs:      base,
		EffectiveArgs: effectiveArgs,
		CommandLine:   strings.Join(effectiveArgs, " "),
		RecordedAt:    time.Now().UTC(),
	}, nil
}

// landingTestReceiptKey returns the controlplane marker key for a landing check receipt.
func landingTestReceiptKey(landingID, taskID string) string {
	return fmt.Sprintf("landing-test-shape/%s/%s", landingID, taskID)
}

// recordLandingTestReceipt logs the resolved test_shape and effective test arguments
// and persists the receipt as a marker in the controlplane store when available.
func (d *Daemon) recordLandingTestReceipt(ctx context.Context, landingID string, receipt LandingCheckReceipt, audit controlplane.Audit) error {
	if d == nil {
		return nil
	}
	d.taskLog("%s\n", receipt.FormatLogLine())
	if d.store == nil || (landingID == "" && receipt.TaskID == "") {
		return nil
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	a := audit
	if a.Actor == "" {
		a.Actor = "daemon"
	}
	a.Action = "landing-test-shape"
	if a.Outcome == "" {
		a.Outcome = "ok"
	}
	if landingID != "" {
		if err := d.store.PutMarker(ctx, a, landingTestReceiptKey(landingID, receipt.TaskID), raw); err != nil {
			return err
		}
		if err := d.store.PutMarker(ctx, a, "landing-test-receipt-"+landingID, raw); err != nil {
			return err
		}
	}
	if receipt.TaskID != "" && receipt.TaskID != landingID {
		if err := d.store.PutMarker(ctx, a, "landing-test-receipt-"+receipt.TaskID, raw); err != nil {
			return err
		}
	}
	return nil
}

// driveLandingRequest wires target document policy, commit identity, content
// policy, and deny_paths enforcement onto req from pinnedTgt and env (FAE-CTL-052, FAE-SEC-009, FAE-VCS-012).
func driveLandingRequest(req *controller.LandRequest, env envelope, scope liveTaskScope, pinnedTgt *TargetConfig) error {
	var denyPaths []string
	if len(env.Job.DenyPaths) > 0 {
		denyPaths = append(denyPaths, env.Job.DenyPaths...)
	}
	if pinnedTgt != nil && len(pinnedTgt.DenyPaths) > 0 {
		for _, dp := range pinnedTgt.DenyPaths {
			if !slices.Contains(denyPaths, dp) {
				denyPaths = append(denyPaths, dp)
			}
		}
	}
	if err := checkScopeDenyPaths(scope.Files, scope.TestFile, denyPaths); err != nil {
		return err
	}
	if err := checkScopeDenyPaths(scope.ReadOnlyFiles, "", denyPaths); err != nil {
		return err
	}
	if req == nil || pinnedTgt == nil {
		return nil
	}
	docGlobs, docChecks := effectiveTargetDocumentPolicy(*pinnedTgt)
	if len(req.DocumentGlobs) == 0 && len(docGlobs) > 0 {
		req.DocumentGlobs = slices.Clone(docGlobs)
		req.Policy.DocumentGlobs = slices.Clone(docGlobs)
	}
	if len(req.DocumentChecks) == 0 && len(docChecks) > 0 {
		converted := make([]controller.DocumentCheck, 0, len(docChecks))
		for _, dc := range docChecks {
			converted = append(converted, controller.DocumentCheck{
				ID:         dc.ID,
				Command:    slices.Clone(dc.Command),
				Format:     dc.Format,
				Output:     dc.Output,
				CountField: dc.CountField,
				Writes:     slices.Clone(dc.Writes),
			})
		}
		req.DocumentChecks = converted
		req.Policy.DocumentChecks = slices.Clone(converted)
	}
	req.CommitIdentity = pinnedTgt.EffectsCommitIdentity()
	req.ContentPolicy = pinnedTgt.EffectsContentPolicy()
	return nil
}

// applyLandingTestShape resolves the task's landing test arguments according to
// its declared test_shape and test_filter, updates the LandRequest QA check list
// when a non-run shape (pkg, file, custom) is active, and returns the receipt.
func applyLandingTestShape(req *controller.LandRequest, env envelope, scope liveTaskScope, baseArgs []string) (LandingCheckReceipt, error) {
	if err := driveLandingRequest(req, env, scope, nil); err != nil {
		return LandingCheckReceipt{}, err
	}
	if len(baseArgs) == 0 {
		baseArgs = []string{"go", "test", "-json", "-count=1", "./..."}
	}
	lj := landingJobFromEnvelope(env, scope)
	receipt, err := resolveLandingTestReceipt(lj, baseArgs)
	if err != nil {
		return LandingCheckReceipt{}, err
	}
	if req != nil {
		switch receipt.ResolvedShape {
		case LandingTestShapePkg, LandingTestShapeFile, LandingTestShapeCustom:
			if !slices.ContainsFunc(req.QA, func(argv []string) bool { return slices.Equal(argv, receipt.EffectiveArgs) }) {
				req.QA = append(slices.Clone(req.QA), receipt.EffectiveArgs)
			}
		case LandingTestShapeRun:
			if receipt.TestFilter != "" && req.Filter == "" {
				req.Filter = receipt.TestFilter
			}
		}
	}
	return receipt, nil
}

// validateLandingTestShape validates a task's declared test_shape and test_filter
// for landing verification compatibility alongside workflow.TestShapes.
func validateLandingTestShape(shape, filter string, files []string) error {
	if shape == "" {
		return nil
	}
	_, err := buildLandingTestArgsChecked(&LandingJob{
		TestShape:  shape,
		TestFilter: filter,
		Files:      files,
	}, []string{"go", "test", "./..."})
	return err
}

// resolveHostCommandTestShape determines the effective test shape for a live
// HostCommand execution from the stage's configured TestShape and the task's
// admitted TestShape. Whole-suite QA stages retain an empty shape so the full
// suite executes.
func resolveHostCommandTestShape(stageShape, admittedShape, stageID, role string, hasTarget bool, argv []string, testID string) string {
	if admittedShape == "" {
		return stageShape
	}
	if stageShape == "" {
		lowerStage := strings.ToLower(stageID)
		if hasTarget || strings.EqualFold(role, "qa") || strings.Contains(lowerStage, "qa") || strings.Contains(lowerStage, "suite") {
			return ""
		}
	}
	cand := admittedShape
	if cand == LandingTestShapeRun {
		cand = "go-unit"
	}
	if hostCommandCompatibleWithShape(argv, cand, testID) {
		return cand
	}
	return stageShape
}

func hostCommandCompatibleWithShape(argv []string, shape, testID string) bool {
	if len(argv) == 0 || testID == "" {
		return false
	}
	for i := 2; i < len(argv); i++ {
		if argv[i] == "-run" || strings.HasPrefix(argv[i], "-run=") {
			return false
		}
	}
	switch shape {
	case "rust-unit", "python-pytest", "node-test":
		_, err := effects.ShapeTestCommand(argv, shape, testID)
		return err == nil
	case "go-unit":
		return len(argv) >= 3 && filepath.Base(argv[0]) == "go" && argv[1] == "test" && strings.HasPrefix(testID, "Test")
	case "go-integration":
		if len(argv) < 3 || filepath.Base(argv[0]) != "go" || argv[1] != "test" || !strings.HasPrefix(testID, "Test") {
			return false
		}
		targets := 0
		for i := 2; i < len(argv); i++ {
			if argv[i] == "-p" || argv[i] == "-parallel" || argv[i] == "-test.parallel" || argv[i] == "-o" {
				i++
				continue
			}
			if !strings.HasPrefix(argv[i], "-") {
				if strings.HasSuffix(argv[i], "...") {
					return false
				}
				targets++
			}
		}
		return targets == 1
	case "script":
		return strings.HasPrefix(argv[0], "./") && filepath.Clean(argv[0][2:]) == argv[0][2:] && filepath.IsLocal(argv[0][2:])
	case "runner":
		return !strings.HasPrefix(argv[0], "./") && filepath.Base(argv[0]) != "go" && !filepath.IsAbs(argv[0]) && !strings.Contains(argv[0], "/")
	default:
		return false
	}
}
