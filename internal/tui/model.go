// Package tui is Delve's interactive terminal browser: a live
// search box over the existing keyword + semantic engine, with a
// read-only detail panel. It reuses engine functions only —
// db.SearchFiles, search.SemanticSearch — and never reimplements
// ranking, extraction, or storage.
package tui

import (
	"database/sql"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/NoahMenezes/Delve/internal/db"
)

// How long typing must pause before a search fires, how many rows a
// search returns, and how much preview text the detail panel shows.
const (
	debounceDelay = 350 * time.Millisecond
	searchLimit   = 20
	snippetRunes  = 500
)

// searchMode selects the engine: keyword (FTS5 exact match) or
// semantic (embedding cosine). Tab flips it; the bar always shows
// which is active.
type searchMode int

const (
	modeKeyword searchMode = iota
	modeSemantic
)

// result is one displayed row: the engine's record plus the semantic
// score when there is one. Keyword rows carry hasScore=false and
// render no SCORE column.
type result struct {
	record   db.FileRecord
	score    float64
	hasScore bool
}

// Messages drive the async search pipeline (see the Elm comment on
// Model): ticks collapse keystrokes, done/err deliver engine output.
type searchTickMsg struct{ seq int }
type searchDoneMsg struct {
	seq     int
	results []result
}
type searchErrMsg struct {
	seq int
	err error
}

// Model is the Bubble Tea state. THE ELM PATTERN, briefly: the UI is
// a pure function of Model (View), every event arrives as a message
// (Update), and side effects (SQLite, embedding inference) happen in
// tea.Cmd closures that send their outcome back as messages. Blocking
// work never runs inside Update, so typing stays responsive while a
// semantic search embeds in the background.
type Model struct {
	database *sql.DB
	input    textinput.Model
	viewport viewport.Model
	spinner  spinner.Model

	mode      searchMode
	results   []result
	cursor    int
	seq       int // debounce generation: stale ticks/queries are dropped
	searching bool
	errMsg    string
	expanded  bool // detail panel visible for results[cursor]
	width     int
	height    int
}

// New builds the initial model around an open database handle. The
// caller owns the handle and closes it after the program quits.
func New(database *sql.DB) Model {
	in := textinput.New()
	in.Placeholder = "search files…"
	in.Focus()
	in.Prompt = "> "

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	vp := viewport.New(0, 0)

	return Model{database: database, input: in, viewport: vp, spinner: sp}
}

// Init starts the cursor blinking; the spinner ticks only while a
// search is in flight (toggled in Update).
func (m Model) Init() tea.Cmd {
	return textinput.Blink
}
