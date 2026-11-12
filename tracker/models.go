package tracker

import (
	"encoding/json"
	"strings"

	"github.com/xleelz/fae/symphony"
)

// IssueRef identifies a parent or child (sub-issue) in a tracker issue hierarchy.
type IssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state,omitempty"`
}

// Issue extends symphony.Issue with normalized parent and sub-issue hierarchy references.
type Issue struct {
	symphony.Issue
	Parent   *IssueRef  `json:"parent,omitempty"`
	Children []IssueRef `json:"children,omitempty"`
}

// ToSymphonyIssue converts a tracker.Issue into a symphony.Issue while preserving
// Parent and Children hierarchy metadata inside NativeRef.
func (i Issue) ToSymphonyIssue() symphony.Issue {
	return WithHierarchy(i.Issue, i.Parent, i.Children)
}

// ToTrackerIssue reconstructs a tracker.Issue (including Parent and Children) from a symphony.Issue.
func ToTrackerIssue(iss symphony.Issue) Issue {
	parent, children := ExtractHierarchy(iss)
	return Issue{
		Issue:    iss,
		Parent:   parent,
		Children: children,
	}
}

// WithHierarchy returns a copy of iss with parent and children stored in NativeRef when present.
func WithHierarchy(iss symphony.Issue, parent *IssueRef, children []IssueRef) symphony.Issue {
	if parent == nil && len(children) == 0 {
		return iss
	}
	ref := make(map[string]any, len(iss.NativeRef)+2)
	for k, v := range iss.NativeRef {
		ref[k] = v
	}
	if parent != nil && (strings.TrimSpace(parent.ID) != "" || strings.TrimSpace(parent.Identifier) != "") {
		cp := *parent
		ref["parent"] = &cp
	}
	if len(children) > 0 {
		cp := make([]IssueRef, len(children))
		copy(cp, children)
		ref["children"] = cp
	}
	iss.NativeRef = ref
	return iss
}

// ExtractHierarchy extracts normalized parent and sub-issue references from iss.NativeRef.
func ExtractHierarchy(iss symphony.Issue) (*IssueRef, []IssueRef) {
	if len(iss.NativeRef) == 0 {
		return nil, nil
	}
	var parent *IssueRef
	if rawParent, ok := iss.NativeRef["parent"]; ok && rawParent != nil {
		if p, ok := decodeIssueRef(rawParent); ok {
			parent = &p
		}
	}
	var children []IssueRef
	if rawChildren, ok := iss.NativeRef["children"]; ok && rawChildren != nil {
		switch v := rawChildren.(type) {
		case []IssueRef:
			if len(v) > 0 {
				children = make([]IssueRef, len(v))
				copy(children, v)
			}
		case []any:
			for _, item := range v {
				if c, ok := decodeIssueRef(item); ok {
					children = append(children, c)
				}
			}
		default:
			if b, err := json.Marshal(rawChildren); err == nil {
				var parsed []IssueRef
				if json.Unmarshal(b, &parsed) == nil && len(parsed) > 0 {
					children = parsed
				}
			}
		}
	}
	return parent, children
}

func decodeIssueRef(raw any) (IssueRef, bool) {
	switch v := raw.(type) {
	case IssueRef:
		if strings.TrimSpace(v.ID) == "" && strings.TrimSpace(v.Identifier) == "" {
			return IssueRef{}, false
		}
		return v, true
	case *IssueRef:
		if v == nil || (strings.TrimSpace(v.ID) == "" && strings.TrimSpace(v.Identifier) == "") {
			return IssueRef{}, false
		}
		return *v, true
	case map[string]any:
		id, _ := v["id"].(string)
		ident, _ := v["identifier"].(string)
		title, _ := v["title"].(string)
		state, _ := v["state"].(string)
		if stMap, ok := v["state"].(map[string]any); ok && state == "" {
			state, _ = stMap["name"].(string)
		}
		ref := IssueRef{
			ID:         strings.TrimSpace(id),
			Identifier: strings.TrimSpace(ident),
			Title:      strings.TrimSpace(title),
			State:      strings.TrimSpace(state),
		}
		if ref.ID == "" && ref.Identifier == "" {
			return IssueRef{}, false
		}
		return ref, true
	default:
		b, err := json.Marshal(raw)
		if err != nil {
			return IssueRef{}, false
		}
		var ref IssueRef
		if err := json.Unmarshal(b, &ref); err != nil {
			return IssueRef{}, false
		}
		if strings.TrimSpace(ref.ID) == "" && strings.TrimSpace(ref.Identifier) == "" {
			return IssueRef{}, false
		}
		return ref, true
	}
}
