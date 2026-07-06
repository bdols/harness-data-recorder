// Package ui implements the bubbletea TUI for browsing and resuming
// Claude Code sessions.
package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"

	"github.com/bdols/claude-session-browser/internal/backup"
	"github.com/bdols/claude-session-browser/internal/live"
	"github.com/bdols/claude-session-browser/internal/session"
)

type mode int

const (
	modeList mode = iota
	modeDetail
	modeConfirmDelete
	modeInspect
)

var (
	styleHeader   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "236", Dark: "252"}).Underline(true)
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("62"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "243"})
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("62"))
	styleUser     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	styleAsst     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "235", Dark: "252"})
	styleTool     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "243"})
	styleError    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	styleStatus   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "246"})
)

// Model is the top-level bubbletea model.
type Model struct {
	root      string
	backupDir string // empty disables backups
	sessions  []session.Session
	filtered  []session.Session
	cursor    int
	mode      mode
	width     int
	height    int

	filter    textinput.Model
	filtering bool

	// Content search (ripgrep across full session files).
	search       textinput.Model
	searching    bool           // input prompt active
	contentQuery string         // last executed query, "" = off
	matches      map[string]int // per-file match counts for contentQuery

	detail   viewport.Model
	detailOf *session.Session

	// live holds currently-running sessions keyed by session id,
	// refreshed alongside every scan.
	live map[string]live.Session

	// transcripts caches parsed session files for the preview pane,
	// keyed by file path; cleared on every rescan.
	transcripts map[string][]session.Entry

	status string
	err    string
}

// New builds the model. backupDir is where sessions are mirrored on
// startup and on demand; pass "" to disable backups.
func New(root, backupDir string) Model {
	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/"
	ti.CharLimit = 128
	si := textinput.New()
	si.Placeholder = "search all session content…"
	si.Prompt = "search: "
	si.CharLimit = 256
	return Model{root: root, backupDir: backupDir, filter: ti, search: si, live: map[string]live.Session{}, transcripts: map[string][]session.Entry{}}
}

type scannedMsg struct {
	sessions []session.Session
	err      error
}

type resumedMsg struct{ err error }

type liveMsg struct{ sessions map[string]live.Session }

type switchedMsg struct {
	window string
	err    error
}

type searchedMsg struct {
	query   string
	matches map[string]int
	err     error
}

type backedUpMsg struct {
	copied int
	total  int
	err    error
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{scanCmd(m.root), liveCmd()}
	if m.backupDir != "" {
		cmds = append(cmds, backupCmd(m.root, m.backupDir))
	}
	return tea.Batch(cmds...)
}

func liveCmd() tea.Cmd {
	return func() tea.Msg {
		return liveMsg{sessions: live.Scan()}
	}
}

func backupCmd(root, dest string) tea.Cmd {
	return func() tea.Msg {
		copied, total, err := backup.Sync(root, dest)
		return backedUpMsg{copied: copied, total: total, err: err}
	}
}

func searchCmd(root, query string) tea.Cmd {
	return func() tea.Msg {
		matches, err := session.SearchContent(root, query)
		return searchedMsg{query: query, matches: matches, err: err}
	}
}

func scanCmd(root string) tea.Cmd {
	return func() tea.Msg {
		s, err := session.Scan(root)
		return scannedMsg{sessions: s, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.detail.Width = msg.Width
		m.detail.Height = msg.Height - 3
		if m.detailOf != nil {
			m.setDetailContent()
		}
		return m, nil

	case scannedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.sessions = msg.sessions
		m.transcripts = map[string][]session.Entry{}
		m.applyFilter()
		return m, liveCmd()

	case liveMsg:
		m.live = msg.sessions
		return m, nil

	case switchedMsg:
		if msg.err != nil {
			m.err = "tmux: " + msg.err.Error()
		} else {
			m.status = "switched to tmux " + msg.window
		}
		return m, nil

	case searchedMsg:
		if msg.err != nil {
			m.err = "search: " + msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.status = ""
		m.contentQuery = msg.query
		m.matches = msg.matches
		m.cursor = 0
		m.applyFilter()
		return m, nil

	case backedUpMsg:
		if msg.err != nil {
			m.err = "backup: " + msg.err.Error()
		} else if msg.copied > 0 {
			m.status = fmt.Sprintf("backed up %d of %d sessions", msg.copied, msg.total)
		} else {
			m.status = fmt.Sprintf("backup up to date (%d sessions)", msg.total)
		}
		return m, nil

	case resumedMsg:
		if msg.err != nil {
			m.err = "resume failed: " + msg.err.Error()
		} else {
			m.status = "returned from session"
		}
		cmds := []tea.Cmd{scanCmd(m.root)}
		if m.backupDir != "" {
			cmds = append(cmds, backupCmd(m.root, m.backupDir))
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	if m.mode == modeDetail {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Content-search input captures keys while active.
	if m.searching {
		switch msg.String() {
		case "esc":
			m.searching = false
			m.search.SetValue("")
			m.search.Blur()
			return m, nil
		case "enter":
			m.searching = false
			m.search.Blur()
			q := strings.TrimSpace(m.search.Value())
			if q == "" {
				return m, nil
			}
			m.status = "searching…"
			return m, searchCmd(m.root, q)
		default:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			return m, cmd
		}
	}

	// Filter input captures most keys while active.
	if m.filtering {
		switch msg.String() {
		case "esc":
			m.filtering = false
			m.filter.SetValue("")
			m.filter.Blur()
			m.applyFilter()
			return m, nil
		case "enter":
			m.filtering = false
			m.filter.Blur()
			return m, nil
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.applyFilter()
			return m, cmd
		}
	}

	switch m.mode {
	case modeConfirmDelete:
		switch msg.String() {
		case "y", "Y":
			m.mode = modeList
			if s := m.selected(); s != nil {
				if err := os.Remove(s.Path); err != nil {
					m.err = err.Error()
				} else {
					m.status = "deleted " + s.ID
				}
			}
			return m, scanCmd(m.root)
		default:
			m.mode = modeList
			m.status = "delete cancelled"
			return m, nil
		}

	case modeInspect:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			m.mode = modeList
			return m.resume()
		case "w":
			return m.switchTmux()
		default: // any other key closes
			m.mode = modeList
			return m, nil
		}

	case modeDetail:
		switch msg.String() {
		case "q", "esc", "h", "left":
			m.mode = modeList
			m.detailOf = nil
			return m, nil
		case "i":
			m.mode = modeInspect
			m.detailOf = nil
			return m, nil
		case "w":
			return m.switchTmux()
		case "enter":
			return m.resume()
		case "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return m, cmd

	default: // modeList
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			}
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			m.cursor = len(m.filtered) - 1
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "/":
			m.filtering = true
			m.filter.Focus()
			return m, textinput.Blink
		case "s":
			m.searching = true
			m.search.Focus()
			return m, textinput.Blink
		case "esc":
			switch {
			case m.contentQuery != "":
				m.contentQuery = ""
				m.matches = nil
				m.status = ""
				m.search.SetValue("")
				m.applyFilter()
			case m.filter.Value() != "":
				m.filter.SetValue("")
				m.applyFilter()
			}
		case "r":
			m.status = "rescanning…"
			return m, tea.Batch(scanCmd(m.root), liveCmd())
		case "w":
			return m.switchTmux()
		case "b":
			if m.backupDir != "" {
				m.status = "backing up…"
				return m, backupCmd(m.root, m.backupDir)
			}
			m.status = "backups disabled (--no-auto-backup)"
		case "d":
			if m.selected() != nil {
				m.mode = modeConfirmDelete
			}
		case "i":
			if m.selected() != nil {
				m.mode = modeInspect
			}
		case "v", "l", "right", "tab":
			if s := m.selected(); s != nil {
				m.detailOf = s
				m.mode = modeDetail
				m.setDetailContent()
			}
		case "enter":
			return m.resume()
		}
		return m, nil
	}
}

func (m *Model) selected() *session.Session {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return nil
	}
	return &m.filtered[m.cursor]
}

func (m Model) resume() (tea.Model, tea.Cmd) {
	s := m.selected()
	if m.mode == modeDetail && m.detailOf != nil {
		s = m.detailOf
	}
	if s == nil {
		return m, nil
	}
	dir := s.CWD
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		m.err = fmt.Sprintf("cwd %s no longer exists", dir)
		return m, nil
	}
	bin := os.Getenv("CSB_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	c := exec.Command(bin, "--resume", s.ID)
	c.Dir = dir
	m.mode = modeList
	m.detailOf = nil
	m.status = ""
	return m, tea.ExecProcess(c, func(err error) tea.Msg {
		return resumedMsg{err: err}
	})
}

// switchTmux jumps the attached tmux client to the pane running the
// selected session, if it is live and inside tmux.
func (m Model) switchTmux() (tea.Model, tea.Cmd) {
	s := m.selected()
	if m.mode == modeDetail && m.detailOf != nil {
		s = m.detailOf
	}
	m.mode = modeList
	if s == nil {
		return m, nil
	}
	ls, ok := m.live[s.ID]
	if !ok {
		m.err = "session is not running"
		return m, nil
	}
	if ls.TmuxPane == "" {
		m.err = "session is running but not inside tmux"
		return m, nil
	}
	if !live.InTmux() {
		m.err = "run the browser inside tmux to switch windows"
		return m, nil
	}
	window := ls.TmuxWindow
	return m, func() tea.Msg {
		return switchedMsg{window: window, err: live.SwitchTmux(ls.TmuxPane)}
	}
}

func (m *Model) applyFilter() {
	terms := strings.Fields(strings.ToLower(m.filter.Value()))
	m.filtered = nil
	for _, s := range m.sessions {
		if m.contentQuery != "" && m.matches[s.Path] == 0 {
			continue
		}
		hay := strings.ToLower(s.CWD + " " + s.Title + " " + s.LastPrompt + " " + s.ID + " " + s.GitBranch)
		ok := true
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if ok {
			m.filtered = append(m.filtered, s)
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *Model) setDetailContent() {
	s := m.detailOf
	entries, err := session.LoadTranscript(s.Path)
	if err != nil {
		m.detail.SetContent(styleError.Render(err.Error()))
		return
	}
	w := m.width - 2
	if w < 20 {
		w = 78
	}
	var b strings.Builder
	for _, e := range entries {
		switch e.Role {
		case "user":
			b.WriteString(styleUser.Render("❯ "+wordwrap.String(e.Text, w-2)) + "\n\n")
		case "assistant":
			b.WriteString(styleAsst.Render(wordwrap.String(e.Text, w)) + "\n\n")
		case "tool":
			b.WriteString(styleTool.Render("  ⚙ "+truncate(e.Text, w-4)) + "\n")
		}
	}
	m.detail.SetContent(b.String())
	m.detail.GotoTop()
}

func (m Model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	switch m.mode {
	case modeDetail:
		return m.viewDetail()
	case modeInspect:
		return m.viewInspect()
	default:
		return m.viewList()
	}
}

// viewInspect shows a centered card of session metadata and stats.
func (m Model) viewInspect() string {
	s := m.selected()
	if s == nil {
		return m.viewList()
	}
	label := styleDim.Render
	row := func(k, v string) string {
		return fmt.Sprintf("%s  %s", label(fmt.Sprintf("%12s", k)), v)
	}
	dur := s.LastTime.Sub(s.FirstTime)
	tokens := "n/a"
	if s.OutputTokens > 0 || s.ContextTokens > 0 {
		tokens = fmt.Sprintf("%s output · %s context at last reply",
			humanCount(s.OutputTokens), humanCount(s.ContextTokens))
	}
	lines := []string{
		styleTitle.Render(truncate(s.Title, 64)),
		"",
		row("session id", s.ID),
		row("project", shortPath(s.CWD)),
		row("branch", orDash(s.GitBranch)),
		row("started", s.FirstTime.Local().Format("2006-01-02 15:04:05")),
		row("last active", fmt.Sprintf("%s (%s ago)", s.LastTime.Local().Format("2006-01-02 15:04:05"), relTime(s.LastTime))),
		row("span", humanDuration(dur)),
		row("messages", fmt.Sprintf("%d user · %d assistant · %d tool calls", s.UserMsgs, s.AsstMsgs, s.ToolCalls)),
		row("model", orDash(s.Model)),
		row("tokens", tokens),
		row("cli version", orDash(s.Version)),
		row("file", shortPath(s.Path)),
		row("size", humanSize(s.Size)),
	}
	footer := "enter resume · any other key to close"
	if ls, ok := m.live[s.ID]; ok {
		loc := "not in tmux"
		if ls.TmuxWindow != "" {
			loc = fmt.Sprintf("tmux %s (%s)", ls.TmuxWindow, ls.TmuxLabel)
			footer = "enter resume · w switch to tmux · any other key to close"
		}
		lines = append(lines, row("running", fmt.Sprintf("%s · pid %d · %s", ls.Status, ls.PID, loc)))
	}
	lines = append(lines, "", styleStatus.Render(footer))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Render(strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func (m Model) viewDetail() string {
	s := m.detailOf
	head := styleTitle.Render(truncate(s.Title, m.width-1)) + "\n" +
		styleDim.Render(truncate(fmt.Sprintf("%s · %s · %d msgs · %s", shortPath(s.CWD), s.Model, s.UserMsgs+s.AsstMsgs, s.ID), m.width-1))
	foot := styleStatus.Render("↑/↓ scroll · enter resume · esc back")
	return head + "\n" + m.detail.View() + "\n" + foot
}

// liveCell renders the one-column running indicator for a session: a
// dot colored by status (busy/waiting/idle), or a blank when the
// session isn't running.
func (m Model) liveCell(id string) string {
	ls, ok := m.live[id]
	if !ok {
		return " "
	}
	var c lipgloss.Color
	switch ls.Status {
	case "busy":
		c = lipgloss.Color("214") // orange: actively working
	case "waiting":
		c = lipgloss.Color("205") // pink: waiting on the user
	default:
		c = lipgloss.Color("42") // green: idle but alive
	}
	return lipgloss.NewStyle().Foreground(c).Render("●")
}

func (m Model) viewList() string {
	// Wide terminals split into a session list (left) and a live
	// transcript preview of the selection (right).
	previewOn := m.width >= 95
	leftW := m.width
	previewW := 0
	if previewOn {
		leftW = m.width * 45 / 100
		if leftW < 46 {
			leftW = 46
		}
		if leftW > 72 {
			leftW = 72
		}
		previewW = m.width - leftW - 3 // " │ " separator
	}

	timeW, projW := 4, 16
	msgsW, modelW := 0, 0
	titleW := leftW - timeW - projW - 8 // leading/trailing space + 2 gaps
	if !previewOn {
		projW, msgsW, modelW = 20, 4, 8
		titleW = leftW - timeW - projW - msgsW - modelW - 12
	}
	if titleW < 10 {
		titleW = 10
	}

	rows := m.height - 4 // header + context + status lines
	if rows < 1 {
		rows = 1
	}
	start := 0
	if m.cursor >= rows {
		start = m.cursor - rows + 1
	}
	end := start + rows
	if end > len(m.filtered) {
		end = len(m.filtered)
	}

	// Rows carry a leading one-column live indicator; the header gets a
	// matching blank column so titles stay aligned.
	leftLines := make([]string, 0, rows+1)
	if previewOn {
		leftLines = append(leftLines, " "+styleHeader.Render(fmt.Sprintf(" %*s  %-*s  %-*s ",
			timeW, "AGE", projW, "PROJECT", titleW, "TITLE")))
	} else {
		leftLines = append(leftLines, " "+styleHeader.Render(fmt.Sprintf(" %*s  %-*s  %-*s  %*s  %-*s ",
			timeW, "AGE", projW, "PROJECT", titleW, "TITLE", msgsW, "MSGS", modelW, "MODEL")))
	}
	for i := start; i < end; i++ {
		s := m.filtered[i]
		var body string
		if previewOn {
			body = fmt.Sprintf(" %*s  %-*s  %-*s ",
				timeW, relTime(s.LastTime),
				projW, truncate(projName(s.CWD), projW),
				titleW, truncate(s.Title, titleW))
		} else {
			body = fmt.Sprintf(" %*s  %-*s  %-*s  %*d  %-*s ",
				timeW, relTime(s.LastTime),
				projW, truncate(projName(s.CWD), projW),
				titleW, truncate(s.Title, titleW),
				msgsW, s.UserMsgs+s.AsstMsgs,
				modelW, truncate(shortModel(s.Model), modelW))
		}
		if i == m.cursor {
			body = styleSelected.Render(body)
		}
		leftLines = append(leftLines, m.liveCell(s.ID)+body)
	}
	blank := strings.Repeat(" ", leftW)
	for len(leftLines) < rows+1 {
		leftLines = append(leftLines, blank)
	}

	var b strings.Builder
	if previewOn {
		right := m.previewLines(previewW, rows+1)
		sep := styleDim.Render("│")
		for i := 0; i < rows+1; i++ {
			line := leftLines[i]
			if pad := leftW - lipgloss.Width(line); pad > 0 {
				line += strings.Repeat(" ", pad)
			}
			b.WriteString(line + " " + sep)
			if i < len(right) {
				b.WriteString(" " + right[i])
			}
			b.WriteString("\n")
		}
	} else {
		for _, line := range leftLines {
			b.WriteString(line + "\n")
		}
	}

	// Bottom: context line for selection + status/help.
	var ctx string
	if s := m.selected(); s != nil {
		line := fmt.Sprintf(" %s · last: %s", s.CWD, firstLineOf(s.LastPrompt))
		if m.contentQuery != "" {
			line = fmt.Sprintf(" %s · %d matches · last: %s", s.CWD, m.matches[s.Path], firstLineOf(s.LastPrompt))
		}
		ctx = styleDim.Render(truncate(line, m.width-1))
	}
	b.WriteString(ctx + "\n")

	switch {
	case m.mode == modeConfirmDelete:
		s := m.selected()
		b.WriteString(styleError.Render(fmt.Sprintf(" delete session %s? (y/N)", s.ID)))
	case m.searching:
		b.WriteString(" " + m.search.View())
	case m.filtering || m.filter.Value() != "":
		b.WriteString(" " + m.filter.View())
	case m.err != "":
		b.WriteString(styleError.Render(" " + m.err))
	case m.contentQuery != "":
		total := 0
		for _, s := range m.filtered {
			total += m.matches[s.Path]
		}
		b.WriteString(styleStatus.Render(fmt.Sprintf(" content:%q · %d sessions · %d matches  ·  enter resume · esc clear · s search again", m.contentQuery, len(m.filtered), total)))
	default:
		n := fmt.Sprintf(" %d sessions", len(m.filtered))
		if live := len(m.live); live > 0 {
			n += fmt.Sprintf(" · %s %d live", lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("●"), live)
		}
		if m.status != "" {
			n += " · " + m.status
		}
		b.WriteString(styleStatus.Render(n + "  ·  enter resume · v view · i inspect · w tmux · / filter · s search · b backup · d del · r rescan · q quit"))
	}
	return b.String()
}

// transcript returns cached parsed entries for a session file,
// loading and caching them on first use. The map is shared across
// Model copies, so caching from View is effective.
func (m Model) transcript(path string) ([]session.Entry, error) {
	if e, ok := m.transcripts[path]; ok {
		return e, nil
	}
	e, err := session.LoadTranscript(path)
	if err != nil {
		return nil, err
	}
	m.transcripts[path] = e
	return e, nil
}

// previewLines renders the tail of the selected session's transcript
// as at most h lines wrapped to width w.
func (m Model) previewLines(w, h int) []string {
	s := m.selected()
	if s == nil || w <= 0 || h <= 0 {
		return nil
	}
	entries, err := m.transcript(s.Path)
	if err != nil {
		return []string{styleError.Render(truncate(err.Error(), w))}
	}

	meta := fmt.Sprintf("%s · %s · %d msgs", shortPath(s.CWD), shortModel(s.Model), s.UserMsgs+s.AsstMsgs)
	if s.GitBranch != "" {
		meta += " · " + s.GitBranch
	}
	lines := []string{
		styleTitle.Render(truncate(s.Title, w)),
		styleDim.Render(truncate(meta, w)),
		"",
	}
	body := h - len(lines)
	if body <= 0 {
		return lines[:h]
	}

	// Render only the most recent entries; older ones can't fit anyway.
	start := len(entries) - 40
	if start < 0 {
		start = 0
	}
	var rendered []string
	for _, e := range entries[start:] {
		switch e.Role {
		case "user":
			for _, ln := range wrapLines("❯ "+e.Text, w) {
				rendered = append(rendered, styleUser.Render(ln))
			}
			rendered = append(rendered, "")
		case "assistant":
			for _, ln := range wrapLines(e.Text, w) {
				rendered = append(rendered, styleAsst.Render(ln))
			}
			rendered = append(rendered, "")
		case "tool":
			rendered = append(rendered, styleTool.Render(truncate("⚙ "+e.Text, w)))
		}
	}
	if len(rendered) > body {
		rendered = rendered[len(rendered)-body:]
	}
	return append(lines, rendered...)
}

// wrapLines word-wraps then hard-wraps text to width w, so even
// unbroken tokens (URLs, paths) cannot overflow the preview pane.
func wrapLines(text string, w int) []string {
	text = strings.ReplaceAll(text, "\t", "    ")
	return strings.Split(wrap.String(wordwrap.String(text, w), w), "\n")
}

func projName(cwd string) string {
	if cwd == "" {
		return "?"
	}
	return filepath.Base(cwd)
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func shortModel(m string) string {
	m = strings.TrimPrefix(m, "claude-")
	// drop trailing date suffix like -20251001
	parts := strings.Split(m, "-")
	if len(parts) > 1 {
		last := parts[len(parts)-1]
		if len(last) == 8 && strings.IndexFunc(last, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
			parts = parts[:len(parts)-1]
		}
	}
	return strings.Join(parts, "-")
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dmo", int(d.Hours()/(24*30)))
	}
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= w {
		return string(r)
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
