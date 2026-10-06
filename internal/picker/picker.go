// Package picker provides an interactive terminal list for choosing one of
// dewey's recorded sessions to resume.
package picker

import (
	"strings"

	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/kjkondratuk/dewey/internal/session"
)

// defaultWidth is used until the first tea.WindowSizeMsg arrives.
const defaultWidth = 100

// defaultHeight is the table's viewport height.
const defaultHeight = 20

// Fixed-format column widths; see computeColumns for PROJECT and SUMMARY,
// which are sized from content instead.
const (
	idWidth         = 8
	msgsWidth       = 4
	startedWidth    = 16
	maxProjectWidth = 16
	minSummaryWidth = 20

	// perColumnPadding is the horizontal padding bubbles/table's default
	// Cell/Header styles add around every column (Padding(0, 1): one space
	// each side). Column-width math has to account for it, since a column's
	// Width is content width, not rendered width.
	perColumnPadding = 2
)

// pickerMode distinguishes navigating the table from typing a filter --
// typing cannot do both at once, since keys like "j"/"k" are letters too.
type pickerMode int

const (
	modeNormal pickerMode = iota
	modeFilter
)

// model is the pure, testable part of the picker: it has no TTY dependency,
// so its Update/View can be exercised directly in tests. newModel builds the
// initial table from sums at width; Choose is what actually runs it as a
// bubbletea program.
type model struct {
	table  table.Model
	filter textinput.Model
	mode   pickerMode

	sums []session.Summary // the full, unfiltered set
	// visible is the summary behind each currently-displayed table row, in
	// the same order. The table's cursor indexes *this* slice, never sums --
	// resolving a chosen id through sums after filtering returns the wrong
	// session.
	visible []session.Summary

	width    int
	chosen   string
	quitting bool
}

func newModel(sums []session.Summary, width int) model {
	_, rows := session.Cells(sums)

	t := table.New(
		table.WithColumns(computeColumns(sums, width)),
		table.WithRows(toTableRows(rows)),
		table.WithFocused(true),
		table.WithHeight(defaultHeight),
		table.WithWidth(width),
	)

	ti := textinput.New()
	ti.Placeholder = "filter"

	return model{
		table:   t,
		filter:  ti,
		sums:    sums,
		visible: sums,
		width:   width,
	}
}

// computeColumns sizes the table's columns from content and the available
// width. ID, MSGS and STARTED are fixed-format fields; PROJECT grows to the
// widest project cell (capped, so one long project name can't blow out the
// table); SUMMARY takes whatever width is left, floored so it stays usable
// in a narrow terminal.
func computeColumns(sums []session.Summary, width int) []table.Column {
	_, rows := session.Cells(sums)

	projectWidth := len("PROJECT")
	for _, row := range rows {
		if len(row) <= 3 {
			continue
		}
		if w := len([]rune(row[3])); w > projectWidth {
			projectWidth = w
		}
	}
	if projectWidth > maxProjectWidth {
		projectWidth = maxProjectWidth
	}

	fixed := (idWidth + perColumnPadding) +
		(startedWidth + perColumnPadding) +
		(msgsWidth + perColumnPadding) +
		(projectWidth + perColumnPadding)
	summaryWidth := width - fixed - perColumnPadding
	if summaryWidth < minSummaryWidth {
		summaryWidth = minSummaryWidth
	}

	return []table.Column{
		{Title: "ID", Width: idWidth},
		{Title: "STARTED", Width: startedWidth},
		{Title: "MSGS", Width: msgsWidth},
		{Title: "PROJECT", Width: projectWidth},
		{Title: "SUMMARY", Width: summaryWidth},
	}
}

func toTableRows(rows [][]string) []table.Row {
	out := make([]table.Row, len(rows))
	for i, r := range rows {
		out[i] = table.Row(r)
	}
	return out
}

// matchesFilter reports whether any cell of row contains query, case
// insensitively. An empty query matches everything.
func matchesFilter(row []string, query string) bool {
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	for _, cell := range row {
		if strings.Contains(strings.ToLower(cell), q) {
			return true
		}
	}
	return false
}

// applyFilter recomputes the visible rows from the current filter value and
// resets the cursor to 0, so it can never point past the end of a shorter,
// filtered list.
func (m *model) applyFilter() {
	_, rows := session.Cells(m.sums)
	query := m.filter.Value()

	visible := make([]session.Summary, 0, len(m.sums))
	visibleRows := make([]table.Row, 0, len(rows))
	for i, row := range rows {
		if matchesFilter(row, query) {
			visible = append(visible, m.sums[i])
			visibleRows = append(visibleRows, table.Row(row))
		}
	}

	m.visible = visible
	m.table.SetRows(visibleRows)
	m.table.SetCursor(0)
}

// choose records the currently highlighted row as the chosen session,
// resolved through visible (the filtered set) rather than sums -- the fix
// for the classic filter-picker bug where the cursor indexes filtered rows
// but the id gets looked up in the original, unfiltered slice.
func (m *model) choose() {
	cur := m.table.Cursor()
	if cur < 0 || cur >= len(m.visible) {
		return
	}
	m.chosen = m.visible[cur].ID
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.table.SetColumns(computeColumns(m.sums, msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(msg.Height)
		return m, nil

	case tea.KeyPressMsg:
		if m.mode == modeFilter {
			switch msg.String() {
			case "enter":
				// Applies the filter by returning to normal mode, keeping
				// whatever is currently filtered.
				m.mode = modeNormal
				m.filter.Blur()
				return m, nil
			case "esc":
				// Cancels the filter: back to normal mode with the full set
				// restored.
				m.mode = modeNormal
				m.filter.Blur()
				m.filter.SetValue("")
				m.applyFilter()
				return m, nil
			}

			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.applyFilter()
			return m, cmd
		}

		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit

		case "enter":
			m.choose()
			m.quitting = true
			return m, tea.Quit

		case "/":
			m.mode = modeFilter
			cmd := m.filter.Focus()
			return m, cmd
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) View() tea.View {
	if m.mode == modeFilter {
		return tea.NewView(m.table.View() + "\n/" + m.filter.View())
	}
	return tea.NewView(m.table.View())
}

// Choose runs an interactive table-based picker and returns the id of the
// chosen session. Returns "" when the user quits without choosing, which is
// a normal outcome and not an error.
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
