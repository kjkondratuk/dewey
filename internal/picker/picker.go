// Package picker provides an interactive terminal list for choosing one of
// dewey's recorded sessions to resume.
package picker

import (
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/kjkondratuk/dewey/internal/session"
)

// defaultWidth is used until the first tea.WindowSizeMsg arrives.
const defaultWidth = 100

// item wraps a session.Summary as a list.Item. Its title is the
// corresponding row from session.Columns, so the picker and the plain
// `--sessions` listing render identically.
type item struct {
	sum session.Summary
	row string
}

func (it item) FilterValue() string { return it.sum.FirstUserMessage }
func (it item) Title() string       { return it.row }
func (it item) Description() string { return "" }

// model is the pure, testable part of the picker: it has no TTY dependency,
// so its Update/View can be exercised directly in tests. newModel builds the
// initial list from sums at width; Choose is what actually runs it as a
// bubbletea program.
type model struct {
	list     list.Model
	sums     []session.Summary
	chosen   string
	quitting bool
}

func newModel(sums []session.Summary, width int) model {
	delegate := list.NewDefaultDelegate()
	delegate.ShowDescription = false
	delegate.SetSpacing(0)

	l := list.New(itemsFor(sums, width), delegate, width, 20)
	l.Title = "select a session to resume"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)

	return model{list: l, sums: sums}
}

// itemsFor builds the list items for sums at width, routed through
// session.Columns so a resize rebuilds rows at the new width rather than
// stretching stale ones.
func itemsFor(sums []session.Summary, width int) []list.Item {
	_, rows := session.Columns(sums, width)
	items := make([]list.Item, len(sums))
	for i, s := range sums {
		row := ""
		if i < len(rows) {
			row = rows[i]
		}
		items[i] = item{sum: s, row: row}
	}
	return items
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height)
		m.list.SetItems(itemsFor(m.sums, msg.Width))
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit

		case "esc":
			// While filtering (or with a filter applied) esc belongs to the
			// list -- it cancels/clears the filter. Only treat it as "quit
			// the picker" once there's no filter in play.
			if m.list.FilterState() == list.Unfiltered {
				m.quitting = true
				return m, tea.Quit
			}

		case "enter":
			// While actively typing a filter, enter applies it (handled by
			// the embedded list below) rather than choosing a session.
			if m.list.FilterState() != list.Filtering {
				if it, ok := m.list.SelectedItem().(item); ok {
					m.chosen = it.sum.ID
				}
				m.quitting = true
				return m, tea.Quit
			}
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() tea.View {
	return tea.NewView(m.list.View())
}

// Choose runs an interactive list and returns the id of the chosen session.
// Returns "" when the user quits without choosing, which is a normal outcome
// and not an error.
func Choose(sums []session.Summary) (string, error) {
	m := newModel(sums, defaultWidth)

	p := tea.NewProgram(m)
	final, err := p.Run()
	if err != nil {
		return "", err
	}

	fm, ok := final.(model)
	if !ok || !fm.quitting {
		return "", nil
	}
	return fm.chosen, nil
}
