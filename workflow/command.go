package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CheckSpec configures a HostCommand stage's structured check mode (FAE-WF-095).
type CheckSpec struct {
	Format     string   `json:"format"`                // "json" | "lines"
	Output     string   `json:"output,omitempty"`      // "stdout" (default) or relative file path
	CountField string   `json:"count_field,omitempty"` // JSON path/field for format=="json" or synthetic count field for "lines"
	Writes     []string `json:"writes,omitempty"`      // path globs the check command is allowed to write
}

// CommandConfig holds HostCommand configuration, including structured check mode.
type CommandConfig struct {
	Command []string   `json:"command,omitempty"`
	Check   *CheckSpec `json:"check,omitempty"`
}

// Validate checks a standalone CommandConfig's CheckSpec.
func (c CommandConfig) Validate() error {
	if c.Check == nil {
		return nil
	}
	return ValidateCheckSpec(*c.Check)
}

// ArgTemplate declares a HostCommand placeholder bound from verified runtime
// configuration such as a declared external read root (FAE-RT-016).
type ArgTemplate struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

// Resolve resolves an ArgTemplate against the verified read-root paths.
func (t ArgTemplate) Resolve(readRoots map[string]string) (string, error) {
	if t.Kind != "read-root" || t.Key == "" {
		return "", fmt.Errorf("invalid arg template kind=%q key=%q", t.Kind, t.Key)
	}
	v, ok := readRoots[t.Key]
	if !ok || v == "" {
		return "", fmt.Errorf("unknown read-root %q", t.Key)
	}
	if err := CheckArg("read-root", v); err != nil {
		return "", err
	}
	return v, nil
}

// ArgBinding types one whole-argument {{name}} placeholder of a HostCommand
// template and names the bound input artifact field that supplies it
// (FAE-WF-035), or a read-root placeholder `{Kind: "read-root", Key: "<name>"}`
// (FAE-RT-016). A bound value is always exactly one literal argument: there is
// no shell, no splitting and no partial interpolation.
type ArgBinding struct {
	Type  string `json:"type,omitempty"`
	Input string `json:"input,omitempty"`
	Field string `json:"field,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Key   string `json:"key,omitempty"`
}

// IsReadRoot reports whether the binding references a declared read root (FAE-RT-016).
func (b ArgBinding) IsReadRoot() bool {
	return b.Kind == "read-root" || b.Type == "read-root"
}

// ReadRootKey returns the read-root name referenced by a read-root binding.
func (b ArgBinding) ReadRootKey() string {
	if b.Key != "" {
		return b.Key
	}
	return b.Input
}

// EffectiveType returns the slot type for template matching.
func (b ArgBinding) EffectiveType() string {
	if b.IsReadRoot() {
		return "read-root"
	}
	return b.Type
}

// ArgTypes is the closed set of placeholder value types.
var ArgTypes = []string{"go-test-name", "test-id", "relative-path", "go-package", "text", "integer", "read-root"}

var (
	placeholderName = regexp.MustCompile(`^\{\{([a-z][a-z0-9_-]*)\}\}$`)
	goTestNamePat   = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)
	integerPat      = regexp.MustCompile(`^[0-9]{1,18}$`)
)

// Placeholder reports the binding name of a whole-argument placeholder. Any
// other argument containing "{{" is not a placeholder and is refused.
func Placeholder(arg string) (string, bool) {
	m := placeholderName.FindStringSubmatch(arg)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Slot reports the type of a target-configuration shape slot {{type}}.
func Slot(arg string) (string, bool) {
	name, ok := Placeholder(arg)
	return name, ok && contains(ArgTypes, name)
}

// DeclaredPathSafe is the one path rule for every file a task may declare or
// a scope may hold: fae's --declared-file, admission, derived files, the
// scope-widen requests the engine accepts and the approved widens the
// resolver reads, RedReport paths, tasks[id] overrides, the instance-level
// files pin, and the provider bridges' pinned scope (--files, --test-file,
// --task-scopes), brief task_scope block and stage block approved_scope. A
// path is non-empty, at most 4096 bytes, relative and clean, never "." or a
// parent escape, without NUL or newline, and has no ".git" component at any
// depth in any letter case, so neither the repository's metadata nor a nested
// repository's is ever in scope. CheckArg's relative-path type refuses only a root .git; the engine
// still reads stored approved scopes with it, so a job that holds an older
// nested-.git widen loads, and the engine drops that path from its approved
// scope, with an event, before the next dispatch.
func DeclaredPathSafe(name string) bool {
	if name == "" || len(name) > 4096 || name == "." || name == ".." || path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\x00\n") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// CheckArg decides whether value is a well-formed literal argument of type t.
// Every type refuses empty values, control characters other than tab, invalid
// UTF-8 and a leading '-' (option injection).
func CheckArg(t, value string) error {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s argument is empty, oversized, invalid UTF-8 or option-like", t)
	}
	for _, r := range value {
		if r != '\t' && unicode.IsControl(r) {
			return fmt.Errorf("%s argument contains a control character", t)
		}
	}
	local := func(p string) bool {
		return !strings.ContainsAny(p, "\\") && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && p != ".git" && !strings.HasPrefix(p, ".git/")
	}
	ok := false
	switch t {
	case "go-test-name":
		ok = goTestNamePat.MatchString(value)
	case "test-id":
		ok = len(value) <= 512 && !strings.ContainsFunc(value, unicode.IsSpace)
	case "relative-path":
		ok = local(value) && value != "."
	case "go-package":
		rest, prefixed := strings.CutPrefix(value, "./")
		ok = value == "." || prefixed && local(rest) && rest != "." && !strings.Contains(rest, "...")
	case "text":
		ok = !strings.ContainsAny(value, "\n\r")
	case "integer":
		ok = integerPat.MatchString(value)
	case "read-root":
		ok = path.IsAbs(value) && path.Clean(value) == value && !strings.ContainsAny(value, "\n\r\\")
	default:
		return fmt.Errorf("unknown argument type %q", t)
	}
	if !ok {
		return fmt.Errorf("value is not a %s argument", t)
	}
	return nil
}

// BindCommand renders a HostCommand template over the dispatch's bound input
// evidence. Each placeholder must resolve to exactly one artifact of its
// input binding with a field of the declared type. Read-root placeholders stay
// templated until BindCommandWithReadRoots resolves them at execution time.
func BindCommand(s Stage, inputs []Evidence) ([]string, error) {
	return BindCommandWithReadRoots(s, inputs, nil)
}

// BindCommandWithReadRoots renders a HostCommand template over the dispatch's
// bound input evidence and verified read-root paths (FAE-RT-016).
func BindCommandWithReadRoots(s Stage, inputs []Evidence, readRoots map[string]string) ([]string, error) {
	out := make([]string, 0, len(s.Command))
	for _, arg := range s.Command {
		name, ok := Placeholder(arg)
		if !ok {
			if strings.Contains(arg, "{{") {
				return nil, fmt.Errorf("partial placeholder %q", arg)
			}
			out = append(out, arg)
			continue
		}
		if tmpl, isTmpl := s.ArgTemplates[name]; isTmpl {
			if readRoots == nil {
				out = append(out, arg)
				continue
			}
			val, err := tmpl.Resolve(readRoots)
			if err != nil {
				return nil, fmt.Errorf("placeholder %s: %w", name, err)
			}
			out = append(out, val)
			continue
		}
		b, declared := s.Bindings[name]
		if !declared {
			return nil, fmt.Errorf("undeclared placeholder %s", name)
		}
		if b.IsReadRoot() {
			if readRoots == nil {
				out = append(out, arg)
				continue
			}
			val, err := (ArgTemplate{Kind: "read-root", Key: b.ReadRootKey()}).Resolve(readRoots)
			if err != nil {
				return nil, fmt.Errorf("placeholder %s: %w", name, err)
			}
			out = append(out, val)
			continue
		}
		var in *Input
		for i := range s.Inputs {
			if s.Inputs[i].Name == b.Input {
				in = &s.Inputs[i]
			}
		}
		if in == nil {
			return nil, fmt.Errorf("placeholder %s names unknown input %s", name, b.Input)
		}
		var values []string
		for _, a := range inputs {
			if a.Type != in.Type || a.Version != in.Version || in.Stage != "" && a.Stage != in.Stage {
				continue
			}
			val, found := lookupFieldPath(a.Fields, b.Field)
			if !found {
				return nil, fmt.Errorf("placeholder %s: field %s is absent or not scalar", name, b.Field)
			}
			switch v := val.(type) {
			case string:
				values = append(values, v)
			case json.Number:
				values = append(values, v.String())
			case float64:
				values = append(values, fmt.Sprint(v))
			case int:
				values = append(values, strconv.Itoa(v))
			default:
				return nil, fmt.Errorf("placeholder %s: field %s is absent or not scalar", name, b.Field)
			}
		}
		if len(values) != 1 {
			return nil, fmt.Errorf("placeholder %s binds %d artifacts, want exactly 1", name, len(values))
		}
		if e := CheckArg(b.Type, values[0]); e != nil {
			return nil, fmt.Errorf("placeholder %s: %w", name, e)
		}
		out = append(out, values[0])
	}
	return out, nil
}

// MatchShape reports whether a bound vector fits an allowlisted shape: equal
// literal arguments, and {{type}} slots holding a well-formed value.
func MatchShape(shape, argv []string) bool {
	if len(shape) != len(argv) {
		return false
	}
	for i, want := range shape {
		if i > 0 && HasRuntimeReferences([]string{want}) {
			if !matchRuntimeArgument(want, argv[i]) {
				return false
			}
			continue
		}
		if t, ok := Slot(want); ok && i > 0 {
			if CheckArg(t, argv[i]) != nil {
				return false
			}
		} else if want != argv[i] {
			return false
		}
	}
	return true
}

// MatchTemplate reports whether every vector the template can bind fits the
// shape: literals equal, and each placeholder sits in a slot of its type.
func MatchTemplate(shape []string, s Stage) bool {
	if len(shape) != len(s.Command) {
		return false
	}
	for i, arg := range s.Command {
		if HasRuntimeReferences([]string{arg}) {
			if i == 0 || shape[i] != arg {
				return false
			}
			continue
		}
		if name, ok := Placeholder(arg); ok {
			effType := s.Bindings[name].EffectiveType()
			if tmpl, isTmpl := s.ArgTemplates[name]; isTmpl && tmpl.Kind == "read-root" {
				effType = "read-root"
			}
			if t, slot := Slot(shape[i]); !slot || i == 0 || t != effType {
				return false
			}
		} else if arg != shape[i] {
			return false
		}
	}
	return true
}

// validateBindings checks a stage's placeholder declarations (FAE-WF-035).
func validateBindings(d Document, id string, s Stage, bad func(string, string)) {
	p := "stages." + id
	used := map[string]bool{}
	for i, arg := range s.Command {
		if HasRuntimeReferences([]string{arg}) {
			if i == 0 {
				bad(fmt.Sprintf("%s.command[%d]", p, i), "placeholder cannot choose executable")
			}
			continue
		}
		name, ok := Placeholder(arg)
		if !ok {
			if strings.Contains(arg, "{{") {
				bad(fmt.Sprintf("%s.command[%d]", p, i), "placeholder must be a whole {{name}} argument")
			}
			continue
		}
		if s.Primitive != HostCommand || i == 0 {
			bad(fmt.Sprintf("%s.command[%d]", p, i), "placeholders bind HostCommand arguments, never the program")
		}
		_, inBindings := s.Bindings[name]
		_, inTemplates := s.ArgTemplates[name]
		if !inBindings && !inTemplates {
			bad(fmt.Sprintf("%s.command[%d]", p, i), "undeclared placeholder "+name)
		}
		used[name] = true
	}
	for name, tmpl := range s.ArgTemplates {
		tp := p + ".arg_templates." + name
		if s.Primitive != HostCommand {
			bad(tp, "arg_templates require a HostCommand")
		}
		if !used[name] {
			bad(tp, "arg_template is not used by the command")
		}
		if tmpl.Kind != "read-root" {
			bad(tp+".kind", "unknown arg_template kind")
		}
		if tmpl.Key == "" {
			bad(tp+".key", "read-root key required")
		}
	}
	for name, b := range s.Bindings {
		bp := p + ".bindings." + name
		if s.Primitive != HostCommand {
			bad(bp, "bindings require a HostCommand")
		}
		if !used[name] {
			bad(bp, "binding is not used by the command")
		}
		if b.IsReadRoot() {
			if b.ReadRootKey() == "" {
				bad(bp+".key", "read-root key required")
			}
			continue
		}
		if !contains(ArgTypes, b.Type) {
			bad(bp+".type", "unknown argument type")
		}
		var in *Input
		for i := range s.Inputs {
			if s.Inputs[i].Name == b.Input {
				in = &s.Inputs[i]
			}
		}
		if in == nil || in.Optional {
			bad(bp+".input", "must name a required input of the stage")
			continue
		}
		if b.Field == "" {
			bad(bp+".field", "required")
			continue
		}
		if a, ok := ArtifactDeclaration(d, in.Type); ok {
			var schema struct {
				Properties map[string]struct {
					Type any `json:"type"`
				} `json:"properties"`
			}
			if json.Unmarshal(a.Schema, &schema) == nil && schema.Properties != nil {
				prop, ok := schema.Properties[b.Field]
				if !ok && strings.Contains(b.Field, ".") {
					first, _, _ := strings.Cut(b.Field, ".")
					prop, ok = schema.Properties[first]
				}
				if !ok {
					bad(bp+".field", "artifact "+in.Type+" declares no field "+b.Field)
				} else if !strings.Contains(b.Field, ".") && prop.Type != nil && prop.Type != "string" && prop.Type != "integer" {
					bad(bp+".field", "field must be a string or integer")
				}
			}
		}
	}
}

func lookupFieldPath(fields map[string]any, fieldPath string) (any, bool) {
	if fields == nil || fieldPath == "" {
		return nil, false
	}
	if v, ok := fields[fieldPath]; ok {
		return v, true
	}
	parts := strings.Split(fieldPath, ".")
	var cur any = fields
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func validCountField(f string) bool {
	if strings.TrimSpace(f) == "" || f != strings.TrimSpace(f) {
		return false
	}
	for _, part := range strings.Split(f, ".") {
		if part == "" || strings.ContainsAny(part, " \t\r\n\x00") {
			return false
		}
	}
	return true
}

// ValidWriteGlob reports whether g is a safe relative path or glob pattern.
func ValidWriteGlob(g string) bool {
	return validWriteGlob(g, false)
}

func validWriteGlob(g string, allowTemplate bool) bool {
	if g == "" || len(g) > 4096 || g == "." || g == ".." || strings.HasPrefix(g, "/") || strings.ContainsAny(g, "\\\x00\n\r") {
		return false
	}
	parts := strings.Split(g, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.EqualFold(p, ".git") {
			return false
		}
		check := p
		if allowTemplate {
			check = templateTokenPat.ReplaceAllString(check, "item")
		}
		if strings.Contains(check, "{{") || strings.Contains(check, "}}") {
			return false
		}
		if _, err := path.Match(check, "probe"); err != nil {
			return false
		}
	}
	return true
}

var templateTokenPat = regexp.MustCompile(`\{\{item(?:\.[A-Za-z0-9_.-]+)?\}\}`)

// ValidateCheckSpec validates a CheckSpec configuration (FAE-WF-095).
func ValidateCheckSpec(c CheckSpec) error {
	if c.Format != "json" && c.Format != "lines" {
		return errors.New("check format must be json or lines")
	}
	if !validCountField(c.CountField) {
		return errors.New("check requires count_field")
	}
	if c.Output != "" && c.Output != "stdout" && !DeclaredPathSafe(c.Output) {
		return errors.New("check output must be stdout or a safe relative path")
	}
	for i, w := range c.Writes {
		if !ValidWriteGlob(w) {
			return fmt.Errorf("invalid check writes glob at index %d: %q", i, w)
		}
	}
	return nil
}

type checkSelector struct {
	Selector   selector
	CountField string
	Format     string
}

func checkRunSelectors(d Document, x Expr) []checkSelector {
	type checkInfo struct {
		countField string
		format     string
	}
	runs := map[string]checkInfo{}
	for id, s := range d.Stages {
		if s.Primitive == HostCommand && s.Check != nil && validCountField(s.Check.CountField) {
			info := checkInfo{countField: s.Check.CountField, format: s.Check.Format}
			runs["\x00"+id] = info
			if s.Output != "" {
				runs[s.Output+"\x00"] = info
				runs[s.Output+"\x00"+id] = info
			}
		}
	}
	found := map[selector]bool{}
	var out []checkSelector
	var walk func(Expr)
	walk = func(e Expr) {
		if (e.Op == "field" || e.Op == "count" || e.Op == "exists") && (e.Type != "" || e.Stage != "") {
			if info, ok := runs[e.Type+"\x00"+e.Stage]; ok {
				sel := selector{e.Type, e.Stage}
				if !found[sel] {
					found[sel] = true
					out = append(out, checkSelector{Selector: sel, CountField: info.countField, Format: info.format})
				}
			}
		}
		for _, a := range e.Args {
			walk(a)
		}
	}
	walk(x)
	sort.Slice(out, func(i, j int) bool { return out[i].Selector.label() < out[j].Selector.label() })
	return out
}

func matchesCountFieldPath(exprPath []string, countField string, format string) bool {
	if len(exprPath) == 0 {
		return false
	}
	if len(exprPath) == 1 && exprPath[0] == countField {
		return true
	}
	if format == "lines" && len(exprPath) == 1 && exprPath[0] == "line_count" {
		return true
	}
	parts := strings.Split(countField, ".")
	if len(exprPath) != len(parts) {
		return false
	}
	for i := range parts {
		if exprPath[i] != parts[i] {
			return false
		}
	}
	return true
}

func checkCountBound(x Expr, cs checkSelector) bool {
	return conjunct(x, func(a Expr) bool {
		if len(a.Args) != 2 {
			return false
		}
		left, right, op := a.Args[0], a.Args[1], a.Op
		if right.Op == "field" && left.Op == "literal" {
			left, right = right, left
			switch op {
			case "ge":
				op = "le"
			case "gt":
				op = "lt"
			case "le":
				op = "ge"
			case "lt":
				op = "gt"
			}
		}
		if left.Op != "field" || left.All || left.Type != cs.Selector.Type || left.Stage != cs.Selector.Stage || !matchesCountFieldPath(left.Path, cs.CountField, cs.Format) || right.Op != "literal" {
			return false
		}
		n, ok := number(right.Value)
		return ok && (op == "ge" && n >= 1 || op == "gt" && n >= 0 || op == "eq" && n >= 1)
	})
}

func checkGate(x Expr, stage string, h Stage) bool {
	if h.Check == nil || !validCountField(h.Check.CountField) {
		return false
	}
	exitOK := conjunct(x, func(a Expr) bool {
		if a.Op != "eq" || len(a.Args) != 2 || a.Args[0].Op != "field" || a.Args[0].All || a.Args[0].Stage != stage || len(a.Args[0].Path) != 1 || a.Args[0].Path[0] != "exit_code" || a.Args[1].Op != "literal" {
			return false
		}
		n, ok := number(a.Args[1].Value)
		return ok && n == 0
	})
	countOK := conjunct(x, func(a Expr) bool {
		if len(a.Args) != 2 || a.Args[0].Op != "field" || a.Args[0].All || a.Args[0].Stage != stage || !matchesCountFieldPath(a.Args[0].Path, h.Check.CountField, h.Check.Format) || a.Args[1].Op != "literal" {
			return false
		}
		n, ok := number(a.Args[1].Value)
		return ok && (a.Op == "ge" && n >= 1 || a.Op == "gt" && n >= 0 || a.Op == "eq" && n >= 1)
	})
	return exitOK && countOK
}

func testOrCheckGate(x Expr, stage string, h Stage) bool {
	if h.Formal != nil {
		return FormalGateAccepts(x, stage, "") || FormalGateAccepts(x, stage, h.Output)
	}
	if h.TestRun && testGate(x, stage) {
		return true
	}
	if h.Check != nil && checkGate(x, stage, h) {
		return true
	}
	return false
}

func branchArmIsFailureRoute(x Expr, sel selector) bool {
	found := false
	var walk func(Expr)
	walk = func(e Expr) {
		if (e.Op == "ne" || e.Op == "gt" || e.Op == "ge") && len(e.Args) == 2 && e.Args[0].Op == "field" && e.Args[0].Type == sel.Type && e.Args[0].Stage == sel.Stage && len(e.Args[0].Path) == 1 && e.Args[0].Path[0] == "exit_code" {
			found = true
		}
		for _, a := range e.Args {
			walk(a)
		}
	}
	walk(x)
	return found
}

func validateCheck(d Document, id string, s Stage, bad func(string, string)) {
	p := "stages." + id
	if s.Check != nil {
		if s.Primitive != HostCommand {
			bad(p+".check", "check requires a HostCommand")
		}
		if s.Check.Format != "json" && s.Check.Format != "lines" {
			bad(p+".check.format", "check format must be json or lines")
		}
		if !validCountField(s.Check.CountField) {
			bad(p+".check.count_field", "check requires count_field")
		}
		if s.Check.Output != "" && s.Check.Output != "stdout" && !DeclaredPathSafe(s.Check.Output) {
			bad(p+".check.output", "check output must be stdout or a safe relative path")
		}
		for i, w := range s.Check.Writes {
			if !ValidWriteGlob(w) {
				bad(fmt.Sprintf("%s.check.writes[%d]", p, i), "invalid check writes glob")
				continue
			}
			for _, otherID := range sortedStageIDs(d.Stages) {
				other := d.Stages[otherID]
				for _, ws := range other.WriteScope {
					if GlobsOverlap(w, normalizeTemplateGlob(ws)) {
						bad(fmt.Sprintf("%s.check.writes[%d]", p, i), fmt.Sprintf("check writes glob %q overlaps stage %s write_scope %q", w, otherID, ws))
					}
				}
				for _, b := range EffectiveBranches(other) {
					for _, ws := range b.WriteScope {
						if GlobsOverlap(w, normalizeTemplateGlob(ws)) {
							bad(fmt.Sprintf("%s.check.writes[%d]", p, i), fmt.Sprintf("check writes glob %q overlaps stage %s branch %s write_scope %q", w, otherID, b.ID, ws))
						}
					}
				}
			}
		}
	}
	if s.Primitive == Gate && s.Expr != nil {
		for _, cs := range checkRunSelectors(d, *s.Expr) {
			if !checkCountBound(*s.Expr, cs) {
				bad(p+".expression", "check gate must assert "+cs.CountField+" >= 1 for "+cs.Selector.label())
			}
		}
	}
	if s.Primitive == Branch {
		for i, arm := range s.Cases {
			for _, cs := range checkRunSelectors(d, arm.When) {
				if !branchArmIsFailureRoute(arm.When, cs.Selector) && !checkCountBound(arm.When, cs) {
					bad(fmt.Sprintf("%s.cases[%d].when", p, i), "check branch arm must assert "+cs.CountField+" >= 1 for "+cs.Selector.label())
				}
			}
		}
	}
}

// ParseCheckOutput parses raw check output (from stdout or a declared file)
// according to spec, returning the structured payload map and the numeric
// count extracted from spec.CountField (FAE-WF-095).
func ParseCheckOutput(spec CheckSpec, raw string, exitCode int, timedOut, truncated bool, digest string) (map[string]any, float64, error) {
	if err := ValidateCheckSpec(spec); err != nil {
		return nil, 0, err
	}
	switch spec.Format {
	case "json":
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
			return nil, 0, errors.New("check json output must be a JSON object")
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil || obj == nil {
			return nil, 0, fmt.Errorf("invalid check json output: %w", err)
		}
		var extra any
		if dec.Decode(&extra) == nil {
			return nil, 0, errors.New("check json output must be a single JSON object")
		}
		val, ok := lookupFieldPath(obj, spec.CountField)
		if !ok {
			return nil, 0, fmt.Errorf("check json output missing count_field %q", spec.CountField)
		}
		count, ok := number(val)
		if !ok {
			return nil, 0, fmt.Errorf("check json count_field %q must be numeric", spec.CountField)
		}
		if _, has := obj[spec.CountField]; !has {
			obj[spec.CountField] = count
		}
		if _, has := obj["exit_code"]; !has {
			obj["exit_code"] = exitCode
		}
		if _, has := obj["timed_out"]; !has {
			obj["timed_out"] = timedOut
		}
		if _, has := obj["truncated"]; !has {
			obj["truncated"] = truncated
		}
		if digest != "" {
			if _, has := obj["output_digest"]; !has {
				obj["output_digest"] = digest
			}
		}
		return obj, count, nil
	case "lines":
		var lines []string
		out := map[string]any{}
		for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			lines = append(lines, trimmed)
			if k, v, ok := strings.Cut(trimmed, ":"); ok {
				key := strings.TrimSpace(k)
				valStr := strings.TrimSpace(v)
				if key != "" && !strings.ContainsAny(key, " \t") && key != "lines" && key != "line_count" && key != spec.CountField {
					if n, err := strconv.Atoi(valStr); err == nil {
						out[key] = n
					} else if b, err := strconv.ParseBool(valStr); err == nil {
						out[key] = b
					} else {
						out[key] = valStr
					}
				}
			}
		}
		if lines == nil {
			lines = []string{}
		}
		count := len(lines)
		out["lines"] = lines
		out["line_count"] = count
		setFieldPath(out, spec.CountField, count)
		out[spec.CountField] = count
		out["exit_code"] = exitCode
		out["timed_out"] = timedOut
		out["truncated"] = truncated
		if digest != "" {
			out["output_digest"] = digest
		}
		return out, float64(count), nil
	default:
		return nil, 0, fmt.Errorf("unsupported check format %q", spec.Format)
	}
}

func setFieldPath(m map[string]any, fieldPath string, val any) {
	parts := strings.Split(fieldPath, ".")
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		next, ok := cur[parts[i]].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[parts[i]] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
}

// MatchesAnyGlob reports whether relPath matches or overlaps at least one pattern in globs.
func MatchesAnyGlob(relPath string, globs []string) bool {
	for _, g := range globs {
		if MatchGlob(g, relPath) || GlobsOverlap(g, relPath) {
			return true
		}
	}
	return false
}

// GlobsOverlap reports whether two relative path globs can match any common path.
func GlobsOverlap(a, b string) bool {
	a = strings.TrimPrefix(path.Clean(a), "./")
	b = strings.TrimPrefix(path.Clean(b), "./")
	if a == "" || b == "" || a == "." || b == "." {
		return false
	}
	if a == b {
		return true
	}
	aSegs := strings.Split(a, "/")
	bSegs := strings.Split(b, "/")
	return segmentsOverlap(aSegs, bSegs)
}

func segmentsOverlap(a, b []string) bool {
	n, m := len(a), len(b)
	dp := make([][]bool, n+1)
	for i := range dp {
		dp[i] = make([]bool, m+1)
	}
	dp[0][0] = true
	for i := 0; i <= n; i++ {
		for j := 0; j <= m; j++ {
			if !dp[i][j] {
				continue
			}
			if i < n && a[i] == "**" {
				dp[i+1][j] = true
				if j < m {
					dp[i][j+1] = true
					dp[i+1][j+1] = true
				}
			}
			if j < m && b[j] == "**" {
				dp[i][j+1] = true
				if i < n {
					dp[i+1][j] = true
					dp[i+1][j+1] = true
				}
			}
			if i < n && j < m && a[i] != "**" && b[j] != "**" && singleSegmentOverlap(a[i], b[j]) {
				dp[i+1][j+1] = true
			}
		}
	}
	return dp[n][m]
}

func singleSegmentOverlap(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	n, m := len(ra), len(rb)
	dp := make([][]bool, n+1)
	for i := range dp {
		dp[i] = make([]bool, m+1)
	}
	dp[0][0] = true
	for i := 0; i <= n; i++ {
		for j := 0; j <= m; j++ {
			if !dp[i][j] {
				continue
			}
			if i < n && ra[i] == '*' {
				dp[i+1][j] = true
				if j < m {
					dp[i][j+1] = true
					dp[i+1][j+1] = true
				}
			}
			if j < m && rb[j] == '*' {
				dp[i][j+1] = true
				if i < n {
					dp[i+1][j] = true
					dp[i+1][j+1] = true
				}
			}
			if i < n && j < m && ra[i] != '*' && rb[j] != '*' {
				if ra[i] == '?' || rb[j] == '?' || ra[i] == rb[j] {
					dp[i+1][j+1] = true
				}
			}
		}
	}
	return dp[n][m]
}

// CheckWritesDisjoint verifies that every glob in writes is valid and disjoint
// from the task's declared editable files/globs (FAE-WF-095).
func CheckWritesDisjoint(writes, declaredFiles []string) error {
	for _, w := range writes {
		if !ValidWriteGlob(w) {
			return fmt.Errorf("invalid check writes glob %q", w)
		}
		for _, d := range declaredFiles {
			if GlobsOverlap(w, d) {
				return fmt.Errorf("check writes glob %q overlaps task declared file %q", w, d)
			}
		}
	}
	return nil
}

// ExpandBranchWriteScope expands {{item}} and {{item.field}} placeholders in a
// branch's WriteScope globs using the branch's bound item element (FAE-WF-096).
func ExpandBranchWriteScope(patterns []string, item any) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(patterns))
	for _, pat := range patterns {
		var expandErr error
		expanded := templateTokenPat.ReplaceAllStringFunc(pat, func(token string) string {
			inner := strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}")
			var val any = item
			if rest, ok := strings.CutPrefix(inner, "item."); ok {
				m, ok := item.(map[string]any)
				if !ok {
					expandErr = fmt.Errorf("cannot resolve %s on non-object item", token)
					return ""
				}
				val, ok = lookupFieldPath(m, rest)
				if !ok {
					expandErr = fmt.Errorf("missing field %s in branch item", token)
					return ""
				}
			}
			var s string
			switch v := val.(type) {
			case string:
				s = v
			case json.Number:
				s = v.String()
			case float64:
				s = fmt.Sprint(v)
			case int:
				s = strconv.Itoa(v)
			default:
				expandErr = fmt.Errorf("placeholder %s did not resolve to scalar", token)
				return ""
			}
			if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "*?[]\\\x00\n\r") || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
				expandErr = fmt.Errorf("placeholder %s resolved to unsafe path segment %q", token, s)
				return ""
			}
			return s
		})
		if expandErr != nil {
			return nil, expandErr
		}
		if !ValidWriteGlob(expanded) {
			return nil, fmt.Errorf("expanded write_scope %q is not a valid relative glob", expanded)
		}
		out = append(out, expanded)
	}
	return out, nil
}

func sortedStageIDs(stages map[string]Stage) []string {
	ids := make([]string, 0, len(stages))
	for id := range stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func normalizeTemplateGlob(g string) string {
	return templateTokenPat.ReplaceAllString(g, "*")
}

// CheckDocumentWritesDisjoint verifies that every HostCommand Check.Writes glob
// in d is disjoint from all declaredFiles (FAE-WF-095(e)).
func CheckDocumentWritesDisjoint(d Document, declaredFiles []string) error {
	if len(declaredFiles) == 0 {
		return nil
	}
	for _, stageID := range sortedStageIDs(d.Stages) {
		s := d.Stages[stageID]
		if s.Check == nil || len(s.Check.Writes) == 0 {
			continue
		}
		if err := CheckWritesDisjoint(s.Check.Writes, declaredFiles); err != nil {
			return fmt.Errorf("stage %s: %w", stageID, err)
		}
	}
	return nil
}
