package session

import (
	"path/filepath"
	"strconv"
)

// Fixed column widths. STARTED matches len("2006-01-02 15:04"); the others
// are chosen to comfortably hold their content ("unknown", a 2-4 digit
// count, a typical directory name) without being wasteful.
const (
	idWidth      = 8
	startedWidth = 16
	msgsWidth    = 4
	projectWidth = 12
	gutter       = "  "

	// minSummaryWidth is the floor the summary column never goes below, so a
	// narrow terminal degrades to a hard truncation rather than negative
	// slice arithmetic.
	minSummaryWidth = 20

	ellipsis = "…"
)

const fixedColumnsWidth = idWidth + len(gutter) + startedWidth + len(gutter) + msgsWidth + len(gutter) + projectWidth + len(gutter)

// Columns renders summaries as an aligned table: a header row and one row
// per session, with the summary column truncated to fit width.
//
// Shared so the plain listing and the interactive picker cannot drift apart.
func Columns(sums []Summary, width int) (header string, rows []string) {
	summaryWidth := width - fixedColumnsWidth
	if summaryWidth < minSummaryWidth {
		summaryWidth = minSummaryWidth
	}

	header = padRightRunes("ID", idWidth) + gutter +
		padRightRunes("STARTED", startedWidth) + gutter +
		padLeftRunes("MSGS", msgsWidth) + gutter +
		padRightRunes("PROJECT", projectWidth) + gutter +
		"SUMMARY"

	rows = make([]string, len(sums))
	for i, s := range sums {
		rows[i] = formatRow(s, summaryWidth)
	}
	return header, rows
}

func formatRow(s Summary, summaryWidth int) string {
	id := padRightRunes(firstNRunes(s.ID, idWidth), idWidth)

	started := "unknown"
	if !s.StartedAt.IsZero() {
		started = s.StartedAt.Local().Format("2006-01-02 15:04")
	}
	started = padRightRunes(started, startedWidth)

	msgs := padLeftRunes(strconv.Itoa(s.MessageCount), msgsWidth)

	project := padRightRunes(truncateRunes(projectLabel(s), projectWidth), projectWidth)

	summary := truncateRunes(summaryLabel(s), summaryWidth)

	return id + gutter + started + gutter + msgs + gutter + project + gutter + summary
}

// projectLabel is the directory name identifying which project a session ran
// in: the project root when known, else the raw cwd, else "-" when neither
// was recorded.
func projectLabel(s Summary) string {
	switch {
	case s.ProjectRoot != "":
		return filepath.Base(s.ProjectRoot)
	case s.CWD != "":
		return filepath.Base(s.CWD)
	default:
		return "-"
	}
}

// summaryLabel is the text shown in the SUMMARY column, before truncation.
// A session resumed from another one gets its summary prefixed, since its
// own opening message continues a conversation rather than describing what
// it was originally about.
func summaryLabel(s Summary) string {
	msg := s.FirstUserMessage
	if msg == "" {
		msg = "(no messages)"
	}
	if s.ResumedFrom != "" {
		msg = "↳ from " + firstNRunes(s.ResumedFrom, idWidth) + ": " + msg
	}
	return msg
}

// firstNRunes returns the first n runes of s, or s unchanged if it has fewer.
func firstNRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// truncateRunes truncates s to at most width runes, counting runes rather
// than bytes so a multi-byte character is never cut mid-codepoint. A
// truncated string ends with an ellipsis, which counts against width.
func truncateRunes(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return ellipsis
	}
	return string(r[:width-1]) + ellipsis
}

// padRightRunes pads s with trailing spaces until it is width runes wide. A
// string already at or beyond width is returned unchanged.
func padRightRunes(s string, width int) string {
	n := width - runeLen(s)
	if n <= 0 {
		return s
	}
	return s + spaces(n)
}

// padLeftRunes pads s with leading spaces until it is width runes wide. A
// string already at or beyond width is returned unchanged.
func padLeftRunes(s string, width int) string {
	n := width - runeLen(s)
	if n <= 0 {
		return s
	}
	return spaces(n) + s
}

func runeLen(s string) int {
	return len([]rune(s))
}

func spaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}
