package workflow

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Expr is a closed JSON AST. Operations: literal, field, count, exists, item,
// eq, ne, lt, le, gt, ge, and, or, not, prefix, in. A field or count selects
// evidence by type and/or stage. All selects every match; otherwise the last
// recorded match wins. Count's optional argument is evaluated per match; a
// count with a path counts elements of that array field instead, and its
// filter reads the current element through item.
// The evaluator caps depth at 64 and work at 10000 nodes (including filters).
type Expr struct {
	Op        string   `json:"op"`
	ItemDepth int      `json:"item_depth,omitempty"`
	Args      []Expr   `json:"args,omitempty"`
	Value     any      `json:"value,omitempty"`
	Type      string   `json:"type,omitempty"`
	Stage     string   `json:"stage,omitempty"`
	Path      []string `json:"path,omitempty"`
	All       bool     `json:"all,omitempty"`
	Enum      []string `json:"enum,omitempty"`
}
type Evidence struct {
	// Value retains a top-level list artifact; Fields exposes its items to the
	// gate and typed binding vocabulary used for object artifacts.
	Value      json.RawMessage `json:"value,omitempty"`
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	Stage      string          `json:"stage"`
	Tree       string          `json:"tree"`
	Role       string          `json:"role,omitempty"`
	Session    string          `json:"session,omitempty"`
	Provenance bool            `json:"provenance"`
	Fields     map[string]any  `json:"fields"`
}
type Truth string

const (
	True    Truth = "true"
	False   Truth = "false"
	Unknown Truth = "unknown"
)

type Leaf struct {
	Op      string   `json:"op"`
	Path    []string `json:"path,omitempty"`
	Value   any      `json:"value"`
	Unknown string   `json:"unknown,omitempty"`
}
type GateResult struct {
	Value       Truth    `json:"value"`
	Passed      bool     `json:"passed"`
	EvidenceIDs []string `json:"evidence_ids"`
	Leaves      []Leaf   `json:"leaves"`
	Steps       int      `json:"steps"`
}
type evaluator struct {
	evidence []Evidence
	result   GateResult
	ids      map[string]bool
	// item is the array element in scope of an element count's filter.
	items []any
}
type value struct {
	data    any
	unknown bool
}

func Eval(expr Expr, evidence []Evidence) GateResult {
	e := evaluator{evidence: evidence, ids: map[string]bool{}}
	v := e.eval(expr, 0)
	e.result.Value = truth(v)
	e.result.Passed = e.result.Value == True
	for id := range e.ids {
		e.result.EvidenceIDs = append(e.result.EvidenceIDs, id)
	}
	sort.Strings(e.result.EvidenceIDs)
	return e.result
}
func truth(v value) Truth {
	if v.unknown {
		return Unknown
	}
	b, ok := v.data.(bool)
	if !ok {
		return Unknown
	}
	if b {
		return True
	}
	return False
}
func (e *evaluator) unknown(x Expr, reason string) value {
	e.result.Leaves = append(e.result.Leaves, Leaf{Op: x.Op, Path: x.Path, Unknown: reason})
	return value{unknown: true}
}
func (e *evaluator) eval(x Expr, depth int) value {
	if depth > 64 || e.result.Steps >= 10000 {
		return e.unknown(x, "evaluation budget exhausted")
	}
	e.result.Steps++
	leaf := func(v any) value {
		e.result.Leaves = append(e.result.Leaves, Leaf{Op: x.Op, Path: x.Path, Value: v})
		return value{data: v}
	}
	switch x.Op {
	case "literal":
		return leaf(x.Value)
	case "item", "outer":
		itemDepth := x.ItemDepth
		if x.Op == "outer" {
			itemDepth = 1
		}
		if itemDepth < 0 || itemDepth >= len(e.items) {
			return e.unknown(x, "item outside its enclosing element count")
		}
		v, reason := lookup(e.items[len(e.items)-1-itemDepth], x.Path, x.Enum)
		if reason != "" {
			return e.unknown(x, reason)
		}
		return leaf(v)
	case "field", "count", "exists":
		selected := []Evidence{}
		for _, a := range e.evidence {
			if (x.Type == "" || a.Type == x.Type) && (x.Stage == "" || a.Stage == x.Stage) {
				selected = append(selected, a)
			}
		}
		if x.Op == "count" && len(x.Path) > 0 {
			return e.countElements(x, selected, depth, leaf)
		}
		if x.Op == "count" {
			n := 0
			for _, a := range selected {
				if e.result.Steps >= 10000 {
					return e.unknown(x, "evaluation budget exhausted")
				}
				e.ids[a.ID] = true
				if len(x.Args) == 0 {
					n++
					continue
				}
				old := e.evidence
				e.evidence = []Evidence{a}
				v := e.eval(x.Args[0], depth+1)
				e.evidence = old
				if truth(v) == Unknown {
					return e.unknown(x, "unknown count filter")
				}
				if truth(v) == True {
					n++
				}
			}
			return leaf(float64(n))
		}
		if x.Op == "exists" {
			for _, a := range selected {
				if e.result.Steps >= 10000 {
					return e.unknown(x, "evaluation budget exhausted")
				}
				e.ids[a.ID] = true
			}
			return leaf(len(selected) > 0)
		}
		if len(selected) == 0 {
			return e.unknown(x, "missing evidence")
		}
		if !x.All {
			selected = selected[len(selected)-1:]
		}
		out := []any{}
		for _, a := range selected {
			e.ids[a.ID] = true
			v, reason := lookup(a.Fields, x.Path, x.Enum)
			if reason != "" {
				return e.unknown(x, reason)
			}
			out = append(out, v)
		}
		if x.All {
			return leaf(out)
		}
		return leaf(out[0])
	}
	n := len(x.Args)
	if (x.Op == "not" && n != 1) || (x.Op != "not" && n != 2) {
		return e.unknown(x, "invalid arity")
	}
	a := e.eval(x.Args[0], depth+1)
	var b value
	if n == 2 {
		b = e.eval(x.Args[1], depth+1)
	}
	switch x.Op {
	case "and":
		if truth(a) == False || truth(b) == False {
			return value{data: false}
		}
		if truth(a) == Unknown || truth(b) == Unknown {
			return e.unknown(x, "unknown operand")
		}
		return value{data: true}
	case "or":
		if truth(a) == True || truth(b) == True {
			return value{data: true}
		}
		if truth(a) == Unknown || truth(b) == Unknown {
			return e.unknown(x, "unknown operand")
		}
		return value{data: false}
	case "not":
		if truth(a) == Unknown {
			return e.unknown(x, "unknown operand")
		}
		return value{data: truth(a) == False}
	}
	if a.unknown || b.unknown {
		return e.unknown(x, "unknown operand")
	}
	switch x.Op {
	case "eq", "ne":
		same, ok := equal(a.data, b.data)
		if !ok {
			return e.unknown(x, "comparison type mismatch")
		}
		if x.Op == "ne" {
			same = !same
		}
		return value{data: same}
	case "lt", "le", "gt", "ge":
		av, ok := exactNumber(a.data)
		bv, bok := exactNumber(b.data)
		if !ok || !bok {
			return e.unknown(x, "numeric type mismatch")
		}
		switch x.Op {
		case "lt":
			return value{data: av.Cmp(bv) < 0}
		case "le":
			return value{data: av.Cmp(bv) <= 0}
		case "gt":
			return value{data: av.Cmp(bv) > 0}
		default:
			return value{data: av.Cmp(bv) >= 0}
		}
	case "prefix":
		av, ok := a.data.(string)
		bv, bok := b.data.(string)
		if !ok || !bok {
			return e.unknown(x, "string type mismatch")
		}
		return value{data: strings.HasPrefix(av, bv)}
	case "in":
		set, ok := b.data.([]any)
		if !ok {
			return e.unknown(x, "set type mismatch")
		}
		for _, v := range set {
			eq, ok := equal(a.data, v)
			if !ok {
				return e.unknown(x, "membership type mismatch")
			}
			if eq {
				return value{data: true}
			}
		}
		return value{data: false}
	default:
		return e.unknown(x, "unknown operation")
	}
}

// countElements counts the elements of the array at x.Path in the latest
// selected record (every record when x.All), filtered with each element in
// scope of item. Missing, non-array or unknown-filter inputs yield unknown.
func (e *evaluator) countElements(x Expr, selected []Evidence, depth int, leaf func(any) value) value {
	if len(selected) == 0 {
		return e.unknown(x, "missing evidence")
	}
	if !x.All {
		selected = selected[len(selected)-1:]
	}
	n := 0
	for _, a := range selected {
		e.ids[a.ID] = true
		v, reason := lookup(a.Fields, x.Path, nil)
		if reason != "" {
			return e.unknown(x, reason)
		}
		list, ok := v.([]any)
		if !ok {
			return e.unknown(x, "field type mismatch")
		}
		for _, element := range list {
			if e.result.Steps >= 10000 {
				return e.unknown(x, "evaluation budget exhausted")
			}
			if len(x.Args) == 0 {
				n++
				continue
			}
			e.items = append(e.items, element)
			v := e.eval(x.Args[0], depth+1)
			e.items = e.items[:len(e.items)-1]
			if truth(v) == Unknown {
				return e.unknown(x, "unknown count filter")
			}
			if truth(v) == True {
				n++
			}
		}
	}
	return leaf(float64(n))
}

// lookup follows path through objects; enum restricts a string result.
func lookup(v any, path, enum []string) (any, string) {
	for _, part := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, "field type mismatch"
		}
		var found bool
		v, found = m[part]
		if !found {
			return nil, "missing field " + part
		}
	}
	if len(enum) > 0 {
		s, ok := v.(string)
		found := false
		for _, allowed := range enum {
			found = found || s == allowed
		}
		if !ok || !found {
			return nil, "unrecognised status"
		}
	}
	return v, ""
}
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, e := strconv.ParseFloat(string(n), 64)
		return f, e == nil
	}
	return 0, false
}
func equal(a, b any) (bool, bool) {
	an, aok := exactNumber(a)
	bn, bok := exactNumber(b)
	if aok || bok {
		if !aok || !bok {
			return false, false
		}
		return an.Cmp(bn) == 0, true
	}
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return false, false
	}
	switch a.(type) {
	case string, bool, nil:
		return reflect.DeepEqual(a, b), true
	}
	return false, false
}
func ValidateExpr(x Expr) error { return validateExpr(x, 0, 0) }

// validateExpr checks arity and selectors; item is legal only inside the
// filter of an element count (count with a path), its innermost scope.
func validateExpr(x Expr, depth int, element int) error {
	if depth > 64 {
		return fmt.Errorf("expression too deep")
	}
	n := len(x.Args)
	if x.ItemDepth < 0 || (x.Op != "item" && x.ItemDepth != 0) {
		return fmt.Errorf("item_depth belongs to item and must be non-negative")
	}
	switch x.Op {
	case "literal":
		if n != 0 {
			return fmt.Errorf("literal has arguments")
		}
	case "item":
		if n != 0 || x.Type != "" || x.Stage != "" || x.All || element == 0 {
			return fmt.Errorf("item requires no selector and an enclosing element count")
		}
		if x.ItemDepth >= element {
			return fmt.Errorf("item_depth exceeds enclosing element counts")
		}
	case "outer":
		if n != 0 || x.Type != "" || x.Stage != "" || x.All || element < 2 {
			return fmt.Errorf("outer requires two enclosing element counts and no selector")
		}
	case "field", "exists":
		if n != 0 || x.Type == "" && x.Stage == "" {
			return fmt.Errorf("lookup requires selector and no arguments")
		}
	case "count":
		if n > 1 || x.Type == "" && x.Stage == "" {
			return fmt.Errorf("count requires selector and at most one filter")
		}
		for _, a := range x.Args {
			if err := validateExpr(a, depth+1, element+boolInt(len(x.Path) > 0)); err != nil {
				return err
			}
		}
		return nil
	case "not":
		if n != 1 {
			return fmt.Errorf("not requires one argument")
		}
	case "eq", "ne", "lt", "le", "gt", "ge", "and", "or", "prefix", "in":
		if n != 2 {
			return fmt.Errorf("%s requires two arguments", x.Op)
		}
		if x.Op != "and" && x.Op != "or" && mistakenItemComparison(x.Args[0], x.Args[1]) {
			return fmt.Errorf("item compared with its own field; name the enclosing element with outer or item_depth")
		}
	default:
		return fmt.Errorf("unknown operation %q", x.Op)
	}
	for _, a := range x.Args {
		if err := validateExpr(a, depth+1, element); err != nil {
			return err
		}
	}
	return nil
}

func exactNumber(v any) (*big.Rat, bool) {
	var s string
	switch n := v.(type) {
	case int:
		s = strconv.Itoa(n)
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, false
		}
		s = strconv.FormatFloat(n, 'g', -1, 64)
	case json.Number:
		s = string(n)
	default:
		return nil, false
	}
	if len(s) > 10000 {
		return nil, false
	}
	if p := strings.IndexAny(s, "eE"); p >= 0 {
		n, e := strconv.Atoi(s[p+1:])
		if e != nil || n > 10000 || n < -10000 {
			return nil, false
		}
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
