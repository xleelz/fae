package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Colours are the 16 ANSI colours, so the terminal's own theme decides the
// exact shades.
var (
	styleBold     = lipgloss.NewStyle().Bold(true)
	styleFaint    = lipgloss.NewStyle().Faint(true)
	styleReverse  = lipgloss.NewStyle().Reverse(true)
	styleError    = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styleWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleHeading  = lipgloss.NewStyle().Bold(true).Underline(true)
	styleSelected = lipgloss.NewStyle().Reverse(true).Bold(true)
	styleKey      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	styleWrite    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11"))
	styleBox      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

var classStyles = map[stateClass]lipgloss.Style{
	classUnknown:   lipgloss.NewStyle().Faint(true),
	classReady:     lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
	classWaiting:   lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
	classStage:     lipgloss.NewStyle().Foreground(lipgloss.Color("12")),
	classRunning:   lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true),
	classBlocked:   lipgloss.NewStyle().Foreground(lipgloss.Color("13")),
	classEscalated: lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true),
	classDone:      lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
	classClosed:    lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
}

// glyphs mark each state class without colour too.
var classGlyphs = map[stateClass]string{
	classUnknown:   "?",
	classReady:     "○",
	classWaiting:   "◌",
	classStage:     "◆",
	classRunning:   "▶",
	classBlocked:   "■",
	classEscalated: "!",
	classDone:      "✓",
	classClosed:    "×",
}

func (s implState) glyph() string { return classGlyphs[s.Class] }

func (s implState) render(text string) string { return classStyles[s.Class].Render(text) }

// fit truncates a styled line to width cells, then pads it to exactly width.
func fit(line string, width int) string {
	if width <= 0 {
		return ""
	}
	line = ansi.Truncate(line, width, "…")
	if w := lipgloss.Width(line); w < width {
		line += strings.Repeat(" ", width-w)
	}
	return line
}

// clip truncates a styled line to width cells.
func clip(line string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(line, width, "…")
}

// wrap breaks plain text into lines of at most width cells, keeping its own
// line breaks.
func wrap(text string, width int) []string {
	if width < 8 {
		width = 8
	}
	out := []string{}
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Wrap(para, width, ""), "\n")...)
	}
	return out
}

// scrollTo returns the first visible line so that line cursor stays inside a
// window of height lines, starting from the previous offset.
func scrollTo(offset, cursor, height, total int) int {
	if height <= 0 {
		return 0
	}
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+height {
		offset = cursor - height + 1
	}
	if offset > total-height {
		offset = total - height
	}
	return max(offset, 0)
}

// window returns the visible lines.
func window(lines []string, offset, height int) []string {
	if offset > len(lines) {
		offset = len(lines)
	}
	end := min(len(lines), offset+max(height, 0))
	return lines[offset:end]
}

// ago renders a duration since t, coarsely.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return age(now.Sub(t)) + " ago"
}

// age renders a duration coarsely: 850ms, 12s, 4m10s, 3h5m, 2d4h.
func age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

// stamp renders a time in local time: the clock today, else the date too.
func stamp(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 02 15:04")
}

// ansiStrip removes styling, so a selected line can be restyled whole.
func ansiStrip(s string) string { return ansi.Strip(s) }

// renderGauge renders a compact progress bar like [████░░].
func renderGauge(used, total, width int) string {
	if width <= 0 {
		width = 8
	}
	if total <= 0 {
		return "[" + strings.Repeat("░", width) + "]"
	}
	used = min(max(used, 0), total)
	filled := (used * width) / total
	if used > 0 && filled == 0 {
		filled = 1
	}
	filled = min(max(filled, 0), width)
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// until renders a countdown to t relative to now, or "due" when !now.Before(t).
func until(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	if !now.Before(t) {
		return "due"
	}
	return "in " + age(t.Sub(now))
}

// matchesFilter reports whether any field contains filter (case-insensitive).
// An empty filter matches everything.
func matchesFilter(filter string, fields ...string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), filter) {
			return true
		}
	}
	return false
}
