package app

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/kjkondratuk/dewey/internal/agent"
)

// abortArmWindow is how long a first escape stays armed before a second one
// stops counting as a deliberate double-press.
const abortArmWindow = 1500 * time.Millisecond

type agentEventMsg agent.Event

// abortDisarmMsg fires when a first escape has gone unanswered long enough
// that it should no longer arm an abort.
type abortDisarmMsg struct{}

func waitForAgentEvent(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return agentEventMsg{Kind: "done"}
		}
		return agentEventMsg(ev)
	}
}

type turn struct {
	role string // "user" | "assistant" | "error" | "system"
	text string // raw markdown, never pre-rendered
}

var blockStyles = map[string]lipgloss.Style{
	"user": lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(0, 1),
	"assistant": lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1),
	"error": lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("196")).
		Padding(0, 1),
	"system": lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1),
}

var statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

type App struct {
	Agent          agent.Agent
	Conversation   viewport.Model
	Entry          textinput.Model
	agentChan      <-chan agent.Event
	cancel         context.CancelFunc
	renderer       *glamour.TermRenderer
	spinner        spinner.Model
	turns          []turn
	streaming      bool
	pendingConfirm chan<- bool
	// busy is true from the moment a query is submitted until the turn ends.
	// While busy the input is disabled, which also prevents a second Run()
	// from racing the first one on the agent's history.
	busy bool
	// abortArmed is set by a first escape press while busy; a second press
	// within abortArmWindow cancels the turn.
	abortArmed    bool
	width, height int
}

func (m *App) Init() tea.Cmd {
	m.spinner = spinner.New(spinner.WithSpinner(spinner.MiniDot))
	return m.Entry.Focus()
}

func (m *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyPressMsg:
		if m.pendingConfirm != nil {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "y":
				m.pendingConfirm <- true
				m.pendingConfirm = nil
				m.newLine("system", "Allowed.")
				return m, waitForAgentEvent(m.agentChan)
			case "n":
				m.pendingConfirm <- false
				m.pendingConfirm = nil
				m.newLine("system", "Denied.")
				return m, waitForAgentEvent(m.agentChan)
			case "esc":
				m.pendingConfirm = nil
				if m.cancel != nil {
					m.cancel()
				}
				return m, waitForAgentEvent(m.agentChan)
			case "pageup":
				m.Conversation.PageUp()
				return m, nil
			case "pagedown":
				m.Conversation.PageDown()
				return m, nil
			case "scrollup", "up":
				m.Conversation.ScrollUp(3)
				return m, nil
			case "scrolldown", "down":
				m.Conversation.ScrollDown(3)
				return m, nil
			}
			return m, nil
		}

		// While a turn is in flight the input is disabled: scrolling and
		// quitting still work, aborting takes two escapes, and every other
		// key is swallowed so a second query can't be submitted.
		if m.busy {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				if m.abortArmed {
					m.abortArmed = false
					if m.cancel != nil {
						m.cancel()
					}
					return m, nil
				}
				m.abortArmed = true
				return m, tea.Tick(abortArmWindow, func(time.Time) tea.Msg {
					return abortDisarmMsg{}
				})
			case "pageup":
				m.Conversation.PageUp()
				return m, nil
			case "pagedown":
				m.Conversation.PageDown()
				return m, nil
			case "scrollup", "up":
				m.Conversation.ScrollUp(3)
				return m, nil
			case "scrolldown", "down":
				m.Conversation.ScrollDown(3)
				return m, nil
			}
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "esc":
			if m.cancel != nil {
				m.cancel()
			}
			return m, nil

		case "pageup":
			m.Conversation.PageUp()
			return m, nil

		case "pagedown":
			m.Conversation.PageDown()
			return m, nil

		case "scrollup", "up":
			m.Conversation.ScrollUp(3)
			return m, nil

		case "scrolldown", "down":
			m.Conversation.ScrollDown(3)
			return m, nil

		case "scrollleft", "left":
			m.Conversation.ScrollLeft(3)
			return m, nil

		case "scrollright", "right":
			m.Conversation.ScrollRight(3)
			return m, nil

		case "enter":
			m.newLine("user", m.Entry.Value())
			ctx, cancel := context.WithCancel(context.Background())
			m.cancel = cancel
			ch, err := m.Agent.Run(ctx, m.Entry.Value())
			m.Entry.SetValue("")
			if err != nil {
				m.cancel = nil
				m.newLine("error", "Error: "+err.Error())
				return m, nil
			}
			m.agentChan = ch
			m.busy = true
			m.abortArmed = false
			m.Entry.Blur()
			return m, tea.Batch(waitForAgentEvent(ch), m.spinner.Tick)
		}

	case abortDisarmMsg:
		m.abortArmed = false
		return m, nil

	case spinner.TickMsg:
		// Only keep the tick chain alive while busy, so it stops on its own
		// once the turn ends rather than spinning forever.
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case agentEventMsg:
		switch msg.Kind {
		case "cancelled":
			m.streaming = false
			m.cancel = nil
			m.newLine("system", "Cancelled")
			return m, m.finishTurn()
		case "confirm":
			m.streaming = false
			m.pendingConfirm = msg.Confirm
			// The agent emits complete markdown (fencing only the argument
			// text), so render it verbatim rather than fencing it again.
			m.newLine("system", msg.Payload)
			// Do not resume waitForAgentEvent yet — the agent goroutine is
			// blocked on Confirm, not on the event channel. Resumed from
			// the pendingConfirm branch in the KeyPressMsg handler above.
			return m, nil
		case "compacted":
			m.newLine("system", msg.Payload)
			return m, waitForAgentEvent(m.agentChan)
		case "chunk":
			m.appendChunk(msg.Payload)
			return m, waitForAgentEvent(m.agentChan)
		case "tool":
			// Close out the in-flight assistant block so the tool status
			// line renders as its own turn, and the reply that follows the
			// tool call starts a fresh block.
			m.streaming = false
			m.newLine("system", msg.Payload)
			return m, waitForAgentEvent(m.agentChan)
		case "error":
			m.streaming = false
			m.cancel = nil
			m.newLine("error", "Error: "+msg.Payload)
			return m, m.finishTurn()
		case "done":
			m.streaming = false
			m.cancel = nil
			return m, m.finishTurn()
		}
	}

	var cmd tea.Cmd
	m.Entry, cmd = m.Entry.Update(msg)
	return m, cmd
}

func (m *App) render() {
	blocks := make([]string, len(m.turns))
	for i, t := range m.turns {
		text := t.text
		if m.renderer != nil {
			if out, err := m.renderer.Render(text); err == nil {
				text = strings.TrimRight(out, "\n")
			}
		}
		style := blockStyles[t.role]
		blocks[i] = style.Width(m.width - style.GetHorizontalFrameSize()).Render(text)
	}
	m.Conversation.SetContent(strings.Join(blocks, "\n\n"))
	m.Conversation.GotoBottom()
}

// finishTurn clears the in-flight state and hands the input back to the user.
func (m *App) finishTurn() tea.Cmd {
	m.busy = false
	m.abortArmed = false
	return m.Entry.Focus()
}

// statusLine is what replaces the input while a turn is in flight. It stands
// in for the entry rather than occupying an extra row, so the conversation
// viewport never resizes mid-turn.
func (m *App) statusLine() string {
	if m.pendingConfirm != nil {
		return statusStyle.Render("Awaiting your approval — y to allow, n to deny, esc to cancel")
	}

	hint := "esc esc to abort"
	if m.abortArmed {
		hint = "press esc again to abort"
	}
	return m.spinner.View() + " " + statusStyle.Render("Thinking… ("+hint+")")
}

// newLine commits a complete turn (the user's prompt, an error, a system note).
func (m *App) newLine(role, text string) {
	m.turns = append(m.turns, turn{role: role, text: text})
	m.render()
}

// appendChunk grows the in-flight assistant turn instead of starting a new one.
func (m *App) appendChunk(text string) {
	if !m.streaming {
		m.turns = append(m.turns, turn{role: "assistant"})
		m.streaming = true
	}
	m.turns[len(m.turns)-1].text += text
	m.render()
}

func (m *App) layout() {
	m.Conversation.SetWidth(m.width)
	m.Conversation.SetHeight(m.height - 1)
	m.Entry.SetWidth(m.width)

	blockFrame := blockStyles["assistant"].GetHorizontalFrameSize()
	m.renderer, _ = glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(m.width-blockFrame),
	)
	m.render()
}

func (m *App) View() tea.View {
	bottom := m.Entry.View()
	if m.busy {
		bottom = m.statusLine()
	}

	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left,
		m.Conversation.View(),
		bottom,
	))
	v.AltScreen = true

	// Only the root places a real cursor; the entry reports its own position
	// relative to its top-left corner. While busy the entry isn't on screen,
	// so there's nowhere to put one.
	if cur := m.Entry.Cursor(); cur != nil && !m.busy {
		cur.Position.Y += m.height - 1
		v.Cursor = cur
	}
	return v
}
