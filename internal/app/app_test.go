package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kjkondratuk/dewey/internal/agent"
)

// fakeAgent hands back a channel the test drives directly, so Update can be
// stepped through one event at a time.
type fakeAgent struct {
	ch      chan agent.Event
	err     error
	runs    int
	lastCtx context.Context
}

func (f *fakeAgent) Run(ctx context.Context, input string) (<-chan agent.Event, error) {
	f.runs++
	f.lastCtx = ctx
	if f.err != nil {
		return nil, f.err
	}
	return f.ch, nil
}

func newTestApp(t *testing.T, a agent.Agent) *App {
	t.Helper()
	entry := textinput.New()
	entry.Prompt = "> "
	// Match main.go: the root View publishes the real cursor itself.
	entry.SetVirtualCursor(false)
	m := &App{Conversation: viewport.New(), Entry: entry, Agent: a}
	m.Init()
	// Give it a terminal so layout()/View() have real dimensions.
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m
}

func key(s string) tea.KeyPressMsg {
	if len(s) == 1 {
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	switch s {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	panic("unhandled key " + s)
}

// submit types input and presses enter, returning the resulting command.
func submit(t *testing.T, m *App, input string) tea.Cmd {
	t.Helper()
	m.Entry.SetValue(input)
	_, cmd := m.Update(key("enter"))
	return cmd
}

func TestSubmit_EntersBusyStateAndBlursInput(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)

	submit(t, m, "hello")

	if !m.busy {
		t.Error("busy = false after submitting, want true")
	}
	if m.Entry.Focused() {
		t.Error("entry still focused while busy, want blurred")
	}
	if got := m.Entry.Value(); got != "" {
		t.Errorf("entry value = %q, want cleared after a successful submit", got)
	}
	if f.runs != 1 {
		t.Errorf("agent runs = %d, want 1", f.runs)
	}
}

func TestSubmit_WhileBusyIsIgnored(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)

	submit(t, m, "first")
	submit(t, m, "second")

	if f.runs != 1 {
		t.Errorf("agent runs = %d, want 1 — a second submit while busy must be ignored", f.runs)
	}
}

func TestSubmit_TypingIsSwallowedWhileBusy(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	submit(t, m, "hello")

	m.Update(key("x"))
	if got := m.Entry.Value(); got != "" {
		t.Errorf("entry value = %q, want empty — keystrokes must not reach a disabled input", got)
	}
}

func TestSubmit_RunErrorKeepsThePromptAndStaysIdle(t *testing.T) {
	f := &fakeAgent{err: errors.New("model down")}
	m := newTestApp(t, f)

	submit(t, m, "hello")

	if m.busy {
		t.Error("busy = true after a failed Run, want false")
	}
	if got := m.Entry.Value(); got != "hello" {
		t.Errorf("entry value = %q, want the prompt preserved so it needn't be retyped", got)
	}
}

// Every route out of a turn must hand the input back, or the session is dead.
func TestTerminalEventsAlwaysLeaveBusy(t *testing.T) {
	for _, kind := range []string{"done", "error", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeAgent{ch: make(chan agent.Event, 8)}
			m := newTestApp(t, f)
			submit(t, m, "hello")

			m.Update(agentEventMsg{Kind: kind, Payload: "x"})

			if m.busy {
				t.Errorf("busy = true after %q, want false", kind)
			}
			if !m.Entry.Focused() {
				t.Errorf("entry not refocused after %q", kind)
			}
			if m.abortArmed {
				t.Errorf("abortArmed = true after %q, want cleared", kind)
			}
		})
	}
}

func TestUnknownEventKindKeepsDrainingInsteadOfWedging(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	submit(t, m, "hello")

	_, cmd := m.Update(agentEventMsg{Kind: "something-new"})
	if cmd == nil {
		t.Fatal("unknown event kind returned no command — the channel would never be read again")
	}
	if !m.busy {
		t.Error("busy = false, want the turn still considered in flight")
	}
}

func TestAbort_RequiresTwoEscapes(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	submit(t, m, "hello")

	m.Update(key("esc"))
	if !m.abortArmed {
		t.Fatal("abortArmed = false after one escape, want armed")
	}
	if f.lastCtx.Err() != nil {
		t.Fatal("context cancelled by a single escape, want two presses required")
	}

	m.Update(key("esc"))
	if f.lastCtx.Err() == nil {
		t.Error("context not cancelled after a second escape")
	}
	if m.abortArmed {
		t.Error("abortArmed = true after firing, want cleared")
	}
}

func TestAbort_DisarmTimerExpires(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	submit(t, m, "hello")

	m.Update(key("esc"))
	m.Update(abortDisarmMsg{gen: m.abortGen})

	if m.abortArmed {
		t.Error("abortArmed = true after its disarm fired, want false")
	}
	m.Update(key("esc"))
	if f.lastCtx.Err() != nil {
		t.Error("a single escape aborted after the window lapsed; it should only re-arm")
	}
}

// A disarm timer from an earlier arming must not cancel a later one — that
// silently breaks the only escape hatch available while input is gated.
func TestAbort_StaleDisarmDoesNotBreakALaterArming(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)

	// Turn one: arm, then abort. The first arm's disarm timer is still pending.
	submit(t, m, "first")
	m.Update(key("esc"))
	staleGen := m.abortGen
	m.Update(key("esc"))
	m.Update(agentEventMsg{Kind: "cancelled"})

	// Turn two: arm again, then let the stale timer land.
	f.ch = make(chan agent.Event, 8)
	submit(t, m, "second")
	m.Update(key("esc"))
	if !m.abortArmed {
		t.Fatal("abortArmed = false after arming on the second turn")
	}

	m.Update(abortDisarmMsg{gen: staleGen})
	if !m.abortArmed {
		t.Fatal("a stale disarm cleared the current arming")
	}

	m.Update(key("esc"))
	if f.lastCtx.Err() == nil {
		t.Error("second escape did not abort — the stale timer swallowed the double-press")
	}
}

func TestConfirm_ClearsAnArmedAbortSoASinglePressMeansCancel(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	submit(t, m, "hello")

	m.Update(key("esc")) // arm while thinking
	resp := make(chan bool, 1)
	m.Update(agentEventMsg{Kind: "confirm", Payload: "allow?", Confirm: resp})

	if m.abortArmed {
		t.Error("abortArmed survived into the confirm prompt, where a single esc already cancels")
	}
}

func TestConfirm_AnswerResumesWaitingAndRestartsTheSpinner(t *testing.T) {
	for _, answer := range []struct {
		key  string
		want bool
	}{{"y", true}, {"n", false}} {
		t.Run(answer.key, func(t *testing.T) {
			f := &fakeAgent{ch: make(chan agent.Event, 8)}
			m := newTestApp(t, f)
			submit(t, m, "hello")

			resp := make(chan bool, 1)
			m.Update(agentEventMsg{Kind: "confirm", Payload: "allow?", Confirm: resp})

			_, cmd := m.Update(key(answer.key))
			if cmd == nil {
				t.Fatal("answering the prompt returned no command")
			}
			select {
			case got := <-resp:
				if got != answer.want {
					t.Errorf("confirm answer = %v, want %v", got, answer.want)
				}
			default:
				t.Fatal("no answer delivered on the confirm channel")
			}
			if m.pendingConfirm != nil {
				t.Error("pendingConfirm still set after answering")
			}
		})
	}
}

func TestSpinner_TickStopsWhenIdleAndWhileConfirming(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)

	// Idle: no chain should be kept alive.
	if _, cmd := m.Update(spinner.TickMsg{}); cmd != nil {
		t.Error("spinner tick rechained while idle")
	}

	submit(t, m, "hello")
	if _, cmd := m.Update(spinner.TickMsg{ID: m.spinner.ID()}); cmd == nil {
		t.Error("spinner tick did not rechain while busy")
	}

	resp := make(chan bool, 1)
	m.Update(agentEventMsg{Kind: "confirm", Payload: "allow?", Confirm: resp})
	if _, cmd := m.Update(spinner.TickMsg{ID: m.spinner.ID()}); cmd != nil {
		t.Error("spinner kept ticking behind a confirmation prompt it isn't rendered in")
	}
}

func TestStatusLine_ReplacesTheInputAndFitsTheWidth(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	submit(t, m, "hello")

	if got := strings.Count(m.statusLine(), "\n"); got != 0 {
		t.Errorf("statusLine() spans %d extra rows, want exactly one row", got)
	}
	if !strings.Contains(m.statusLine(), "Thinking") {
		t.Errorf("statusLine() = %q, want a thinking indicator", m.statusLine())
	}

	m.Update(key("esc"))
	if !strings.Contains(m.statusLine(), "again") {
		t.Errorf("statusLine() = %q, want the armed hint once armed", m.statusLine())
	}
}

func TestStatusLine_ClampsToNarrowTerminals(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 24})
	submit(t, m, "hello")

	for _, name := range []string{"thinking", "confirming"} {
		if name == "confirming" {
			resp := make(chan bool, 1)
			m.Update(agentEventMsg{Kind: "confirm", Payload: "allow?", Confirm: resp})
		}
		if got := lipgloss.Width(m.statusLine()); got > 20 {
			t.Errorf("%s statusLine width = %d, want <= 20", name, got)
		}
	}
}

func TestView_OnlyPublishesACursorWhenTheInputIsVisible(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)

	if v := m.View(); v.Cursor == nil {
		t.Error("no cursor while idle, want one on the input")
	}

	submit(t, m, "hello")
	if v := m.View(); v.Cursor != nil {
		t.Error("cursor published while the input is off screen")
	}

	m.Update(agentEventMsg{Kind: "done"})
	if v := m.View(); v.Cursor == nil {
		t.Error("cursor not restored after the turn ended")
	}
}

func TestTruncateToWidth(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hell"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"héllo", 3, "hél"},
	}
	for _, c := range cases {
		if got := truncateToWidth(c.in, c.width); got != c.want {
			t.Errorf("truncateToWidth(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}
