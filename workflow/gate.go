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

// Expr is a closed JSON AST. Operations: literal, field, count, exists, eq,
// ne, lt, le, gt, ge, and, or, not, prefix, in. A field or count selects
// evidence by type and/or stage. All selects every match; otherwise the last
// recorded match wins. Count's optional argument is evaluated per match.
// The evaluator caps depth at 64 and work at 10000 nodes (including filters).
type Expr struct {
	Op    string   `json:"op"`
	Args  []Expr   `json:"args,omitempty"`
	Value any      `json:"value,omitempty"`
	Type  string   `json:"type,omitempty"`
	Stage string   `json:"stage,omitempty"`
	Path  []string `json:"path,omitempty"`
	All   bool     `json:"all,omitempty"`
	Enum  []string `json:"enum,omitempty"`
}
type Evidence struct {
	ID         string         `json:"id"`
	TaskID     string         `json:"task_id"`
	Type       string         `json:"type"`
	Version    int            `json:"version"`
	Stage      string         `json:"stage"`
	Tree       string         `json:"tree"`
	Role       string         `json:"role,omitempty"`
	Session    string         `json:"session,omitempty"`
	Provenance bool           `json:"provenance"`
	Fields     map[string]any `json:"fields"`
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
	case "field", "count", "exists":
		selected := []Evidence{}
		for _, a := range e.evidence {
			if (x.Type == "" || a.Type == x.Type) && (x.Stage == "" || a.Stage == x.Stage) {
				selected = append(selected, a)
			}
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
			var v any = a.Fields
			for _, part := range x.Path {
				m, ok := v.(map[string]any)
				if !ok {
					return e.unknown(x, "field type mismatch")
				}
				var found bool
				v, found = m[part]
				if !found {
					return e.unknown(x, "missing field "+part)
				}
			}
			if len(x.Enum) > 0 {
				s, ok := v.(string)
				found := false
				for _, allowed := range x.Enum {
					found = found || s == allowed
				}
				if !ok || !found {
					return e.unknown(x, "unrecognised status")
				}
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
func ValidateExpr(x Expr) error { return validateExpr(x, 0) }
func validateExpr(x Expr, depth int) error {
	if depth > 64 {
		return fmt.Errorf("expression too deep")
	}
	n := len(x.Args)
	switch x.Op {
	case "literal":
		if n != 0 {
			return fmt.Errorf("literal has arguments")
		}
	case "field", "exists":
		if n != 0 || x.Type == "" && x.Stage == "" {
			return fmt.Errorf("lookup requires selector and no arguments")
		}
	case "count":
		if n > 1 || x.Type == "" && x.Stage == "" {
			return fmt.Errorf("count requires selector and at most one filter")
		}
	case "not":
		if n != 1 {
			return fmt.Errorf("not requires one argument")
		}
	case "eq", "ne", "lt", "le", "gt", "ge", "and", "or", "prefix", "in":
		if n != 2 {
			return fmt.Errorf("%s requires two arguments", x.Op)
		}
	default:
		return fmt.Errorf("unknown operation %q", x.Op)
	}
	for _, a := range x.Args {
		if err := validateExpr(a, depth+1); err != nil {
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
