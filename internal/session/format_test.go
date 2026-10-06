package session

import (
	"testing"
	"time"
	"unicode/utf8"
)

// 1. Headers come back in a fixed, known order.
func TestCells_HeaderOrder(t *testing.T) {
	headers, _ := Cells(nil)
	want := []string{"ID", "STARTED", "MSGS", "PROJECT", "SUMMARY"}
	if len(headers) != len(want) {
		t.Fatalf("headers = %v, want %v", headers, want)
	}
	for i := range want {
		if headers[i] != want[i] {
			t.Errorf("headers[%d] = %q, want %q", i, headers[i], want[i])
		}
	}
}

// 2. The ID cell is the first 8 runes of the session id.
func TestCells_ShortIDIsEightRunes(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9-ca71-48c0"}, FirstUserMessage: "hi"},
	}
	_, rows := Cells(sums)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	id := rows[0][0]
	if utf8.RuneCountInString(id) != 8 {
		t.Errorf("id cell = %q, want 8 runes", id)
	}
	if id != "81e12fd9" {
		t.Errorf("id cell = %q, want %q", id, "81e12fd9")
	}
}

// 3. A multi-byte id is truncated on a rune boundary, not a byte boundary.
func TestCells_MultiByteIDDoesNotSplitRune(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "☕☕☕☕☕☕☕☕☕☕-rest"}, FirstUserMessage: "hi"},
	}
	_, rows := Cells(sums)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	id := rows[0][0]
	if !utf8.ValidString(id) {
		t.Errorf("id cell = %q, not valid UTF-8", id)
	}
	if utf8.RuneCountInString(id) != 8 {
		t.Errorf("id cell = %q, want 8 runes", id)
	}
	if id != "☕☕☕☕☕☕☕☕" {
		t.Errorf("id cell = %q, want 8 coffee cups", id)
	}
}

// 4. A zero StartedAt renders "unknown".
func TestCells_ZeroStartedAtRendersUnknown(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: "hi"},
	}
	_, rows := Cells(sums)
	if rows[0][1] != "unknown" {
		t.Errorf("started cell = %q, want %q", rows[0][1], "unknown")
	}
}

// 5. A non-zero StartedAt renders in local time as "2006-01-02 15:04".
func TestCells_StartedAtFormatted(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9", StartedAt: time.Date(2026, 10, 5, 9, 7, 0, 0, time.UTC)}, FirstUserMessage: "hi"},
	}
	_, rows := Cells(sums)
	want := time.Date(2026, 10, 5, 9, 7, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
	if rows[0][1] != want {
		t.Errorf("started cell = %q, want %q", rows[0][1], want)
	}
}

// 6. Message count renders as a plain decimal string.
func TestCells_MessageCountIsDecimalString(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: "hi", MessageCount: 42},
	}
	_, rows := Cells(sums)
	if rows[0][2] != "42" {
		t.Errorf("msgs cell = %q, want %q", rows[0][2], "42")
	}
}

// 7. Project falls back ProjectRoot -> CWD -> "-".
func TestCells_ProjectFallsBackProjectRootThenCWDThenDash(t *testing.T) {
	cases := []struct {
		name string
		sum  Summary
		want string
	}{
		{"project root wins", Summary{Header: Header{ID: "a", ProjectRoot: "/x/dewey", CWD: "/x/other"}}, "dewey"},
		{"cwd when no project root", Summary{Header: Header{ID: "a", CWD: "/x/other"}}, "other"},
		{"dash when neither", Summary{Header: Header{ID: "a"}}, "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, rows := Cells([]Summary{c.sum})
			if rows[0][3] != c.want {
				t.Errorf("project cell = %q, want %q", rows[0][3], c.want)
			}
		})
	}
}

// 8. The project label is not truncated, even when long.
func TestCells_ProjectLabelNotTruncated(t *testing.T) {
	long := "some-very-long-service-name-that-would-have-been-truncated-before"
	sums := []Summary{
		{Header: Header{ID: "a", ProjectRoot: "/x/" + long}},
	}
	_, rows := Cells(sums)
	if rows[0][3] != long {
		t.Errorf("project cell = %q, want untruncated %q", rows[0][3], long)
	}
}

// 9. An empty FirstUserMessage renders "(no messages)".
func TestCells_EmptySummaryRendersNoMessages(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9"}, FirstUserMessage: ""},
	}
	_, rows := Cells(sums)
	if rows[0][4] != "(no messages)" {
		t.Errorf("summary cell = %q, want %q", rows[0][4], "(no messages)")
	}
}

// 10. ResumedFrom produces the "↳ from <first 8 of ResumedFrom>: " prefix.
func TestCells_ResumedFromPrefix(t *testing.T) {
	sums := []Summary{
		{Header: Header{ID: "81e12fd9", ResumedFrom: "original-session-1234"}, FirstUserMessage: "continue please"},
	}
	_, rows := Cells(sums)
	want := "↳ from original: continue please"
	if rows[0][4] != want {
		t.Errorf("summary cell = %q, want %q", rows[0][4], want)
	}
}

// 11. Empty input returns headers and no rows.
func TestCells_EmptyInputReturnsHeadersNoRows(t *testing.T) {
	headers, rows := Cells(nil)
	if len(headers) == 0 {
		t.Error("headers is empty, want headers even with no sessions")
	}
	if len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0", len(rows))
	}
}
