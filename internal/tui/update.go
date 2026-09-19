package tui

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/search"
)

// Update routes every event. Key handling runs before the input
// component sees the message so navigation keys never leak into the
// query text; everything else falls through to the focused input,
// and any query change restarts the debounce window.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Reserve room for the "> " prompt plus the " delve
		// [semantic]" suffix so the mode indicator never wraps.
		if w := msg.Width - 24; w > 10 {
			m.input.Width = w
		} else {
			m.input.Width = 10
		}
		// Detail panel gets roughly the bottom half when open.
		m.viewport.Width = msg.Width - 4
		m.viewport.Height = msg.Height / 2
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			// Mode flip invalidates the current list: scores
			// from one engine are meaningless in the other.
			if m.mode == modeKeyword {
				m.mode = modeSemantic
			} else {
				m.mode = modeKeyword
			}
			m.results, m.cursor, m.expanded, m.errMsg = nil, 0, false, ""
			return m, m.fireSearch()
		case "up", "shift+tab":
			if m.cursor > 0 {
				m.cursor--
				m.refreshDetail()
			}
			return m, nil
		case "down":
			if m.cursor < len(m.results)-1 {
				m.cursor++
				m.refreshDetail()
			}
			return m, nil
		case "enter":
			// Read-only expand: shows the detail panel for the
			// row under the cursor. No file action happens here.
			if len(m.results) > 0 {
				m.expanded = !m.expanded
				m.refreshDetail()
			}
			return m, nil
		case "esc":
			if m.expanded {
				m.expanded = false
				return m, nil
			}
			m.input.SetValue("")
			m.results, m.cursor, m.errMsg = nil, 0, ""
			m.searching = false
			return m, nil
		}

	case searchTickMsg:
		// Only the latest generation searches: every keystroke
		// between scheduling and firing invalidates this tick.
		if msg.seq != m.seq {
			return m, nil
		}
		return m, m.runSearch(msg.seq)

	case searchDoneMsg:
		if msg.seq != m.seq {
			return m, nil // a newer query already fired
		}
		m.searching = false
		m.results = msg.results
		m.cursor, m.expanded, m.errMsg = 0, false, ""
		return m, nil

	case searchErrMsg:
		if msg.seq != m.seq {
			return m, nil
		}
		m.searching = false
		m.results = nil
		m.errMsg = msg.err.Error()
		return m, nil

	case spinner.TickMsg:
		if !m.searching {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	// Default: the query box owns the keystroke. A value change
	// restarts the debounce window; cursor keys and similar reach
	// here too but change nothing, so no search refires for them.
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds := []tea.Cmd{cmd}
	if m.input.Value() != before {
		cmds = append(cmds, m.fireSearch())
	}
	// Let the detail viewport scroll while it is open.
	if m.expanded {
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		cmds = append(cmds, vcmd)
	}
	return m, tea.Batch(cmds...)
}

// fireSearch bumps the debounce generation and arms a tick: the
// query is read when the tick fires, not now, so bursts of typing
// pay for exactly one engine call. Empty queries clear the list
// without touching SQLite at all.
func (m *Model) fireSearch() tea.Cmd {
	m.seq++
	m.searching = true
	m.errMsg = ""
	seq := m.seq
	return tea.Tick(debounceDelay, func(time.Time) tea.Msg {
		return searchTickMsg{seq: seq}
	})
}

// runSearch returns the async engine call for one debounced query.
// Keyword is a millisecond SQLite lookup; semantic embeds the query
// first (the slow step — the spinner covers it). Both reuse the
// Phase 2/4 functions unchanged.
func (m *Model) runSearch(seq int) tea.Cmd {
	query := m.input.Value()
	if query == "" {
		return func() tea.Msg { return searchDoneMsg{seq: seq} }
	}
	database, mode := m.database, m.mode
	return func() tea.Msg {
		if mode == modeSemantic {
			hits, err := search.SemanticSearch(database, query, searchLimit)
			if err != nil {
				return searchErrMsg{seq: seq, err: err}
			}
			out := make([]result, 0, len(hits))
			for _, h := range hits {
				out = append(out, result{record: h.Record, score: h.Score, hasScore: true})
			}
			return searchDoneMsg{seq: seq, results: out}
		}
		rows, err := db.SearchFiles(database, query, searchLimit, "")
		if err != nil {
			return searchErrMsg{seq: seq, err: err}
		}
		out := make([]result, 0, len(rows))
		for _, r := range rows {
			out = append(out, result{record: r})
		}
		return searchDoneMsg{seq: seq, results: out}
	}
}

// refreshDetail rebuilds the viewport for the row under the cursor
// after navigation or expand. Kept in tui.go (not view.go) because
// it mutates model state.
func (m *Model) refreshDetail() {
	if !m.expanded || len(m.results) == 0 {
		return
	}
	m.viewport.SetContent(detailBody(m.results[m.cursor]))
	m.viewport.GotoTop()
}

// Run opens the index, refuses headless use with a clean error
// (piped/CI output can't render an interactive UI), and blocks until
// the user quits. The database closes before return, so quitting
// mid-search can't corrupt the index.
func Run() error {
	info, err := os.Stdout.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("no terminal detected: `delve` without arguments opens the interactive browser, which needs a TTY (try `delve search <query>` for scripted use)")
	}

	database, err := db.InitDB()
	if err != nil {
		return err
	}
	defer database.Close()

	model := New(database)
	// Alt-screen keeps the shell history clean: quitting restores
	// exactly what was there before, no scrollback litter.
	program := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("browser failed: %w", err)
	}
	return nil
}
