package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// TokenTestIdentity is the RED identity of a declared test outside Go (the
// script and runner shapes, FAE-WF-036, FAE-WF-076): the token stream of the
// declaration that names the test, so whitespace, indentation width, comments
// and other tests added to the file leave it unchanged while any change to the
// declared test's own code changes it. The declaration is the brace-balanced
// block opened on (or just after) the line naming the test, else the indented
// block after a line ending in ':', else that line alone.
func TokenTestIdentity(source []byte, id string) (string, error) {
	region, err := declaredTest(string(source), id)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join(tokens(region), "\x00")))
	return "tokens:" + hex.EncodeToString(sum[:]), nil
}

// CheckFrozenTokens refuses a change to the frozen declared test's tokens.
func CheckFrozenTokens(frozen, current []byte, id string) error {
	a, err := TokenTestIdentity(frozen, id)
	if err != nil {
		return err
	}
	b, err := TokenTestIdentity(current, id)
	if err != nil {
		return err
	}
	if a != b {
		return errors.New("frozen RED test " + id + " changed")
	}
	return nil
}

func declaredTest(source, id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", errors.New("no declared test identifier")
	}
	lines := strings.Split(source, "\n")
	at := -1
	for i, line := range lines {
		if wordIndex(line, id) >= 0 {
			at = i
			break
		}
	}
	if at < 0 {
		return "", errors.New("declared test identifier absent from the test file")
	}
	head := lines[at]
	braced := strings.IndexByte(head[wordIndex(head, id):], '{') >= 0
	for _, line := range lines[at+1:] {
		if strings.TrimSpace(line) != "" {
			// A formatter may move the opening brace to its own line.
			braced = braced || strings.HasPrefix(strings.TrimSpace(line), "{")
			break
		}
	}
	if braced {
		rest := strings.Join(lines[at:], "\n")
		depth := 0
		for i, r := range rest {
			switch r {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					return rest[:i+1], nil
				}
			}
		}
		return "", errors.New("unbalanced declaration of " + id)
	}
	if !strings.HasSuffix(strings.TrimSpace(stripComment(head)), ":") {
		return head, nil
	}
	indent := func(s string) int {
		return len(strings.ReplaceAll(s, "\t", "        ")) - len(strings.TrimLeft(strings.ReplaceAll(s, "\t", "        "), " "))
	}
	block := []string{head}
	for _, line := range lines[at+1:] {
		if strings.TrimSpace(line) != "" && indent(line) <= indent(head) {
			break
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n"), nil
}

// wordIndex finds id as a whole identifier in line.
func wordIndex(line, id string) int {
	word := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	for from := 0; ; {
		i := strings.Index(line[from:], id)
		if i < 0 {
			return -1
		}
		i += from
		if (i == 0 || !word(line[i-1])) && (i+len(id) == len(line) || !word(line[i+len(id)])) {
			return i
		}
		from = i + 1
	}
}

func stripComment(line string) string {
	for _, marker := range []string{"#", "//"} {
		if i := strings.Index(line, marker); i >= 0 {
			line = line[:i]
		}
	}
	return line
}

// tokens splits code into words, quoted strings and punctuation, dropping
// whitespace and line comments.
func tokens(code string) []string {
	out := []string{}
	word := func(r byte) bool {
		return r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	}
	for i := 0; i < len(code); {
		c := code[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#' || c == '/' && i+1 < len(code) && code[i+1] == '/':
			for i < len(code) && code[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(code) && code[j] != c {
				if code[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(code))
			out = append(out, code[i:j])
			i = j
		case word(c):
			j := i
			for j < len(code) && word(code[j]) {
				j++
			}
			out = append(out, code[i:j])
			i = j
		default:
			out = append(out, string(c))
			i++
		}
	}
	return out
}
