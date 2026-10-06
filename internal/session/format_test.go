package session

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 1. Columns align: every row's STARTED field starts at the same index as
// the header's.
func TestColumns_Align(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9-ca71", StartedAt: time.Date(2026, 10, 5, 9, 7, 0, 0, time.UTC), ProjectRoot: "/x/dewey"}, FirstUserMessage: "review this project and write a doc", MessageCount: 8},
		{Header: Header{ID: "4d315e3c-20fc", StartedAt: time.Date(2026, 10, 5, 9, 2, 0, 0, time.UTC), ProjectRoot: "/x/dewey"}, FirstUserMessage: "hello?", MessageCount: 3},
	}

	header, rows := Columns(sums, 100)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	idx := strings.Index(header, "STARTED")
	if idx < 0 {
		t.Fatalf("header %q has no STARTED label", header)
	}
	for i, row := range rows {
		// The STARTED column content (a date) must begin at the same byte
		// offset as the header label, since both are left-padded to the
		// same fixed-width ID column + gutter.
		if len(row) < idx {
			t.Fatalf("row[%d] = %q shorter than STARTED offset %d", i, row, idx)
		}
	}
}

// 2. A long summary is truncated with a trailing … and the row does not
// exceed width.
func TestColumns_LongSummaryTruncated(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: strings.Repeat("a very long summary that keeps going ", 10)},
	}

	const width = 100
	_, rows := Columns(sums, width)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	row := rows[0]
	if !strings.Contains(row, "…") {
		t.Errorf("row = %q, want a truncation ellipsis", row)
	}
	if utf8.RuneCountInString(row) > width {
		t.Errorf("row rune count = %d, want <= %d", utf8.RuneCountInString(row), width)
	}
}

// 3. A short summary is not padded with trailing whitespace.
func TestColumns_ShortSummaryNoTrailingWhitespace(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: "hi"},
	}

	_, rows := Columns(sums, 100)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0] != strings.TrimRight(rows[0], " ") {
		t.Errorf("row = %q, want no trailing whitespace", rows[0])
	}
}

// 4. A multi-byte summary truncates on a rune boundary.
func TestColumns_MultiByteSummaryTruncatesOnRuneBoundary(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: strings.Repeat("café ☕ review ", 20)},
	}

	_, rows := Columns(sums, 60)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if !utf8.ValidString(rows[0]) {
		t.Errorf("row = %q, not valid UTF-8", rows[0])
	}
}

// 5. An empty FirstUserMessage renders "(no messages)".
func TestColumns_EmptyFirstUserMessage(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: ""},
	}

	_, rows := Columns(sums, 100)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if !strings.Contains(rows[0], "(no messages)") {
		t.Errorf("row = %q, want (no messages)", rows[0])
	}
}

// 6. ResumedFrom produces the "↳ from" prefix.
func TestColumns_ResumedFromPrefix(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9", ResumedFrom: "original-session-1234"}, FirstUserMessage: "continue please"},
	}

	_, rows := Columns(sums, 100)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if !strings.Contains(rows[0], "↳ from original") {
		t.Errorf("row = %q, want a ↳ from prefix naming the original session", rows[0])
	}
	if !strings.Contains(rows[0], "continue please") {
		t.Errorf("row = %q, want the resumed session's own first message too", rows[0])
	}
}

// 7. A zero StartedAt renders "unknown" without breaking alignment.
func TestColumns_ZeroStartedAtRendersUnknown(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: "hi"},
		{Header: Header{ID: "4d315e3c", StartedAt: time.Date(2026, 10, 5, 9, 2, 0, 0, time.UTC)}, FirstUserMessage: "hello"},
	}

	header, rows := Columns(sums, 100)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if !strings.Contains(rows[0], "unknown") {
		t.Errorf("row = %q, want unknown for a zero StartedAt", rows[0])
	}

	idx := strings.Index(header, "MSGS")
	// Alignment must survive: the second row's MSGS column starts at the
	// same offset as the first row's and the header's.
	if len(rows[0]) < idx || len(rows[1]) < idx {
		t.Fatalf("rows too short for MSGS offset %d: %q / %q", idx, rows[0], rows[1])
	}
}

// 8. A very small width still produces valid, non-panicking output.
func TestColumns_VerySmallWidth(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: "a reasonably long opening message for this session"},
	}

	header, rows := Columns(sums, 40)
	if header == "" {
		t.Error("header is empty")
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if !utf8.ValidString(rows[0]) {
		t.Errorf("row = %q, not valid UTF-8", rows[0])
	}
}

// 9. Empty input returns a header and no rows.
func TestColumns_EmptyInput(t *testing.T) {
	header, rows := Columns(nil, 100)
	if header == "" {
		t.Error("header is empty, want a header row even with no sessions")
	}
	if len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0", len(rows))
	}
}
