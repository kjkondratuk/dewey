package app

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/kjkondratuk/dewey/internal/agent"
)

type agentEventMsg agent.Event

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

type App struct {
	Agent          agent.Agent
	Conversation   viewport.Model
	Entry          textinput.Model
	agentChan      <-chan agent.Event
	cancel         context.CancelFunc
	renderer       *glamour.TermRenderer
	turns          []turn
	streaming      bool
	pendingConfirm chan<- bool
	width, height  int
}

func (m *App) Init() tea.Cmd {
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
			return m, waitForAgentEvent(ch)
		}

	case agentEventMsg:
		switch msg.Kind {
		case "cancelled":
			m.streaming = false
			m.cancel = nil
			m.newLine("system", "Cancelled")
			return m, nil
		case "confirm":
			m.streaming = false
			m.pendingConfirm = msg.Confirm
			m.newLine("system", "```\n"+msg.Payload+"\n```")
			// Do not resume waitForAgentEvent yet — the agent goroutine is
			// blocked on Confirm, not on the event channel. Resumed from
			// the pendingConfirm branch in the KeyPressMsg handler above.
			return m, nil
		case "chunk":
			m.appendChunk(msg.Payload)
			return m, waitForAgentEvent(m.agentChan)
		case "tool":
			// Close out the in-flight assistant block so the tool status
			// line renders as its own turn, and the reply that follows the
			// tool call starts a fresh block.
			m.streaming = false
			m.newLine("system", "Running "+msg.Payload+"...")
			return m, waitForAgentEvent(m.agentChan)
		case "error":
			m.streaming = false
			m.cancel = nil
			m.newLine("error", "Error: "+msg.Payload)
			return m, nil
		case "done":
			m.streaming = false
			m.cancel = nil
			return m, nil
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
	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left,
		m.Conversation.View(),
		m.Entry.View(),
	))
	v.AltScreen = true

	// Only the root places a real cursor; the entry reports its own position
	// relative to its top-left corner.
	if cur := m.Entry.Cursor(); cur != nil {
		cur.Position.Y += m.height - 1
		v.Cursor = cur
	}
	return v
}
