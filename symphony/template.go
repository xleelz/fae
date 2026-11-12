package symphony

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultEmptyWorkflowPrompt is the fallback prompt when WORKFLOW.md has an empty body (Symphony §5.4).
const DefaultEmptyWorkflowPrompt = "You are working on an issue from the configured tracker."

// RenderPrompt renders a strict Liquid-compatible prompt template for the given issue and attempt (Symphony §§5.4, 12).
func RenderPrompt(promptTemplate string, issue Issue, attempt *int) (string, error) {
	if strings.TrimSpace(promptTemplate) == "" {
		return DefaultEmptyWorkflowPrompt, nil
	}

	nodes, err := parseTemplateNodes(promptTemplate)
	if err != nil {
		return "", err
	}

	ctx := newRenderContext(issue, attempt)
	var out strings.Builder
	if err := evalNodes(nodes, ctx, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

// ValidatePromptTemplate checks that promptTemplate has valid template syntax.
func ValidatePromptTemplate(promptTemplate string) error {
	if strings.TrimSpace(promptTemplate) == "" {
		return nil
	}
	_, err := parseTemplateNodes(promptTemplate)
	return err
}

// RenderContinuationPrompt builds concise continuation guidance for subsequent turns on
// the same live coding-agent thread without resending the full initial prompt (Symphony §§7.1, 10.2).
func RenderContinuationPrompt(issue Issue, attempt *int, turnNumber, maxTurns int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Continue working on issue %s", issue.Identifier)
	if strings.TrimSpace(issue.Title) != "" {
		fmt.Fprintf(&sb, " (%s)", issue.Title)
	}
	sb.WriteString(".")
	if strings.TrimSpace(issue.State) != "" {
		fmt.Fprintf(&sb, " Current tracker state: %s.", issue.State)
	}
	if turnNumber > 0 && maxTurns > 0 {
		fmt.Fprintf(&sb, " This is continuation turn %d of %d on the current agent thread.", turnNumber, maxTurns)
	} else if turnNumber > 0 {
		fmt.Fprintf(&sb, " This is continuation turn %d on the current agent thread.", turnNumber)
	}
	if attempt != nil {
		fmt.Fprintf(&sb, " Retry/attempt: %d.", *attempt)
	}
	sb.WriteString(" Inspect your current progress in the workspace, complete any remaining work or verification, and stop once the issue reaches a terminal state or no further action is needed.")
	return sb.String()
}

// strictObject wraps a map of known fields so unknown property lookups fail with ErrTemplateRenderError.
type strictObject struct {
	typeName string
	fields   map[string]any
}

type renderContext struct {
	vars map[string]any
}

func newRenderContext(issue Issue, attempt *int) *renderContext {
	var desc any
	if issue.Description != nil {
		desc = *issue.Description
	}
	var prio any
	if issue.Priority != nil {
		prio = *issue.Priority
	}
	var branch any
	if issue.BranchName != nil {
		branch = *issue.BranchName
	}
	var url any
	if issue.URL != nil {
		url = *issue.URL
	}
	var assignee any
	if issue.AssigneeID != nil {
		assignee = *issue.AssigneeID
	}
	var createdAt any
	if issue.CreatedAt != nil {
		createdAt = issue.CreatedAt.UTC().Format(time.RFC3339)
	}
	var updatedAt any
	if issue.UpdatedAt != nil {
		updatedAt = issue.UpdatedAt.UTC().Format(time.RFC3339)
	}

	labels := make([]any, len(issue.Labels))
	for i, l := range issue.Labels {
		labels[i] = l
	}

	blockedBy := make([]any, len(issue.BlockedBy))
	for i, b := range issue.BlockedBy {
		var bid, bident, bstate any
		if b.ID != nil {
			bid = *b.ID
		}
		if b.Identifier != nil {
			bident = *b.Identifier
		}
		if b.State != nil {
			bstate = *b.State
		}
		blockedBy[i] = strictObject{
			typeName: "BlockerRef",
			fields: map[string]any{
				"id":         bid,
				"identifier": bident,
				"state":      bstate,
			},
		}
	}

	nativeRef := make(map[string]any, len(issue.NativeRef))
	for k, v := range issue.NativeRef {
		nativeRef[k] = v
	}

	parentVal, childrenVal := extractHierarchyObjects(issue.NativeRef)

	issueObj := strictObject{
		typeName: "Issue",
		fields: map[string]any{
			"id":           issue.ID,
			"native_ref":   nativeRef,
			"identifier":   issue.Identifier,
			"title":        issue.Title,
			"description":  desc,
			"priority":     prio,
			"state":        issue.State,
			"branch_name":  branch,
			"url":          url,
			"assignee_id":  assignee,
			"labels":       labels,
			"blocked_by":   blockedBy,
			"parent":       parentVal,
			"children":     childrenVal,
			"dispatchable": issue.Dispatchable,
			"created_at":   createdAt,
			"updated_at":   updatedAt,
		},
	}

	var attemptVal any
	if attempt != nil {
		attemptVal = *attempt
	}

	return &renderContext{
		vars: map[string]any{
			"issue":   issueObj,
			"attempt": attemptVal,
		},
	}
}

func extractHierarchyObjects(nativeRef map[string]any) (any, []any) {
	children := []any{}
	if len(nativeRef) == 0 {
		return nil, children
	}
	var parent any
	if rawParent, ok := nativeRef["parent"]; ok && rawParent != nil {
		if obj, ok := toIssueRefStrictObject(rawParent); ok {
			parent = obj
		}
	}
	if rawChildren, ok := nativeRef["children"]; ok && rawChildren != nil {
		switch v := rawChildren.(type) {
		case []any:
			for _, item := range v {
				if obj, ok := toIssueRefStrictObject(item); ok {
					children = append(children, obj)
				}
			}
		default:
			if b, err := json.Marshal(rawChildren); err == nil {
				var arr []any
				if json.Unmarshal(b, &arr) == nil {
					for _, item := range arr {
						if obj, ok := toIssueRefStrictObject(item); ok {
							children = append(children, obj)
						}
					}
				}
			}
		}
	}
	return parent, children
}

func toIssueRefStrictObject(raw any) (strictObject, bool) {
	if raw == nil {
		return strictObject{}, false
	}
	if so, ok := raw.(strictObject); ok {
		return so, true
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return strictObject{}, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return strictObject{}, false
	}
	id, _ := m["id"].(string)
	ident, _ := m["identifier"].(string)
	title, _ := m["title"].(string)
	state, _ := m["state"].(string)
	if stMap, ok := m["state"].(map[string]any); ok && state == "" {
		state, _ = stMap["name"].(string)
	}
	id = strings.TrimSpace(id)
	ident = strings.TrimSpace(ident)
	title = strings.TrimSpace(title)
	state = strings.TrimSpace(state)
	if id == "" && ident == "" {
		return strictObject{}, false
	}
	return strictObject{
		typeName: "IssueRef",
		fields: map[string]any{
			"id":         id,
			"identifier": ident,
			"title":      title,
			"state":      state,
		},
	}, true
}

func (c *renderContext) cloneWith(k string, v any) *renderContext {
	next := make(map[string]any, len(c.vars)+1)
	for ek, ev := range c.vars {
		next[ek] = ev
	}
	next[k] = v
	return &renderContext{vars: next}
}

// Template AST nodes
type templateNode interface {
	isTemplateNode()
}

type textNode struct {
	text string
}

func (textNode) isTemplateNode() {}

type outputNode struct {
	expr string
}

func (outputNode) isTemplateNode() {}

type ifBranch struct {
	cond  string
	nodes []templateNode
}

type ifNode struct {
	branches []ifBranch
	elseBody []templateNode
}

func (ifNode) isTemplateNode() {}

type forNode struct {
	varName  string
	listExpr string
	body     []templateNode
	elseBody []templateNode
}

func (forNode) isTemplateNode() {}

type tokenKind int

const (
	tokText tokenKind = iota
	tokOutput
	tokTag
)

type templateToken struct {
	kind      tokenKind
	content   string
	trimLeft  bool
	trimRight bool
}

func tokenizeTemplate(src string) ([]templateToken, error) {
	var tokens []templateToken
	pos := 0
	n := len(src)

	for pos < n {
		nextOut := strings.Index(src[pos:], "{{")
		nextTag := strings.Index(src[pos:], "{%")

		// Check for unmatched closing delimiters in plain text
		nextCloseOut := strings.Index(src[pos:], "}}")
		nextCloseTag := strings.Index(src[pos:], "%}")

		nextDelim := -1
		isOut := false
		if nextOut != -1 && (nextTag == -1 || nextOut < nextTag) {
			nextDelim = pos + nextOut
			isOut = true
		} else if nextTag != -1 {
			nextDelim = pos + nextTag
			isOut = false
		}

		if nextCloseOut != -1 && (nextDelim == -1 || pos+nextCloseOut < nextDelim) {
			return nil, fmt.Errorf("%w: unexpected closing '}}'", ErrTemplateParseError)
		}
		if nextCloseTag != -1 && (nextDelim == -1 || pos+nextCloseTag < nextDelim) {
			return nil, fmt.Errorf("%w: unexpected closing '%%}'", ErrTemplateParseError)
		}

		if nextDelim == -1 {
			tokens = append(tokens, templateToken{kind: tokText, content: src[pos:]})
			break
		}

		if nextDelim > pos {
			tokens = append(tokens, templateToken{kind: tokText, content: src[pos:nextDelim]})
		}

		if isOut {
			closeIdx := findClosingDelimiter(src[nextDelim+2:], "}}")
			if closeIdx == -1 {
				return nil, fmt.Errorf("%w: unclosed '{{' tag", ErrTemplateParseError)
			}
			inner := src[nextDelim+2 : nextDelim+2+closeIdx]
			trimLeft := strings.HasPrefix(inner, "-")
			trimRight := strings.HasSuffix(inner, "-")
			if trimLeft {
				inner = inner[1:]
			}
			if trimRight && len(inner) > 0 {
				inner = inner[:len(inner)-1]
			}
			inner = strings.TrimSpace(inner)
			if inner == "" {
				return nil, fmt.Errorf("%w: empty '{{ }}' expression", ErrTemplateParseError)
			}
			tokens = append(tokens, templateToken{
				kind:      tokOutput,
				content:   inner,
				trimLeft:  trimLeft,
				trimRight: trimRight,
			})
			pos = nextDelim + 2 + closeIdx + 2
		} else {
			closeIdx := findClosingDelimiter(src[nextDelim+2:], "%}")
			if closeIdx == -1 {
				return nil, fmt.Errorf("%w: unclosed '{%%' tag", ErrTemplateParseError)
			}
			inner := src[nextDelim+2 : nextDelim+2+closeIdx]
			trimLeft := strings.HasPrefix(inner, "-")
			trimRight := strings.HasSuffix(inner, "-")
			if trimLeft {
				inner = inner[1:]
			}
			if trimRight && len(inner) > 0 {
				inner = inner[:len(inner)-1]
			}
			inner = strings.TrimSpace(inner)
			if inner == "" {
				return nil, fmt.Errorf("%w: empty '{%% %%}' tag", ErrTemplateParseError)
			}
			tokens = append(tokens, templateToken{
				kind:      tokTag,
				content:   inner,
				trimLeft:  trimLeft,
				trimRight: trimRight,
			})
			pos = nextDelim + 2 + closeIdx + 2
		}
	}

	// Apply trimLeft / trimRight on adjacent text tokens
	for i := 0; i < len(tokens); i++ {
		if tokens[i].kind == tokOutput || tokens[i].kind == tokTag {
			if tokens[i].trimLeft && i > 0 && tokens[i-1].kind == tokText {
				tokens[i-1].content = strings.TrimRight(tokens[i-1].content, " \t\r\n")
			}
			if tokens[i].trimRight && i+1 < len(tokens) && tokens[i+1].kind == tokText {
				tokens[i+1].content = strings.TrimLeft(tokens[i+1].content, " \t\r\n")
			}
		}
	}

	return tokens, nil
}

func findClosingDelimiter(s string, delim string) int {
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i <= len(s)-len(delim); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if c == '"' {
			inDouble = true
			continue
		}
		if c == '\'' {
			inSingle = true
			continue
		}
		if s[i:i+len(delim)] == delim {
			return i
		}
	}
	return -1
}

func parseTemplateNodes(src string) ([]templateNode, error) {
	tokens, err := tokenizeTemplate(src)
	if err != nil {
		return nil, err
	}
	nodes, nextIdx, stopTag, err := parseBlockTokens(tokens, 0)
	if err != nil {
		return nil, err
	}
	if stopTag != "" || nextIdx < len(tokens) {
		return nil, fmt.Errorf("%w: unexpected tag %q", ErrTemplateParseError, stopTag)
	}
	return nodes, nil
}

func parseBlockTokens(tokens []templateToken, startIdx int) ([]templateNode, int, string, error) {
	var nodes []templateNode
	idx := startIdx

	for idx < len(tokens) {
		tok := tokens[idx]
		switch tok.kind {
		case tokText:
			if tok.content != "" {
				nodes = append(nodes, textNode{text: tok.content})
			}
			idx++
		case tokOutput:
			nodes = append(nodes, outputNode{expr: tok.content})
			idx++
		case tokTag:
			tagName, tagRest := splitTagCommand(tok.content)
			switch tagName {
			case "endif", "endunless", "endfor", "else", "elsif":
				return nodes, idx, tok.content, nil
			case "if", "unless":
				if tagRest == "" {
					return nil, idx, "", fmt.Errorf("%w: %s tag missing condition", ErrTemplateParseError, tagName)
				}
				node, nextIdx, err := parseIfTag(tokens, idx, tagName == "unless")
				if err != nil {
					return nil, idx, "", err
				}
				nodes = append(nodes, node)
				idx = nextIdx
			case "for":
				node, nextIdx, err := parseForTag(tokens, idx, tagRest)
				if err != nil {
					return nil, idx, "", err
				}
				nodes = append(nodes, node)
				idx = nextIdx
			default:
				return nil, idx, "", fmt.Errorf("%w: unknown control tag %q", ErrTemplateParseError, tagName)
			}
		}
	}
	return nodes, idx, "", nil
}

func parseIfTag(tokens []templateToken, startIdx int, isUnless bool) (ifNode, int, error) {
	_, firstCond := splitTagCommand(tokens[startIdx].content)
	if isUnless {
		firstCond = "not (" + firstCond + ")"
	}
	expectedEnd := "endif"
	if isUnless {
		expectedEnd = "endunless"
	}

	var branches []ifBranch
	var elseBody []templateNode
	curCond := firstCond
	inElse := false
	idx := startIdx + 1

	for {
		body, nextIdx, stopTag, err := parseBlockTokens(tokens, idx)
		if err != nil {
			return ifNode{}, idx, err
		}
		if stopTag == "" {
			return ifNode{}, nextIdx, fmt.Errorf("%w: unclosed %s block (missing %s)", ErrTemplateParseError, map[bool]string{false: "if", true: "unless"}[isUnless], expectedEnd)
		}

		if inElse {
			elseBody = body
		} else {
			branches = append(branches, ifBranch{cond: curCond, nodes: body})
		}

		stopName, stopRest := splitTagCommand(stopTag)
		idx = nextIdx + 1

		if stopName == expectedEnd {
			if stopRest != "" {
				return ifNode{}, idx, fmt.Errorf("%w: unexpected arguments in %s", ErrTemplateParseError, expectedEnd)
			}
			return ifNode{branches: branches, elseBody: elseBody}, idx, nil
		}
		if stopName == "elsif" {
			if isUnless || inElse {
				return ifNode{}, idx, fmt.Errorf("%w: unexpected elsif in block", ErrTemplateParseError)
			}
			if stopRest == "" {
				return ifNode{}, idx, fmt.Errorf("%w: elsif missing condition", ErrTemplateParseError)
			}
			curCond = stopRest
			continue
		}
		if stopName == "else" {
			if inElse || stopRest != "" {
				return ifNode{}, idx, fmt.Errorf("%w: invalid else tag", ErrTemplateParseError)
			}
			inElse = true
			continue
		}
		return ifNode{}, idx, fmt.Errorf("%w: mismatched block tag %q, expected %s", ErrTemplateParseError, stopName, expectedEnd)
	}
}

func parseForTag(tokens []templateToken, startIdx int, header string) (forNode, int, error) {
	// Expected format: "<ident> in <expr>"
	parts := strings.Fields(header)
	if len(parts) < 3 || parts[1] != "in" {
		return forNode{}, startIdx, fmt.Errorf("%w: invalid for tag syntax %q, expected 'for <var> in <expr>'", ErrTemplateParseError, header)
	}
	varName := parts[0]
	if !isValidIdentifier(varName) {
		return forNode{}, startIdx, fmt.Errorf("%w: invalid loop variable %q", ErrTemplateParseError, varName)
	}
	inIdx := strings.Index(header, " in ")
	if inIdx == -1 {
		return forNode{}, startIdx, fmt.Errorf("%w: invalid for tag syntax %q", ErrTemplateParseError, header)
	}
	listExpr := strings.TrimSpace(header[inIdx+4:])
	if listExpr == "" {
		return forNode{}, startIdx, fmt.Errorf("%w: missing collection expression in for tag", ErrTemplateParseError)
	}

	idx := startIdx + 1
	var loopBody []templateNode
	var elseBody []templateNode
	inElse := false

	for {
		body, nextIdx, stopTag, err := parseBlockTokens(tokens, idx)
		if err != nil {
			return forNode{}, idx, err
		}
		if stopTag == "" {
			return forNode{}, nextIdx, fmt.Errorf("%w: unclosed for block (missing endfor)", ErrTemplateParseError)
		}
		if inElse {
			elseBody = body
		} else {
			loopBody = body
		}

		stopName, stopRest := splitTagCommand(stopTag)
		idx = nextIdx + 1

		if stopName == "endfor" {
			if stopRest != "" {
				return forNode{}, idx, fmt.Errorf("%w: unexpected arguments in endfor", ErrTemplateParseError)
			}
			return forNode{
				varName:  varName,
				listExpr: listExpr,
				body:     loopBody,
				elseBody: elseBody,
			}, idx, nil
		}
		if stopName == "else" {
			if inElse || stopRest != "" {
				return forNode{}, idx, fmt.Errorf("%w: invalid else in for block", ErrTemplateParseError)
			}
			inElse = true
			continue
		}
		return forNode{}, idx, fmt.Errorf("%w: mismatched tag %q inside for block", ErrTemplateParseError, stopName)
	}
}

func splitTagCommand(tag string) (string, string) {
	tag = strings.TrimSpace(tag)
	for i := 0; i < len(tag); i++ {
		if tag[i] == ' ' || tag[i] == '\t' || tag[i] == '\n' || tag[i] == '\r' {
			return tag[:i], strings.TrimSpace(tag[i+1:])
		}
	}
	return tag, ""
}

func isValidIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func evalNodes(nodes []templateNode, ctx *renderContext, out *strings.Builder) error {
	for _, node := range nodes {
		switch n := node.(type) {
		case textNode:
			out.WriteString(n.text)
		case outputNode:
			val, err := evalPipeline(n.expr, ctx)
			if err != nil {
				return err
			}
			out.WriteString(formatTemplateValue(val))
		case ifNode:
			matched := false
			for _, b := range n.branches {
				ok, err := evalCondition(b.cond, ctx)
				if err != nil {
					return err
				}
				if ok {
					matched = true
					if err := evalNodes(b.nodes, ctx, out); err != nil {
						return err
					}
					break
				}
			}
			if !matched && len(n.elseBody) > 0 {
				if err := evalNodes(n.elseBody, ctx, out); err != nil {
					return err
				}
			}
		case forNode:
			val, err := evalPipeline(n.listExpr, ctx)
			if err != nil {
				return err
			}
			items, err := toTemplateList(val)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				if len(n.elseBody) > 0 {
					if err := evalNodes(n.elseBody, ctx, out); err != nil {
						return err
					}
				}
				continue
			}
			total := len(items)
			for i, item := range items {
				loopCtx := ctx.cloneWith(n.varName, item)
				loopCtx.vars["forloop"] = strictObject{
					typeName: "forloop",
					fields: map[string]any{
						"index":   i + 1,
						"index0":  i,
						"first":   i == 0,
						"last":    i == total-1,
						"length":  total,
						"rindex":  total - i,
						"rindex0": total - i - 1,
					},
				}
				if err := evalNodes(n.body, loopCtx, out); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func evalPipeline(expr string, ctx *renderContext) (any, error) {
	parts, err := splitPipeSegments(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemplateParseError, err)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: empty expression", ErrTemplateParseError)
	}

	val, err := evalAtom(strings.TrimSpace(parts[0]), ctx)
	if err != nil {
		return nil, err
	}

	for _, filterSpec := range parts[1:] {
		val, err = applyFilter(val, strings.TrimSpace(filterSpec), ctx)
		if err != nil {
			return nil, err
		}
	}
	return val, nil
}

func splitPipeSegments(expr string) ([]string, error) {
	var parts []string
	inSingle := false
	inDouble := false
	escaped := false
	start := 0

	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '|':
			if i+1 < len(expr) && expr[i+1] == '|' {
				i++
				continue
			}
			parts = append(parts, expr[start:i])
			start = i + 1
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unclosed string literal in %q", expr)
	}
	parts = append(parts, expr[start:])
	return parts, nil
}

func evalAtom(atom string, ctx *renderContext) (any, error) {
	atom = strings.TrimSpace(atom)
	if atom == "" {
		return nil, fmt.Errorf("%w: empty expression", ErrTemplateRenderError)
	}

	// String literal
	if (strings.HasPrefix(atom, "\"") && strings.HasSuffix(atom, "\"")) ||
		(strings.HasPrefix(atom, "'") && strings.HasSuffix(atom, "'")) {
		if len(atom) < 2 {
			return nil, fmt.Errorf("%w: invalid string literal %s", ErrTemplateParseError, atom)
		}
		if atom[0] == '"' {
			unq, err := strconv.Unquote(atom)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid string literal %s: %v", ErrTemplateParseError, atom, err)
			}
			return unq, nil
		}
		return atom[1 : len(atom)-1], nil
	}

	// Literals
	switch atom {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "nil", "null", "empty", "blank":
		return nil, nil
	}

	// Numeric literal
	if n, err := strconv.Atoi(atom); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(atom, 64); err == nil {
		return f, nil
	}

	// Variable / dotted path lookup
	segments := strings.Split(atom, ".")
	rootName := strings.TrimSpace(segments[0])
	if !isValidIdentifier(rootName) {
		return nil, fmt.Errorf("%w: invalid expression %q", ErrTemplateRenderError, atom)
	}

	cur, exists := ctx.vars[rootName]
	if !exists {
		return nil, fmt.Errorf("%w: unknown variable %q", ErrTemplateRenderError, rootName)
	}

	for _, seg := range segments[1:] {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil, fmt.Errorf("%w: invalid empty property in %q", ErrTemplateRenderError, atom)
		}
		switch obj := cur.(type) {
		case strictObject:
			nextVal, ok := obj.fields[seg]
			if !ok {
				return nil, fmt.Errorf("%w: unknown property %q on %s", ErrTemplateRenderError, seg, obj.typeName)
			}
			cur = nextVal
		case map[string]any:
			nextVal, ok := obj[seg]
			if !ok {
				return nil, fmt.Errorf("%w: unknown key %q in map (%s)", ErrTemplateRenderError, seg, atom)
			}
			cur = nextVal
		case []any:
			if seg == "size" || seg == "length" {
				cur = len(obj)
			} else if seg == "first" {
				if len(obj) > 0 {
					cur = obj[0]
				} else {
					cur = nil
				}
			} else if seg == "last" {
				if len(obj) > 0 {
					cur = obj[len(obj)-1]
				} else {
					cur = nil
				}
			} else {
				return nil, fmt.Errorf("%w: unknown property %q on list", ErrTemplateRenderError, seg)
			}
		default:
			return nil, fmt.Errorf("%w: cannot access property %q on %v (%s)", ErrTemplateRenderError, seg, cur, atom)
		}
	}

	return cur, nil
}

func applyFilter(val any, filterSpec string, ctx *renderContext) (any, error) {
	if filterSpec == "" {
		return nil, fmt.Errorf("%w: empty filter name", ErrTemplateRenderError)
	}

	filterName := filterSpec
	filterArgRaw := ""
	hasArg := false
	if colonIdx := strings.IndexByte(filterSpec, ':'); colonIdx != -1 {
		filterName = strings.TrimSpace(filterSpec[:colonIdx])
		filterArgRaw = strings.TrimSpace(filterSpec[colonIdx+1:])
		hasArg = true
	}

	switch filterName {
	case "downcase":
		return strings.ToLower(formatTemplateValue(val)), nil
	case "upcase":
		return strings.ToUpper(formatTemplateValue(val)), nil
	case "strip", "trim":
		return strings.TrimSpace(formatTemplateValue(val)), nil
	case "size", "length":
		switch tv := val.(type) {
		case nil:
			return 0, nil
		case string:
			return len(tv), nil
		case []any:
			return len(tv), nil
		case []string:
			return len(tv), nil
		case map[string]any:
			return len(tv), nil
		default:
			return len(formatTemplateValue(val)), nil
		}
	case "default":
		if !hasArg {
			return nil, fmt.Errorf("%w: filter 'default' requires an argument", ErrTemplateRenderError)
		}
		argVal, err := evalAtom(filterArgRaw, ctx)
		if err != nil {
			return nil, err
		}
		if isEmptyForDefault(val) {
			return argVal, nil
		}
		return val, nil
	case "join":
		sep := " "
		if hasArg {
			argVal, err := evalAtom(filterArgRaw, ctx)
			if err != nil {
				return nil, err
			}
			sep = formatTemplateValue(argVal)
		}
		if val == nil {
			return "", nil
		}
		items, err := toTemplateList(val)
		if err != nil {
			return formatTemplateValue(val), nil
		}
		strs := make([]string, len(items))
		for i, item := range items {
			strs[i] = formatTemplateValue(item)
		}
		return strings.Join(strs, sep), nil
	default:
		return nil, fmt.Errorf("%w: unknown filter %q", ErrTemplateRenderError, filterName)
	}
}

func isEmptyForDefault(val any) bool {
	if val == nil {
		return true
	}
	switch tv := val.(type) {
	case bool:
		return !tv
	case string:
		return tv == ""
	case []any:
		return len(tv) == 0
	case []string:
		return len(tv) == 0
	case map[string]any:
		return len(tv) == 0
	default:
		return false
	}
}

func evalCondition(cond string, ctx *renderContext) (bool, error) {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return false, fmt.Errorf("%w: empty condition", ErrTemplateParseError)
	}

	// Strip outer parentheses if balanced
	for strings.HasPrefix(cond, "(") && strings.HasSuffix(cond, ")") && isOuterParens(cond) {
		cond = strings.TrimSpace(cond[1 : len(cond)-1])
	}

	// Logical OR (' or ' / '||')
	if orParts := splitTopLevelLogical(cond, " or ", "||"); len(orParts) > 1 {
		for _, part := range orParts {
			ok, err := evalCondition(part, ctx)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}

	// Logical AND (' and ' / '&&')
	if andParts := splitTopLevelLogical(cond, " and ", "&&"); len(andParts) > 1 {
		for _, part := range andParts {
			ok, err := evalCondition(part, ctx)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}

	// Unary NOT
	if strings.HasPrefix(cond, "not ") || strings.HasPrefix(cond, "not(") {
		rest := strings.TrimSpace(cond[3:])
		ok, err := evalCondition(rest, ctx)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}
	if strings.HasPrefix(cond, "!") && !strings.HasPrefix(cond, "!=") {
		rest := strings.TrimSpace(cond[1:])
		ok, err := evalCondition(rest, ctx)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}

	// Comparison operators: ==, !=, <=, >=, <, >, contains
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">", " contains "} {
		if idx := findTopLevelOperator(cond, op); idx != -1 {
			lhsStr := strings.TrimSpace(cond[:idx])
			rhsStr := strings.TrimSpace(cond[idx+len(op):])
			lhsVal, err := evalPipeline(lhsStr, ctx)
			if err != nil {
				return false, err
			}
			rhsVal, err := evalPipeline(rhsStr, ctx)
			if err != nil {
				return false, err
			}
			return compareTemplateValues(lhsVal, rhsVal, strings.TrimSpace(op))
		}
	}

	val, err := evalPipeline(cond, ctx)
	if err != nil {
		return false, err
	}
	return isTruthy(val), nil
}

func isOuterParens(s string) bool {
	depth := 0
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == '\'' && !inDouble {
			inSingle = !inSingle
		}
		if inSingle || inDouble {
			continue
		}
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
			if depth == 0 && i < len(s)-1 {
				return false
			}
		}
	}
	return depth == 0
}

func splitTopLevelLogical(s string, wordOp string, symOp string) []string {
	var parts []string
	depth := 0
	inSingle := false
	inDouble := false
	start := 0

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == '\'' && !inDouble {
			inSingle = !inSingle
		}
		if inSingle || inDouble {
			continue
		}
		if c == '(' {
			depth++
			continue
		}
		if c == ')' && depth > 0 {
			depth--
			continue
		}
		if depth == 0 {
			if wordOp != "" && i+len(wordOp) <= len(s) && s[i:i+len(wordOp)] == wordOp {
				parts = append(parts, s[start:i])
				start = i + len(wordOp)
				i += len(wordOp) - 1
				continue
			}
			if symOp != "" && i+len(symOp) <= len(s) && s[i:i+len(symOp)] == symOp {
				parts = append(parts, s[start:i])
				start = i + len(symOp)
				i += len(symOp) - 1
				continue
			}
		}
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, s[start:])
	return parts
}

func findTopLevelOperator(s string, op string) int {
	depth := 0
	inSingle := false
	inDouble := false

	for i := 0; i <= len(s)-len(op); i++ {
		c := s[i]
		if c == '"' && !inSingle {
			inDouble = !inDouble
		} else if c == '\'' && !inDouble {
			inSingle = !inSingle
		}
		if inSingle || inDouble {
			continue
		}
		if c == '(' {
			depth++
			continue
		}
		if c == ')' && depth > 0 {
			depth--
			continue
		}
		if depth == 0 && s[i:i+len(op)] == op {
			// Avoid matching '<' inside '<=' or '>' inside '>='
			if (op == "<" || op == ">") && i+1 < len(s) && s[i+1] == '=' {
				continue
			}
			return i
		}
	}
	return -1
}

func compareTemplateValues(lhs, rhs any, op string) (bool, error) {
	switch op {
	case "contains":
		needle := formatTemplateValue(rhs)
		switch tv := lhs.(type) {
		case string:
			return strings.Contains(tv, needle), nil
		case []any:
			for _, item := range tv {
				if formatTemplateValue(item) == needle {
					return true, nil
				}
			}
			return false, nil
		case []string:
			for _, item := range tv {
				if item == needle {
					return true, nil
				}
			}
			return false, nil
		default:
			return false, nil
		}
	case "==":
		return valuesEqual(lhs, rhs), nil
	case "!=":
		return !valuesEqual(lhs, rhs), nil
	case "<", "<=", ">", ">=":
		lf, lok := toFloat(lhs)
		rf, rok := toFloat(rhs)
		if lok && rok {
			switch op {
			case "<":
				return lf < rf, nil
			case "<=":
				return lf <= rf, nil
			case ">":
				return lf > rf, nil
			case ">=":
				return lf >= rf, nil
			}
		}
		ls := formatTemplateValue(lhs)
		rs := formatTemplateValue(rhs)
		switch op {
		case "<":
			return ls < rs, nil
		case "<=":
			return ls <= rs, nil
		case ">":
			return ls > rs, nil
		case ">=":
			return ls >= rs, nil
		}
	}
	return false, fmt.Errorf("%w: unsupported operator %q", ErrTemplateRenderError, op)
}

func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	return formatTemplateValue(a) == formatTemplateValue(b)
}

func toFloat(v any) (float64, bool) {
	switch tv := v.(type) {
	case int:
		return float64(tv), true
	case int64:
		return float64(tv), true
	case float64:
		return tv, true
	default:
		return 0, false
	}
}

func isTruthy(val any) bool {
	if val == nil {
		return false
	}
	switch tv := val.(type) {
	case bool:
		return tv
	case string:
		return tv != ""
	case []any:
		return len(tv) > 0
	case []string:
		return len(tv) > 0
	case map[string]any:
		return len(tv) > 0
	default:
		return true
	}
}

func toTemplateList(val any) ([]any, error) {
	if val == nil {
		return nil, nil
	}
	switch tv := val.(type) {
	case []any:
		return tv, nil
	case []string:
		out := make([]any, len(tv))
		for i, s := range tv {
			out[i] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: expected iterable list in for loop, got %T", ErrTemplateRenderError, val)
	}
}

func formatTemplateValue(val any) string {
	if val == nil {
		return ""
	}
	switch tv := val.(type) {
	case string:
		return tv
	case bool:
		if tv {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(tv)
	case int64:
		return strconv.FormatInt(tv, 10)
	case float64:
		return strconv.FormatFloat(tv, 'f', -1, 64)
	case strictObject:
		if tv.typeName == "IssueRef" {
			ident, _ := tv.fields["identifier"].(string)
			if ident == "" {
				ident, _ = tv.fields["id"].(string)
			}
			title, _ := tv.fields["title"].(string)
			state, _ := tv.fields["state"].(string)
			out := ident
			if title != "" {
				out += " (" + title + ")"
			}
			if state != "" {
				out += " [" + state + "]"
			}
			return out
		}
		return fmt.Sprint(tv.fields)
	case []any:
		parts := make([]string, len(tv))
		for i, item := range tv {
			parts[i] = formatTemplateValue(item)
		}
		return strings.Join(parts, ", ")
	case []string:
		return strings.Join(tv, ", ")
	default:
		return fmt.Sprint(tv)
	}
}
