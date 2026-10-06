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

// enter on a selection sets the chosen id.
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

// esc sets quitting with an empty id.
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

// An empty input produces a model that reports nothing to choose, even on
// enter.
func TestModel_EmptyInputHasNothingToChoose(t *testing.T) {
	m := newModel(nil, 100)

	updated, _ := m.Update(key("enter"))
	mm := updated.(model)

	if mm.chosen != "" {
		t.Errorf("chosen = %q, want empty with no sessions", mm.chosen)
	}
}

// A WindowSizeMsg resizes the list and does not panic.
func TestModel_WindowSizeMsgResizes(t *testing.T) {
	sums := testSummaries()
	m := newModel(sums, 100)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	mm := updated.(model)

	if mm.list.Width() != 60 {
		t.Errorf("list width = %d, want 60", mm.list.Width())
	}
	// Must not panic rendering at the new width.
	_ = mm.View()
}
