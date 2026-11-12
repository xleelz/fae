package workflow

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/xleelz/fae/domain"
)

// Change is one structural difference between two workflow revisions. Kind is
// one of stage, gate, edge, role, cap, route, limit, artifact or document.
// Edges incident to an added or removed stage belong to that stage's change.
type Change struct {
	Kind  string          `json:"kind"`
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  json.RawMessage `json:"from,omitempty"`
	To    json.RawMessage `json:"to,omitempty"`
	Edges []Edge          `json:"edges,omitempty"`
}

// FloorStatus is the validator result, including kernel floors proved as graph properties.
type FloorStatus struct {
	Valid  bool    `json:"valid"`
	Issues []Issue `json:"issues"`
}
type DiffReport struct {
	From    FloorStatus `json:"from_floors"`
	To      FloorStatus `json:"to_floors"`
	Changes []Change    `json:"changes"`
}

// Diff compares documents field by field through their canonical JSON, so
// sections added to the document model are reported without code changes.
func Diff(from, to Document) (DiffReport, error) {
	report := DiffReport{From: floors(from), To: floors(to), Changes: []Change{}}
	a, err := fieldsOf(from)
	if err != nil {
		return report, err
	}
	b, err := fieldsOf(to)
	if err != nil {
		return report, err
	}
	// Members (stages, roles, limits, artifacts) are added or removed; a field
	// of a member present on both sides is always changed.
	add := func(kind, path string, x, y json.RawMessage, member bool) {
		if bytes.Equal(x, y) {
			return
		}
		op := "changed"
		if member && x == nil {
			op = "added"
		} else if member && y == nil {
			op = "removed"
		}
		report.Changes = append(report.Changes, Change{Kind: kind, Op: op, Path: path, From: x, To: y})
	}
	nested := func(x, y json.RawMessage) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
		var p, q map[string]json.RawMessage
		if err := unmarshalFields(x, &p); err != nil {
			return nil, nil, err
		}
		return p, q, unmarshalFields(y, &q)
	}
	stageAt := map[string]int{}
	for _, key := range union(a, b) {
		switch key {
		case "stages", "roles":
			p, q, err := nested(a[key], b[key])
			if err != nil {
				return report, err
			}
			for _, name := range union(p, q) {
				path := key + "." + name
				if p[name] == nil || q[name] == nil {
					kind := "role"
					if key == "stages" {
						kind = stageKind(p[name], q[name])
						stageAt[name] = len(report.Changes)
					}
					add(kind, path, p[name], q[name], true)
					continue
				}
				x, y, err := nested(p[name], q[name])
				if err != nil {
					return report, err
				}
				for _, field := range union(x, y) {
					add(memberKind(key, field, p[name], q[name]), path+"."+field, x[field], y[field], false)
				}
			}
		case "limits", "artifacts":
			p, q, err := nested(a[key], b[key])
			if err != nil {
				return report, err
			}
			for _, field := range union(p, q) {
				add(map[string]string{"limits": "limit", "artifacts": "artifact"}[key], key+"."+field, p[field], q[field], true)
			}
		case "edges":
		default:
			add("document", key, a[key], b[key], false)
		}
	}
	diffEdges(&report, from.Edges, to.Edges, stageAt)
	sort.SliceStable(report.Changes, func(i, j int) bool { return report.Changes[i].Path < report.Changes[j].Path })
	return report, nil
}

func diffEdges(report *DiffReport, from, to []Edge, stageAt map[string]int) {
	present := func(edges []Edge) map[Edge]bool {
		m := map[Edge]bool{}
		for _, e := range edges {
			m[e] = true
		}
		return m
	}
	a, b := present(from), present(to)
	var loose []Change
	visit := func(edges []Edge, other map[Edge]bool, op string) {
		for _, e := range edges {
			if other[e] {
				continue
			}
			other[e] = true // Report duplicates once.
			if i, ok := stageAt[e.To]; ok {
				report.Changes[i].Edges = append(report.Changes[i].Edges, e)
			} else if i, ok := stageAt[e.From]; ok {
				report.Changes[i].Edges = append(report.Changes[i].Edges, e)
			} else {
				raw, _ := json.Marshal(e)
				c := Change{Kind: "edge", Op: op, Path: "edges." + e.From + "->" + e.To}
				if op == "added" {
					c.To = raw
				} else {
					c.From = raw
				}
				loose = append(loose, c)
			}
		}
	}
	visit(to, a, "added")
	visit(from, b, "removed")
	report.Changes = append(report.Changes, loose...)
}

func floors(d Document) FloorStatus {
	issues := Validate(d)
	return FloorStatus{Valid: len(issues) == 0, Issues: issues}
}
func fieldsOf(d Document) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	return m, unmarshalFields(raw, &m)
}
func unmarshalFields(raw json.RawMessage, out *map[string]json.RawMessage) error {
	*out = map[string]json.RawMessage{}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	for k, v := range *out {
		c, err := domain.CanonicalJSON(v)
		if err != nil {
			return err
		}
		(*out)[k] = c
	}
	return nil
}
func union(a, b map[string]json.RawMessage) []string {
	keys := []string{}
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
func primitiveOf(raw json.RawMessage) Primitive {
	var s struct {
		Primitive Primitive `json:"primitive"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.Primitive
}
func stageKind(from, to json.RawMessage) string {
	if primitiveOf(from) == Gate || primitiveOf(to) == Gate {
		return "gate"
	}
	return "stage"
}
func memberKind(section, field string, from, to json.RawMessage) string {
	switch {
	case section == "roles" && field == "cap", section == "stages" && field == "max_iterations":
		return "cap"
	case section == "roles" && field == "routes":
		return "route"
	case section == "roles":
		return "role"
	}
	return stageKind(from, to)
}
