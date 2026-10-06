package picker

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/kjkondratuk/dewey/internal/session"
)

func key(s string) tea.KeyPressMsg {
	if len(s) == 1 {
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	switch s {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl, Text: ""}
	}
	panic("unhandled key " + s)
}

func testSummaries() []session.Summary {
	return []session.Summary{
		{Header: session.Header{ID: "81e12fd9-ca71-48c0"}, FirstUserMessage: "review this project"},
		{Header: session.Header{ID: "4d315e3c-20fc-4ce5"}, FirstUserMessage: "hello?"},
	}
}

// 1. enter in normal mode sets the chosen id to the highlighted session.
func TestModel_EnterChoosesSelectedSession(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(key("enter"))
	mm := updated.(model)

	if !mm.quitting {
		t.Error("quitting = false after enter, want true")
	}
	if mm.chosen != sums[0].ID {
		t.Errorf("chosen = %q, want %q (the first/selected item)", mm.chosen, sums[0].ID)
	}
}

// Moving down then pressing enter chooses the second item.
func TestModel_DownThenEnterChoosesSecondSession(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(key("down"))
	mm := updated.(model)
	updated, _ = mm.Update(key("enter"))
	mm = updated.(model)

	if mm.chosen != sums[1].ID {
		t.Errorf("chosen = %q, want %q", mm.chosen, sums[1].ID)
	}
}

// 2. esc in normal mode sets quitting with an empty id.
func TestModel_EscQuitsWithoutChoosing(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(key("esc"))
	mm := updated.(model)

	if !mm.quitting {
		t.Error("quitting = false after esc, want true")
	}
	if mm.chosen != "" {
		t.Errorf("chosen = %q, want empty", mm.chosen)
	}
}

// q and ctrl+c also quit without choosing.
func TestModel_QAndCtrlCQuitWithoutChoosing(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		sums := testSummaries()
		m := newModel(sums, 100)

		updated, _ := m.Update(key(k))
		mm := updated.(model)

		if !mm.quitting {
			t.Errorf("key %q: quitting = false, want true", k)
		}
		if mm.chosen != "" {
			t.Errorf("key %q: chosen = %q, want empty", k, mm.chosen)
		}
	}
}

// 3. "/" enters filter mode; a subsequent keystroke goes to the textinput,
// not to navigation.
func TestModel_SlashEntersFilterMode_KeystrokeGoesToTextinput(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(key("/"))
	mm := updated.(model)
	if mm.mode != modeFilter {
		t.Fatalf("mode = %v, want modeFilter after /", mm.mode)
	}

	// "j" would move the cursor in normal mode; while filtering it must be
	// consumed by the textinput instead.
	updated, _ = mm.Update(key("j"))
	mm = updated.(model)

	if mm.table.Cursor() != 0 {
		t.Errorf("cursor = %d, want 0 (navigation must not fire while filtering)", mm.table.Cursor())
	}
	if mm.filter.Value() != "j" {
		t.Errorf("filter value = %q, want %q", mm.filter.Value(), "j")
	}
}

// 4. Filtering then choosing returns the id of the filtered row, not the row
// at that index in the original slice. This is the classic filter-picker
// defect: after filtering, the table's cursor indexes the *filtered* rows,
// so resolving through the original slice returns the wrong session.
func TestModel_FilterThenChooseReturnsFilteredRowID(t *testing.T) {
	sums := []session.Summary{
		{Header: session.Header{ID: "session-A"}, FirstUserMessage: "alpha opening message"},
		{Header: session.Header{ID: "session-B"}, FirstUserMessage: "needle unique marker here"},
		{Header: session.Header{ID: "session-C"}, FirstUserMessage: "gamma opening message"},
	}
	// Naive sums[cursor] after filtering to one row (cursor 0) would return
	// sums[0] = session-A, which is wrong. The filtered row is session-B.
	m := newModel(sums, 100)

	updated, _ := m.Update(key("/"))
	mm := updated.(model)

	for _, r := range []rune("needle") {
		updated, _ = mm.Update(key(string(r)))
		mm = updated.(model)
	}

	if len(mm.visible) != 1 {
		t.Fatalf("len(visible) = %d, want 1 after filtering to \"needle\"", len(mm.visible))
	}
	if mm.visible[0].ID != "session-B" {
		t.Fatalf("visible[0].ID = %q, want %q", mm.visible[0].ID, "session-B")
	}

	updated, _ = mm.Update(key("enter"))
	mm = updated.(model)
	if mm.mode != modeNormal {
		t.Fatalf("mode = %v, want modeNormal after enter in filter mode", mm.mode)
	}

	updated, _ = mm.Update(key("enter"))
	mm = updated.(model)

	if mm.chosen != "session-B" {
		t.Errorf("chosen = %q, want %q (the filtered row, not sums[cursor])", mm.chosen, "session-B")
	}
}

// 5. esc in filter mode clears the filter and restores the full row set.
func TestModel_EscInFilterModeClearsFilterAndRestoresRows(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(key("/"))
	mm := updated.(model)
	updated, _ = mm.Update(key("h"))
	mm = updated.(model)
	updated, _ = mm.Update(key("i"))
	mm = updated.(model)

	if len(mm.visible) != 1 {
		t.Fatalf("len(visible) = %d, want 1 after filtering to \"hi\" (matches only \"hello?\")", len(mm.visible))
	}

	updated, _ = mm.Update(key("esc"))
	mm = updated.(model)

	if mm.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal after esc", mm.mode)
	}
	if mm.filter.Value() != "" {
		t.Errorf("filter value = %q, want empty after esc", mm.filter.Value())
	}
	if len(mm.visible) != len(sums) {
		t.Errorf("len(visible) = %d, want %d (full set restored)", len(mm.visible), len(sums))
	}
}

// 6. A filter matching nothing leaves no rows, and enter then chooses
// nothing rather than panicking.
func TestModel_FilterMatchingNothingThenEnterChoosesNothing(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(key("/"))
	mm := updated.(model)
	for _, r := range []rune("zzz-no-match") {
		updated, _ = mm.Update(key(string(r)))
		mm = updated.(model)
	}

	if len(mm.visible) != 0 {
		t.Fatalf("len(visible) = %d, want 0", len(mm.visible))
	}

	updated, _ = mm.Update(key("enter"))
	mm = updated.(model)
	updated, _ = mm.Update(key("enter"))
	mm = updated.(model)

	if mm.chosen != "" {
		t.Errorf("chosen = %q, want empty when nothing matched", mm.chosen)
	}
}

// 7. Changing the filter resets the cursor to 0.
func TestModel_ChangingFilterResetsCursorToZero(t *testing.T) {
	sums := []session.Summary{
		{Header: session.Header{ID: "a"}, FirstUserMessage: "alpha common"},
		{Header: session.Header{ID: "b"}, FirstUserMessage: "beta common"},
		{Header: session.Header{ID: "c"}, FirstUserMessage: "gamma common"},
	}
	m := newModel(sums, 100)

	updated, _ := m.Update(key("down"))
	mm := updated.(model)
	if mm.table.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1 before filtering", mm.table.Cursor())
	}

	updated, _ = mm.Update(key("/"))
	mm = updated.(model)
	updated, _ = mm.Update(key("c"))
	mm = updated.(model)

	if mm.table.Cursor() != 0 {
		t.Errorf("cursor = %d, want 0 after the filter changed", mm.table.Cursor())
	}
}

// 8. An empty sums produces a model that reports nothing to choose and does
// not panic.
func TestModel_EmptyInputHasNothingToChoose(t *testing.T) {
	m := newModel(nil, 100)

	updated, _ := m.Update(key("enter"))
	mm := updated.(model)

	if mm.chosen != "" {
		t.Errorf("chosen = %q, want empty with no sessions", mm.chosen)
	}

	// Must not panic entering filter mode, typing, or resizing either.
	updated, _ = mm.Update(key("/"))
	mm = updated.(model)
	updated, _ = mm.Update(key("x"))
	mm = updated.(model)
	updated, _ = mm.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	mm = updated.(model)
	_ = mm.View()
}

// A WindowSizeMsg resizes the table and does not panic.
func TestModel_WindowSizeMsgResizes(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	mm := updated.(model)

	if mm.width != 60 {
		t.Errorf("width = %d, want 60", mm.width)
	}
	// Must not panic rendering at the new width.
	_ = mm.View()
}
