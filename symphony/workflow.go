package symphony

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ResolveWorkflowPath returns explicitPath when non-empty; otherwise it returns
// <cwd>/WORKFLOW.md (Symphony §5.1).
func ResolveWorkflowPath(explicitPath string) string {
	if explicitPath != "" {
		return explicitPath
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "WORKFLOW.md"
	}
	return filepath.Join(cwd, "WORKFLOW.md")
}

// LoadWorkflow reads and parses a WORKFLOW.md file from disk (Symphony §5.1 & §5.2).
func LoadWorkflow(explicitPath string) (*WorkflowDefinition, error) {
	path := ResolveWorkflowPath(explicitPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMissingWorkflowFile, err)
	}
	return ParseWorkflowBytes(raw, path)
}

// ParseWorkflowBytes parses raw WORKFLOW.md content into a WorkflowDefinition (Symphony §5.2 & §5.5).
func ParseWorkflowBytes(raw []byte, sourcePath string) (*WorkflowDefinition, error) {
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")

	firstNewline := strings.IndexByte(normalized, '\n')
	var firstLine, rest string
	if firstNewline == -1 {
		firstLine = normalized
		rest = ""
	} else {
		firstLine = normalized[:firstNewline]
		rest = normalized[firstNewline+1:]
	}

	if strings.TrimRight(firstLine, " \t") == "---" {
		if firstNewline == -1 {
			return nil, fmt.Errorf("%w: missing closing front matter delimiter", ErrWorkflowParseError)
		}
		lines := strings.Split(rest, "\n")
		closeIdx := -1
		for i, line := range lines {
			if strings.TrimRight(line, " \t") == "---" {
				closeIdx = i
				break
			}
		}
		if closeIdx == -1 {
			return nil, fmt.Errorf("%w: missing closing front matter delimiter", ErrWorkflowParseError)
		}

		frontMatter := strings.Join(lines[:closeIdx], "\n")
		promptBody := strings.Join(lines[closeIdx+1:], "\n")

		cfg, err := parseFrontMatterYAML(frontMatter)
		if err != nil {
			return nil, err
		}

		return &WorkflowDefinition{
			Path:           sourcePath,
			Config:         cfg,
			PromptTemplate: strings.TrimSpace(promptBody),
		}, nil
	}

	return &WorkflowDefinition{
		Path:           sourcePath,
		Config:         map[string]any{},
		PromptTemplate: strings.TrimSpace(normalized),
	}, nil
}

type yamlLine struct {
	lineNum int
	indent  int
	text    string
	raw     string
}

func parseFrontMatterYAML(src string) (map[string]any, error) {
	rawLines := strings.Split(src, "\n")
	lines := make([]yamlLine, 0, len(rawLines))
	hasEffectiveContent := false

	for i, raw := range rawLines {
		indent := 0
		for j := 0; j < len(raw); j++ {
			if raw[j] == ' ' {
				indent++
			} else if raw[j] == '\t' {
				if strings.TrimSpace(raw) != "" && !strings.HasPrefix(strings.TrimSpace(raw), "#") {
					return nil, fmt.Errorf("%w: line %d uses tab indentation", ErrWorkflowParseError, i+1)
				}
				indent++
			} else {
				break
			}
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			hasEffectiveContent = true
		}
		lines = append(lines, yamlLine{
			lineNum: i + 1,
			indent:  indent,
			text:    raw[indent:],
			raw:     raw,
		})
	}

	if !hasEffectiveContent {
		return map[string]any{}, nil
	}

	val, nextIdx, err := parseYAMLBlock(lines, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkflowParseError, err)
	}
	for nextIdx < len(lines) {
		t := strings.TrimSpace(stripYAMLComment(lines[nextIdx].text))
		if t != "" {
			return nil, fmt.Errorf("%w: unexpected trailing content on line %d", ErrWorkflowParseError, lines[nextIdx].lineNum)
		}
		nextIdx++
	}

	m, ok := val.(map[string]any)
	if !ok || m == nil {
		return nil, fmt.Errorf("%w: front matter decoded to %T", ErrWorkflowFrontMatterNotMap, val)
	}
	return m, nil
}

func skipBlankYAML(lines []yamlLine, idx int) int {
	for idx < len(lines) {
		t := strings.TrimSpace(stripYAMLComment(lines[idx].text))
		if t != "" {
			break
		}
		idx++
	}
	return idx
}

func parseYAMLBlock(lines []yamlLine, startIdx int, minIndent int) (any, int, error) {
	idx := skipBlankYAML(lines, startIdx)
	if idx >= len(lines) {
		return map[string]any{}, idx, nil
	}
	if lines[idx].indent < minIndent {
		return nil, idx, nil
	}

	baseIndent := lines[idx].indent
	firstText := strings.TrimSpace(stripYAMLComment(lines[idx].text))

	// Flow collection at block root
	if strings.HasPrefix(firstText, "{") || strings.HasPrefix(firstText, "[") {
		combined, nextIdx, err := collectFlowLines(lines, idx)
		if err != nil {
			return nil, idx, err
		}
		val, err := parseFlowValue(combined)
		if err != nil {
			return nil, idx, err
		}
		return val, nextIdx, nil
	}

	// Sequence block
	if firstText == "-" || strings.HasPrefix(firstText, "- ") {
		return parseYAMLSequence(lines, idx, baseIndent)
	}

	// Check if mapping
	_, _, isMap, err := splitYAMLKeyVal(firstText)
	if err != nil {
		return nil, idx, fmt.Errorf("line %d: %v", lines[idx].lineNum, err)
	}
	if isMap {
		return parseYAMLMapping(lines, idx, baseIndent)
	}

	// Top-level scalar (only valid if single item at root, which will then fail front-matter map check)
	if startIdx == 0 && baseIndent == 0 {
		scalar, err := parseYAMLScalar(firstText)
		if err != nil {
			return nil, idx, fmt.Errorf("line %d: %v", lines[idx].lineNum, err)
		}
		return scalar, idx + 1, nil
	}

	return nil, idx, fmt.Errorf("line %d: expected mapping key or sequence item", lines[idx].lineNum)
}

func parseYAMLSequence(lines []yamlLine, idx int, baseIndent int) ([]any, int, error) {
	var out []any
	for idx < len(lines) {
		idx = skipBlankYAML(lines, idx)
		if idx >= len(lines) {
			break
		}
		if lines[idx].indent < baseIndent {
			break
		}
		if lines[idx].indent > baseIndent {
			return nil, idx, fmt.Errorf("line %d: unexpected indentation in sequence", lines[idx].lineNum)
		}

		t := strings.TrimSpace(stripYAMLComment(lines[idx].text))
		if t != "-" && !strings.HasPrefix(t, "- ") {
			return nil, idx, fmt.Errorf("line %d: expected sequence item '-'", lines[idx].lineNum)
		}

		itemRest := ""
		if len(t) > 1 {
			itemRest = strings.TrimSpace(t[1:])
		}

		if itemRest == "" {
			nextIdx := skipBlankYAML(lines, idx+1)
			if nextIdx >= len(lines) || lines[nextIdx].indent <= baseIndent {
				out = append(out, nil)
				idx = nextIdx
				continue
			}
			child, afterChild, err := parseYAMLBlock(lines, nextIdx, baseIndent+1)
			if err != nil {
				return nil, idx, err
			}
			out = append(out, child)
			idx = afterChild
			continue
		}

		if isBlockScalarIndicator(itemRest) {
			strVal, nextIdx, err := parseBlockScalar(lines, idx+1, baseIndent, itemRest)
			if err != nil {
				return nil, idx, err
			}
			out = append(out, strVal)
			idx = nextIdx
			continue
		}

		if strings.HasPrefix(itemRest, "{") || strings.HasPrefix(itemRest, "[") {
			flowStr, nextIdx, err := collectFlowLinesFromText(lines, idx, itemRest)
			if err != nil {
				return nil, idx, err
			}
			val, err := parseFlowValue(flowStr)
			if err != nil {
				return nil, idx, fmt.Errorf("line %d: %v", lines[idx].lineNum, err)
			}
			out = append(out, val)
			idx = nextIdx
			continue
		}

		// Check if inline mapping inside sequence item: e.g. "- id: 1"
		k, v, isMap, err := splitYAMLKeyVal(itemRest)
		if err != nil {
			return nil, idx, fmt.Errorf("line %d: %v", lines[idx].lineNum, err)
		}
		if isMap {
			itemMap := make(map[string]any)
			val, nextIdx, err := resolveMapEntryValue(lines, idx, baseIndent+2, v)
			if err != nil {
				return nil, idx, err
			}
			itemMap[k] = val
			nextIdx = skipBlankYAML(lines, nextIdx)
			if nextIdx < len(lines) && lines[nextIdx].indent > baseIndent {
				restMapVal, afterMap, err := parseYAMLMapping(lines, nextIdx, lines[nextIdx].indent)
				if err != nil {
					return nil, nextIdx, err
				}
				for mk, mv := range restMapVal {
					itemMap[mk] = mv
				}
				nextIdx = afterMap
			}
			out = append(out, itemMap)
			idx = nextIdx
			continue
		}

		scalar, err := parseYAMLScalar(itemRest)
		if err != nil {
			return nil, idx, fmt.Errorf("line %d: %v", lines[idx].lineNum, err)
		}
		out = append(out, scalar)
		idx++
	}
	return out, idx, nil
}

func parseYAMLMapping(lines []yamlLine, idx int, baseIndent int) (map[string]any, int, error) {
	out := make(map[string]any)
	for idx < len(lines) {
		idx = skipBlankYAML(lines, idx)
		if idx >= len(lines) {
			break
		}
		if lines[idx].indent < baseIndent {
			break
		}
		if lines[idx].indent > baseIndent {
			return nil, idx, fmt.Errorf("line %d: unexpected indentation in mapping", lines[idx].lineNum)
		}

		t := strings.TrimSpace(stripYAMLComment(lines[idx].text))
		if t == "-" || strings.HasPrefix(t, "- ") {
			return nil, idx, fmt.Errorf("line %d: unexpected sequence item in mapping", lines[idx].lineNum)
		}

		key, valText, ok, err := splitYAMLKeyVal(t)
		if err != nil {
			return nil, idx, fmt.Errorf("line %d: %v", lines[idx].lineNum, err)
		}
		if !ok {
			return nil, idx, fmt.Errorf("line %d: invalid mapping entry %q", lines[idx].lineNum, t)
		}

		val, nextIdx, err := resolveMapEntryValue(lines, idx, baseIndent, valText)
		if err != nil {
			return nil, idx, err
		}
		out[key] = val
		idx = nextIdx
	}
	return out, idx, nil
}

func resolveMapEntryValue(lines []yamlLine, lineIdx int, keyIndent int, valText string) (any, int, error) {
	valText = strings.TrimSpace(valText)
	if valText == "" {
		nextIdx := skipBlankYAML(lines, lineIdx+1)
		if nextIdx >= len(lines) || lines[nextIdx].indent < keyIndent {
			return nil, nextIdx, nil
		}
		if lines[nextIdx].indent == keyIndent {
			nextClean := strings.TrimSpace(stripYAMLComment(lines[nextIdx].text))
			if nextClean == "-" || strings.HasPrefix(nextClean, "- ") {
				return parseYAMLSequence(lines, nextIdx, keyIndent)
			}
			return nil, nextIdx, nil
		}
		return parseYAMLBlock(lines, nextIdx, lines[nextIdx].indent)
	}

	if isBlockScalarIndicator(valText) {
		return parseBlockScalar(lines, lineIdx+1, keyIndent, valText)
	}

	if strings.HasPrefix(valText, "{") || strings.HasPrefix(valText, "[") {
		flowStr, nextIdx, err := collectFlowLinesFromText(lines, lineIdx, valText)
		if err != nil {
			return nil, lineIdx, err
		}
		val, err := parseFlowValue(flowStr)
		if err != nil {
			return nil, lineIdx, fmt.Errorf("line %d: %v", lines[lineIdx].lineNum, err)
		}
		return val, nextIdx, nil
	}

	scalar, err := parseYAMLScalar(valText)
	if err != nil {
		return nil, lineIdx, fmt.Errorf("line %d: %v", lines[lineIdx].lineNum, err)
	}
	return scalar, lineIdx + 1, nil
}

func isBlockScalarIndicator(s string) bool {
	switch s {
	case "|", "|-", "|+", ">", ">-", ">+":
		return true
	default:
		return false
	}
}

func parseBlockScalar(lines []yamlLine, startIdx int, parentIndent int, indicator string) (string, int, error) {
	folded := strings.HasPrefix(indicator, ">")
	chomp := "clip"
	if strings.HasSuffix(indicator, "-") {
		chomp = "strip"
	} else if strings.HasSuffix(indicator, "+") {
		chomp = "keep"
	}

	idx := startIdx
	blockIndent := -1
	var rawBlock []yamlLine

	for idx < len(lines) {
		raw := lines[idx].raw
		if strings.TrimSpace(raw) == "" {
			rawBlock = append(rawBlock, lines[idx])
			idx++
			continue
		}
		if lines[idx].indent <= parentIndent {
			break
		}
		if blockIndent == -1 {
			blockIndent = lines[idx].indent
		}
		if lines[idx].indent < blockIndent {
			break
		}
		rawBlock = append(rawBlock, lines[idx])
		idx++
	}

	if blockIndent == -1 {
		return "", idx, nil
	}

	contentLines := make([]string, len(rawBlock))
	for i, l := range rawBlock {
		if len(l.raw) >= blockIndent {
			contentLines[i] = l.raw[blockIndent:]
		} else {
			contentLines[i] = ""
		}
	}

	// Count trailing blank lines
	trailingEmpty := 0
	for len(contentLines) > 0 && strings.TrimSpace(contentLines[len(contentLines)-1]) == "" {
		contentLines = contentLines[:len(contentLines)-1]
		trailingEmpty++
	}

	if len(contentLines) == 0 {
		if chomp == "keep" && trailingEmpty > 0 {
			return strings.Repeat("\n", trailingEmpty), idx, nil
		}
		return "", idx, nil
	}

	var result string
	if !folded {
		result = strings.Join(contentLines, "\n")
	} else {
		var sb strings.Builder
		for i, line := range contentLines {
			if i == 0 {
				sb.WriteString(line)
				continue
			}
			prev := contentLines[i-1]
			if strings.TrimSpace(prev) == "" || strings.TrimSpace(line) == "" || strings.HasPrefix(prev, " ") || strings.HasPrefix(line, " ") {
				sb.WriteByte('\n')
			} else {
				sb.WriteByte(' ')
			}
			sb.WriteString(line)
		}
		result = sb.String()
	}

	switch chomp {
	case "strip":
		// no trailing newline
	case "keep":
		result += "\n" + strings.Repeat("\n", trailingEmpty)
	default: // clip
		result += "\n"
	}

	return result, idx, nil
}

func collectFlowLines(lines []yamlLine, startIdx int) (string, int, error) {
	first := strings.TrimSpace(stripYAMLComment(lines[startIdx].text))
	return collectFlowLinesFromText(lines, startIdx, first)
}

func collectFlowLinesFromText(lines []yamlLine, startIdx int, initial string) (string, int, error) {
	var sb strings.Builder
	sb.WriteString(initial)
	idx := startIdx + 1

	for {
		cur := sb.String()
		depth, inQuote, err := checkFlowBalance(cur)
		if err != nil {
			return "", startIdx, fmt.Errorf("line %d: %v", lines[startIdx].lineNum, err)
		}
		if depth == 0 && !inQuote {
			return cur, idx, nil
		}
		if idx >= len(lines) {
			return "", startIdx, fmt.Errorf("line %d: unclosed flow collection or quote", lines[startIdx].lineNum)
		}
		nextPart := strings.TrimSpace(stripYAMLComment(lines[idx].text))
		if nextPart != "" {
			sb.WriteByte(' ')
			sb.WriteString(nextPart)
		}
		idx++
	}
}

func checkFlowBalance(s string) (int, bool, error) {
	var stack []byte
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(s); i++ {
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
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return 0, false, fmt.Errorf("mismatched closing bracket %q", c)
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack), inSingle || inDouble, nil
}

func parseFlowValue(s string) (any, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return nil, fmt.Errorf("invalid flow value %q", s)
	}
	if s[0] == '[' {
		if s[len(s)-1] != ']' {
			return nil, fmt.Errorf("unclosed flow sequence %q", s)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}, nil
		}
		parts, err := splitFlowItems(inner, ',')
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			val, err := parseFlowElement(part)
			if err != nil {
				return nil, err
			}
			out = append(out, val)
		}
		return out, nil
	}
	if s[0] == '{' {
		if s[len(s)-1] != '}' {
			return nil, fmt.Errorf("unclosed flow mapping %q", s)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return map[string]any{}, nil
		}
		parts, err := splitFlowItems(inner, ',')
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			k, vStr, ok, err := splitYAMLKeyVal(part)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("invalid flow mapping entry %q", part)
			}
			val, err := parseFlowElement(vStr)
			if err != nil {
				return nil, err
			}
			out[k] = val
		}
		return out, nil
	}
	return parseYAMLScalar(s)
}

func parseFlowElement(s string) (any, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		return parseFlowValue(s)
	}
	return parseYAMLScalar(s)
}

func splitFlowItems(s string, sep byte) ([]string, error) {
	var parts []string
	depth := 0
	inSingle := false
	inDouble := false
	escaped := false
	start := 0

	for i := 0; i < len(s); i++ {
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
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unexpected closing bracket in %q", s)
			}
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if inSingle || inDouble || depth != 0 {
		return nil, fmt.Errorf("unbalanced quotes or brackets in %q", s)
	}
	parts = append(parts, s[start:])
	return parts, nil
}

func stripYAMLComment(s string) string {
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(s); i++ {
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
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
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
		if c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}

func splitYAMLKeyVal(s string) (string, string, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false, nil
	}

	inSingle := false
	inDouble := false
	escaped := false
	depth := 0
	colonIdx := -1

	for i := 0; i < len(s); i++ {
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
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			if i == 0 {
				inDouble = true
			}
		case '\'':
			if i == 0 {
				inSingle = true
			}
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
				colonIdx = i
				break
			}
		}
		if colonIdx != -1 {
			break
		}
	}

	if inSingle || inDouble {
		return "", "", false, fmt.Errorf("unclosed quote in %q", s)
	}
	if colonIdx == -1 {
		return "", "", false, nil
	}

	rawKey := strings.TrimSpace(s[:colonIdx])
	if rawKey == "" {
		return "", "", false, fmt.Errorf("empty mapping key in %q", s)
	}
	key, err := unquoteYAMLKey(rawKey)
	if err != nil {
		return "", "", false, err
	}
	val := strings.TrimSpace(s[colonIdx+1:])
	return key, val, true, nil
}

func unquoteYAMLKey(rawKey string) (string, error) {
	if (strings.HasPrefix(rawKey, "\"") && strings.HasSuffix(rawKey, "\"")) ||
		(strings.HasPrefix(rawKey, "'") && strings.HasSuffix(rawKey, "'")) {
		v, err := parseYAMLScalar(rawKey)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(v), nil
	}
	return rawKey, nil
}

func parseYAMLScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "~" || s == "null" || s == "Null" || s == "NULL" {
		return nil, nil
	}

	if strings.HasPrefix(s, "\"") {
		if len(s) < 2 || !strings.HasSuffix(s, "\"") {
			return nil, fmt.Errorf("unclosed double-quoted string %s", s)
		}
		unq, err := strconv.Unquote(s)
		if err != nil {
			return nil, fmt.Errorf("invalid double-quoted string %s: %v", s, err)
		}
		return unq, nil
	}

	if strings.HasPrefix(s, "'") {
		if len(s) < 2 || !strings.HasSuffix(s, "'") {
			return nil, fmt.Errorf("unclosed single-quoted string %s", s)
		}
		inner := s[1 : len(s)-1]
		if strings.Contains(inner, "'") {
			// Ensure any single quotes inside are doubled ''
			cleaned := strings.ReplaceAll(inner, "''", "")
			if strings.Contains(cleaned, "'") {
				return nil, fmt.Errorf("unescaped single quote in %s", s)
			}
			inner = strings.ReplaceAll(inner, "''", "'")
		}
		return inner, nil
	}

	if strings.ContainsAny(s[:1], "[{") {
		return nil, fmt.Errorf("unclosed flow bracket in %s", s)
	}

	switch s {
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}

	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return int(i), nil
	}
	if strings.ContainsAny(s, ".eE") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, nil
		}
	}

	return s, nil
}

// ParseYAMLValue parses a literal configuration value with the same bounded,
// non-executable YAML subset used by workflow front matter. Wrapping it under
// a key preserves objects, sequences and scalar roots without a dependency.
func ParseYAMLValue(source string) (any, error) {
	lines := strings.Split(strings.TrimSpace(source), "\n")
	if len(lines) == 0 || strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("empty YAML input")
	}
	wrapped := "value:\n"
	for _, line := range lines {
		wrapped += "  " + line + "\n"
	}
	result, err := parseFrontMatterYAML(wrapped)
	if err != nil {
		return nil, err
	}
	return result["value"], nil
}
