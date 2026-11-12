package commandshim

import (
	"encoding/json"
	"github.com/xleelz/fae/workflow"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BypassesShim is the pre-tool predicate (FAE-HOST-011): a command line that
// names a shimmed tool by a path outside the session's wrapper directory, or
// that resets the PATH lookup the wrappers rely on, is denied. Each word is
// read as the shell reads it: quoting and escapes are removed, so
// /usr/bin/'g'o, /usr/bin/g\o and /usr/bin/$'\x67o' all name go, and brace
// lists and letter sequences are expanded, so {/usr/bin/go,build} names go
// too. PATH is reset by any word PATH or PATH= assignment; by env (named by
// any path) with an option that clears the environment, unsets PATH or sets a
// search path, in any spelling GNU or BSD env accepts, -S strings included;
// by exec -c or -l, command -p, BASH_ENV and a login or rc-file shell; and by
// sourcing a file (. or source as a command), whose contents the check cannot
// see. More literal bypasses are denied: a git or cargo dashed subcommand
// run as a command, by name or path, or named by path in git's exec directory
// (/usr/lib/git-core/git-push), which skips the git shim and its Local push
// refusal; a Go toolchain internal by path
// ($GOROOT/pkg/tool/linux_amd64/compile), which skips the go shim's clamp; a
// shimmed tool run as a command under another fixed name: a Go toolchain as
// go1.N or gotip, pytest as a Python module (python -m pytest) or under its
// other names (py.test, pytest-3, py.test-3) and rustup run, which puts a
// Rust toolchain's cargo, rustc and rustfmt ahead of their shims; and
// systemd-run in any position, which runs its command under the service
// manager with the manager's PATH, outside the session's process tree. It is
// a lexical check over shell words, deliberately over-inclusive; it is not a
// sandbox. Expansions it cannot evaluate (variables, globs, eval of a
// variable) pass, and so do other programs that run a command elsewhere with
// a PATH of their own (another multiplexer's server, cron and at, a user
// service, a container) and compilers that are not shimmed tools (gccgo,
// tinygo, gcc); commandshim/README.md lists what passes.
func BypassesShim(command, shimDir string) (string, bool) {
	const reset = "resetting PATH bypasses the role command shims"
	// A backslash-newline is a line continuation: the shell joins the halves.
	tokens, ok := shellTokens(strings.ReplaceAll(command, "\\\n", ""))
	if !ok {
		return "a brace expansion too large to check could name a shimmed tool by path", true
	}
	words := make([]string, len(tokens))
	for i, t := range tokens {
		words[i] = t.word
	}
	names := map[string]bool{}
	for _, n := range ShimNames {
		names[n] = true
	}
	for i, t := range tokens {
		w, rest, base := t.word, words[i+1:], filepath.Base(t.word)
		switch {
		case strings.HasPrefix(w, "PATH=") || w == "PATH" || strings.HasPrefix(w, "BASH_ENV=") || w == "BASH_ENV",
			base == "env" && envResetsPath(rest), w == "exec" && execResetsPath(rest), w == "command" && commandUsesDefaultPath(rest), shells[base] && shellReadsProfile(rest):
			return reset, true
		case t.command && (w == "." || w == "source"):
			return "a sourced file can reset PATH, which bypasses the role command shims; call the tool by name without sourcing", true
		case strings.Contains(w, "/") && names[base] && filepath.Clean(filepath.Dir(w)) != filepath.Clean(shimDir):
			return base + " by path " + t.raw + " bypasses the role command shim; call " + base + " by name", true
		case dashedSubcommand(base) && (t.command || strings.Contains(w, "/") && filepath.Base(filepath.Dir(w)) == "git-core"):
			tool, sub, _ := strings.Cut(base, "-")
			return t.raw + " runs " + tool + " " + sub + " without the " + tool + " shim; call " + tool + " " + sub + " instead", true
		case strings.Contains(w, "/") && toolchainInternal(w):
			return "Go toolchain internal " + t.raw + " by path bypasses the go shim; call go by name", true
		case t.command && goToolchainAlias(base):
			return t.raw + " runs a Go toolchain without the go shim and its clamp; call go by name", true
		case t.command && pythonInterpreter(base) && pythonRunsPytest(rest):
			return t.raw + " -m pytest runs pytest without the pytest shim; call pytest by name", true
		case t.command && InterpreterToolCommand(base, rest):
			return "interpreter build and test tools require a scoped HostCommand stage", true
		case t.command && pytestAlias(base):
			return t.raw + " runs pytest under another name without the pytest shim; call pytest by name", true
		case t.command && base == "rustup" && rustupRuns(rest):
			return "rustup run puts a Rust toolchain's cargo, rustc and rustfmt ahead of their role command shims; call the tool by name", true
		case base == "systemd-run":
			return "systemd-run runs its command under the service manager with the manager's PATH, outside the session's process tree and the role command shims", true
		}
	}
	return "", false
}

// goToolchainAliasName matches the names golang.org/dl installs for a Go
// release (go1.22.0, go1.23rc1) and for the development tree (gotip): each
// runs that toolchain's go by path.
var goToolchainAliasName = regexp.MustCompile(`^(go1(\.[0-9]+){1,2}((rc|beta)[0-9]+)?|gotip)$`)

// goToolchainAlias reports whether base names a Go toolchain under another
// name than go.
func goToolchainAlias(base string) bool { return goToolchainAliasName.MatchString(base) }

// pytestAliasName matches the other names pytest is installed under: its
// legacy entry point py.test and versioned names such as Debian's pytest-3
// and py.test-3. Each runs pytest itself.
var pytestAliasName = regexp.MustCompile(`^(py\.test|pytest-[0-9]+(\.[0-9]+)*|py\.test-[0-9]+(\.[0-9]+)*)$`)

// pytestAlias reports whether base names pytest under another name than pytest.
func pytestAlias(base string) bool { return pytestAliasName.MatchString(base) }

// pythonInterpreterName matches python, python3, python3.12, pypy3 and the like.
var pythonInterpreterName = regexp.MustCompile(`^(python|pypy)([0-9]+(\.[0-9]+)*)?$`)

// pythonInterpreter reports whether base names a Python interpreter.
func pythonInterpreter(base string) bool { return pythonInterpreterName.MatchString(base) }

// pythonRunsPytest reports whether a Python interpreter's options, up to its
// first operand, run pytest as a module: -m pytest, -mpytest or a cluster
// ending in m (-Bm pytest), naming pytest, py.test or a module inside pytest.
// A -c command string stops the scan: an interpreter that execs a tool itself
// is outside the check.
func pythonRunsPytest(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return false // the first operand: a script, or - for standard input
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'c':
				return false
			case 'W', 'X': // the value is the rest of the word, or the next word
				if j+1 == len(a) {
					i++
				}
				j = len(a)
			case 'm':
				module := a[j+1:]
				if module == "" && i+1 < len(args) {
					module = args[i+1]
				}
				top, _, _ := strings.Cut(module, ".")
				return top == "pytest" || top == "_pytest" || module == "py.test"
			}
		}
	}
	return false
}

// rustupRuns reports whether rustup's subcommand, after its options and a
// +toolchain, is run.
func rustupRuns(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			return a == "run"
		}
	}
	return false
}

// dashedSubcommand reports whether base names a git or cargo subcommand
// program (git-push, cargo-clippy): one or more lowercase letters, digits or
// dashes after the tool's dash, so a file name with an extension is none.
func dashedSubcommand(base string) bool {
	tool, sub, ok := strings.Cut(base, "-")
	return ok && (tool == "git" || tool == "cargo") && sub != "" && strings.Trim(sub, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}

// toolchainInternal reports whether w names a program in a Go toolchain's
// tool directory, .../pkg/tool/GOOS_GOARCH/NAME (compile, link, asm, vet).
func toolchainInternal(w string) bool {
	arch := filepath.Dir(w)
	tool := filepath.Dir(arch)
	return filepath.Base(tool) == "tool" && filepath.Base(filepath.Dir(tool)) == "pkg" && strings.Contains(filepath.Base(arch), "_")
}

// shellToken is one word of a command line as BypassesShim reads it.
type shellToken struct {
	raw     string // as written, for the reason
	word    string // quoting removed, braces expanded, ${...} braces trimmed
	command bool   // the command name, or a word that may still precede it
}

// shellTokens splits a command line into words. Command separators (; | &
// ( ) ` and newline, so $(...) too) start a new command; blanks and
// redirection operators separate words. A word is in command position until
// the command name: assignments, redirections, options and the prefixes in
// commandPrefixes may precede it, and a quoted word after a shell's -c starts
// a command line of its own. Each word becomes one token per brace
// expansion; false means an expansion past braceLimit.
func shellTokens(command string) ([]shellToken, bool) {
	var out []shellToken
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '|' || r == '&' || r == '(' || r == ')' || r == '`' || r == '\n'
	}) {
		head, target, shell, previous := true, false, false, ""
		for _, blank := range strings.FieldsFunc(segment, func(r rune) bool { return r == ' ' || r == '\t' }) {
			plain := shellWord(blank)
			redirect := strings.ContainsAny(blank, "<>")
			quoted := strings.IndexAny(blank, `'"`) == 0 || strings.HasPrefix(blank, "$'") || strings.HasPrefix(blank, `$"`)
			command := head && !target && !redirect || quoted && shell && commandString(previous)
			for _, piece := range strings.FieldsFunc(blank, func(r rune) bool { return r == '<' || r == '>' }) {
				expanded, ok := braceWords(shellWord(piece))
				if !ok {
					return nil, false
				}
				for _, w := range expanded {
					out = append(out, shellToken{raw: piece, word: strings.Trim(w, "{}$"), command: command})
				}
			}
			switch {
			case target: // a redirection's target, detached from its operator
				target = false
			case redirect:
				target = strings.Trim(blank, "0123456789<>") == ""
			case head:
				head = assignment(plain) || commandPrefixes[plain] || strings.HasPrefix(plain, "-")
			}
			shell = shell || shells[filepath.Base(plain)]
			previous = plain
		}
	}
	return out, true
}

// commandPrefixes are reserved words and builtins after which the next word
// is still a command: `builtin source FILE` sources FILE.
var commandPrefixes = map[string]bool{"!": true, "{": true, "if": true, "then": true, "elif": true, "else": true, "do": true, "while": true, "until": true, "time": true, "builtin": true, "command": true, "eval": true, "exec": true}

// shells are the shells whose options shellReadsProfile reads.
var shells = map[string]bool{"sh": true, "bash": true, "rbash": true, "dash": true, "ash": true, "ksh": true, "mksh": true, "zsh": true}

// assignment reports whether w is a shell variable assignment, NAME=VALUE or
// NAME+=VALUE.
func assignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	name = strings.TrimSuffix(name, "+")
	if !ok || name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	return strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == ""
}

// commandString reports whether w is a short-option cluster with -c, after
// which a shell's next operand is a command line.
func commandString(w string) bool {
	return len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.Contains(w, "c")
}

// envResetsPath reports whether env's arguments, up to its first operand,
// clear the environment (-i, -, --ignore-environment or any abbreviation of
// it), unset PATH (-u PATH, -uPATH, --unset=PATH), or take the utility's
// search path or a login class's environment from elsewhere (BSD -P, -L,
// -U). Short options may be clustered (-iv) and options that take a value
// are skipped with it (-C DIR). A -S string is read as further arguments: an
// attached one is scanned here, and a detached one is already split into the
// words that follow.
func envResetsPath(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-":
			return true
		case a == "--" || len(a) < 2 || a[0] != '-':
			return false
		case strings.HasPrefix(a, "--"):
			name, value, attached := strings.Cut(a[2:], "=")
			switch prefix := func(option string) bool { return strings.HasPrefix(option, name) }; {
			case prefix("ignore-environment"):
				return true
			case prefix("unset"):
				if !attached {
					value = next()
				}
				if value == "PATH" {
					return true
				}
			case prefix("chdir") && !attached:
				next()
			case prefix("split-string") && attached && envResetsPath(strings.Fields(value)):
				return true
			}
		default:
			for j := 1; j < len(a); j++ {
				switch c := a[j]; c {
				case 'i', 'P', 'L', 'U':
					return true
				case 'u', 'C', 'S':
					value := a[j+1:]
					if value == "" && c != 'S' {
						value = next()
					}
					if c == 'u' && value == "PATH" || c == 'S' && envResetsPath(strings.Fields(value)) {
						return true
					}
					j = len(a)
				}
			}
		}
	}
	return false
}

// execResetsPath reports whether exec's options run the command with an
// empty environment (-c) or as a login shell (-l, or an -a name starting
// with -), which reads profile files that can reset PATH.
func execResetsPath(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return false
		}
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'c', 'l':
				return true
			case 'a':
				name := a[j+1:]
				if name == "" && i+1 < len(args) {
					i++
					name = args[i]
				}
				if strings.HasPrefix(name, "-") {
					return true
				}
				j = len(a)
			}
		}
	}
	return false
}

// commandUsesDefaultPath reports whether command's options include -p, which
// searches the system's default PATH instead of the session's.
func commandUsesDefaultPath(args []string) bool {
	for _, a := range args {
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return false
		}
		if strings.Contains(a, "p") {
			return true
		}
	}
	return false
}

// shellReadsProfile reports whether a shell's options, up to its first
// operand, make it a login (-l, --login) or interactive (-i) shell or name an
// rc file (--rcfile, --init-file): such a shell reads profile files that can
// reset PATH.
func shellReadsProfile(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--login" || a == "--rcfile" || a == "--init-file" || strings.HasPrefix(a, "--rcfile=") || strings.HasPrefix(a, "--init-file="):
			return true
		case a == "--" || a == "-" || len(a) < 2 || a[0] != '-' && a[0] != '+':
			return false
		case strings.HasPrefix(a, "--"):
		default:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'l', 'i':
					if a[0] == '-' {
						return true
					}
				case 'o', 'O': // the option name is the next word
					i++
					j = len(a)
				}
			}
		}
	}
	return false
}

// braceLimit bounds the words one word's brace expansion may produce; a
// larger expansion is denied, not checked.
const braceLimit = 1024

// braceWords expands w's brace lists ({a,b}, nested) and letter sequences
// ({a..z}, {a..z..2}) as the shell does before its other expansions. A
// numeric sequence is left as written: its digits cannot spell a shim name or
// an option. ${...} is a parameter expansion, not a brace list. It reports
// false past braceLimit words.
func braceWords(w string) ([]string, bool) {
	var out []string
	var expand func(s string, from int) bool
	expand = func(s string, from int) bool {
		for i := from; i < len(s); i++ {
			if s[i] != '{' || i > 0 && s[i-1] == '$' {
				continue
			}
			end, items := braceItems(s, i)
			if items == nil {
				continue
			}
			for _, item := range items {
				if !expand(s[:i]+item+s[end+1:], i) {
					return false
				}
			}
			return true
		}
		out = append(out, s)
		return len(out) <= braceLimit
	}
	return out, expand(w, 0)
}

// braceItems returns the closing brace and the items of the brace
// expression opening at s[open], or no items when it is none: no top-level
// comma and no letter sequence, or no closing brace.
func braceItems(s string, open int) (int, []string) {
	depth, start := 0, open+1
	var items []string
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case ',':
			if depth == 0 {
				items = append(items, s[start:i])
				start = i + 1
			}
		case '}':
			if depth > 0 {
				depth--
				continue
			}
			if items != nil {
				return i, append(items, s[start:i])
			}
			return i, letterSequence(s[open+1 : i])
		}
	}
	return 0, nil
}

// letterSequence expands a brace sequence between two ASCII letters, X..Y or
// X..Y..STEP, or returns nil for anything else.
func letterSequence(inner string) []string {
	letter := func(s string) bool { return len(s) == 1 && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') }
	parts := strings.Split(inner, "..")
	if len(parts) < 2 || len(parts) > 3 || !letter(parts[0]) || !letter(parts[1]) {
		return nil
	}
	step := 1
	if len(parts) == 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil
		}
		step = max(n, -n, 1)
	}
	from, to := int(parts[0][0]), int(parts[1][0])
	if from > to {
		step = -step
	}
	var items []string
	for c := from; step > 0 && c <= to || step < 0 && c >= to; c += step {
		items = append(items, string(rune(c)))
	}
	return items
}

// shellWord is one word with its shell quoting removed: '...', "...", $"...",
// backslash escapes, and $'...' with its ANSI-C escapes decoded. It reads
// loosely (an unterminated quote runs to the end of the word, and a backslash
// inside single quotes still escapes), so it can only over-match.
func shellWord(w string) string {
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		switch c := w[i]; {
		case c == '$' && i+1 < len(w) && w[i+1] == '\'':
			for i += 2; i < len(w) && w[i] != '\''; i++ {
				if w[i] == '\\' && i+1 < len(w) {
					text, n := ansiEscape(w[i+1:])
					b.WriteString(text)
					i += n
					continue
				}
				b.WriteByte(w[i])
			}
		case c == '$' && i+1 < len(w) && w[i+1] == '"': // locale quoting; the quote goes next
		case c == '\'' || c == '"':
		case c == '\\' && i+1 < len(w):
			i++
			b.WriteByte(w[i])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ansiEscape decodes the escape at the start of s (the text after a backslash
// in $'...') and returns it with the number of bytes it used.
func ansiEscape(s string) (string, int) {
	digits := func(from, max int, base string) int {
		n := 0
		for n < max && from+n < len(s) && strings.IndexByte(base, s[from+n]) >= 0 {
			n++
		}
		return n
	}
	const octal, hex = "01234567", "0123456789abcdefABCDEF"
	switch c := s[0]; {
	case strings.IndexByte(octal, c) >= 0:
		n := 1 + digits(1, 2, octal)
		v, _ := strconv.ParseUint(s[:n], 8, 16)
		return string([]byte{byte(v)}), n
	case c == 'x' || c == 'u' || c == 'U':
		n := digits(1, map[byte]int{'x': 2, 'u': 4, 'U': 8}[c], hex)
		if n == 0 {
			return `\` + s[:1], 1
		}
		v, _ := strconv.ParseUint(s[1:1+n], 16, 32)
		if c == 'x' {
			return string([]byte{byte(v)}), 1 + n
		}
		return string(rune(v)), 1 + n
	case c == 'c' && len(s) > 1:
		return string([]byte{s[1] & 0x1f}), 2
	}
	if i := strings.IndexByte(`abeEfnrtv\'"?`, s[0]); i >= 0 {
		return string("\a\b\x1b\x1b\f\n\r\t\v\\'\"?"[i]), 1
	}
	return `\` + s[:1], 1
}

// gitSessionDenial reports whether a shell command line invokes git push or git
// remote (or push's plumbing, send-pack and http-push), including after git's
// leading global options (git -C DIR push, git -c KEY=VALUE remote) or env
// assignments/options (FAE-MODE-002).
func gitSessionDenial(command string) (string, bool) {
	tokens, ok := shellTokens(strings.ReplaceAll(command, "\\\n", ""))
	if !ok {
		return "", false
	}
	words := make([]string, len(tokens))
	for i, t := range tokens {
		words[i] = t.word
	}
	for i, t := range tokens {
		if !t.command {
			continue
		}
		end := len(tokens)
		for j := i + 1; j < len(tokens); j++ {
			if tokens[j].command {
				end = j
				break
			}
		}
		cmdIdx := i
		if filepath.Base(t.word) == "env" {
			cmdIdx = i + 1 + envOperandOffset(words[i+1:end])
		}
		if cmdIdx >= end || filepath.Base(words[cmdIdx]) != "git" {
			continue
		}
		if sub := gitSubcommand(words[cmdIdx+1 : end]); gitExternalSubcommands[sub] {
			return "git " + sub + " is forbidden inside a session", true
		}
	}
	return "", false
}

func envOperandOffset(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for i++; i < len(args) && assignment(args[i]); i++ {
			}
			return i
		case assignment(a):
		case strings.HasPrefix(a, "--"):
			if (strings.HasPrefix("--unset", a) || strings.HasPrefix("--chdir", a)) && !strings.Contains(a, "=") {
				i++
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				if a[j] == 'u' || a[j] == 'C' || a[j] == 'S' {
					if j+1 == len(a) && a[j] != 'S' {
						i++
					}
					break
				}
			}
		default:
			return i
		}
	}
	return len(args)
}

// PreToolDecision reads a provider pre-tool hook request ({"tool_name",
// "tool_input":{"command"}}) and returns the hook's JSON decision: deny with
// a reason for a shim bypass or forbidden git push/remote, otherwise no
// decision (the provider's own rules apply).
func PreToolDecision(r io.Reader, shimDir string) ([]byte, error) {
	var in struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&in); err != nil {
		return nil, err
	}
	reason, deny := BypassesShim(in.ToolInput.Command, shimDir)
	if !deny && (in.ToolName == "" || in.ToolName == "Bash" || in.ToolName == "shell") {
		if policy, err := ReadRoleShims(filepath.Join(shimDir, ".policy.json")); err == nil {
			if _, err = workflow.CheckShellCommand(workflow.Profile{ShellAllow: policy.ShellAllow, DenyTestRuns: policy.DenyTestRuns}, in.ToolInput.Command, policy.DeniedTestCommands); err != nil {
				reason, deny = err.Error(), true
				if policy.ShellAuditPath != "" {
					raw, _ := json.Marshal(map[string]any{"at": time.Now().UTC(), "command": in.ToolInput.Command, "reason": reason})
					if f, e := os.OpenFile(policy.ShellAuditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); e == nil {
						_, _ = f.Write(append(raw, '\n'))
						_ = f.Close()
					}
				}
			}
		}
	}
	if !deny && (in.ToolName == "" || in.ToolName == "Bash") {
		reason, deny = gitSessionDenial(in.ToolInput.Command)
	}
	if !deny {
		return []byte("{}\n"), nil
	}
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "fae: " + reason}})
	return append(out, '\n'), err
}
