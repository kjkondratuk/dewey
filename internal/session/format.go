package session

import (
	"path/filepath"
	"strconv"
)

// shortIDRunes is how many runes of a session id are shown: enough to
// disambiguate without eating the whole line.
const shortIDRunes = 8

// Cells renders summaries as table data: a header row and one row of cell
// values per session.
//
// Deliberately unpadded and untruncated. Layout belongs to whatever is
// drawing — lipgloss/table for the static listing, bubbles/table for the
// picker — and both get the same cells, so the two views cannot disagree
// about what columns exist or what is in them.
func Cells(sums []Summary) (headers []string, rows [][]string) {
	headers = []string{"ID", "STARTED", "MSGS", "PROJECT", "SUMMARY"}

	rows = make([][]string, len(sums))
	for i, s := range sums {
		rows[i] = []string{
			shortID(s.ID),
			startedLabel(s),
			strconv.Itoa(s.MessageCount),
			projectLabel(s),
			summaryLabel(s),
		}
	}
	return headers, rows
}

// startedLabel is the text shown in the STARTED column: the session's start
// time in local time, or "unknown" when it was never recorded.
func startedLabel(s Summary) string {
	if s.StartedAt.IsZero() {
		return "unknown"
	}
	return s.StartedAt.Local().Format("2006-01-02 15:04")
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

// summaryLabel is the text shown in the SUMMARY column. A session resumed
// from another one gets its summary prefixed, since its own opening message
// continues a conversation rather than describing what it was originally
// about.
func summaryLabel(s Summary) string {
	msg := s.FirstUserMessage
	if msg == "" {
		msg = "(no messages)"
	}
	if s.ResumedFrom != "" {
		msg = "↳ from " + shortID(s.ResumedFrom) + ": " + msg
	}
	return msg
}

// shortID returns the first shortIDRunes runes of id, or id unchanged if it
// has fewer. Rune-safe so a multi-byte character is never cut mid-codepoint.
func shortID(id string) string {
	r := []rune(id)
	if len(r) <= shortIDRunes {
		return id
	}
	return string(r[:shortIDRunes])
}
