// Package ui implements the bubbletea TUI for browsing and resuming
// Claude Code sessions.
package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	modeConfirmRestore
	modeVersions
	modeRunning
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

// keyBar lists the modeList bindings. It lives on its own line so
// transient status (search results, filters, errors) never hides the
// keys that remain available — i inspect, w tmux, etc. all still work
// while a search or filter is active.
const keyBar = "enter resume · v view · i inspect · V versions · e export · R running · w tmux · / filter · s search · b backup · d del · r rescan · q quit"

// Model is the top-level bubbletea model.
type Model struct {
	root       string
	backupDir  string // where backups live; empty only if no home dir
	autoBackup bool   // sync on launch (disabled by --no-auto-backup)
	sessions   []session.Session
	filtered   []session.Session
	cursor     int
	mode       mode
	width      int
	height     int

	filter    textinput.Model
	filtering bool

	// Content search (ripgrep across full session files).
	search       textinput.Model
	searching    bool           // input prompt active
	contentQuery string         // last executed query, "" = off
	matches      map[string]int // per-file match counts for contentQuery

	detail   viewport.Model
	detailOf *session.Session
	// detailBack is the mode to return to when leaving the detail view
	// (modeList normally, modeVersions when viewing a specific version).
	detailBack mode

	// Version browser state for the selected session.
	versions      []versionRow
	versionOf     *session.Session
	versionCursor int

	// pending holds a session awaiting a restore-then-resume confirmation;
	// pendingVersion, when set, is the specific version file to restore
	// (otherwise the most complete version is chosen).
	pending        *session.Session
	pendingVersion string

	// live holds currently-running sessions keyed by session id,
	// refreshed alongside every scan.
	live map[string]live.Session
	// reaped is the set of running session ids whose transcript file no
	// longer exists in Claude's projects dir — rescue candidates.
	reaped map[string]bool

	// Running-sessions (rescue) browser state.
	running       []runningRow
	runningCursor int

	// transcripts caches parsed session files for the preview pane,
	// keyed by file path; cleared on every rescan.
	transcripts map[string][]session.Entry

	status string
	err    string
}

// New builds the model. backupDir is where session versions are stored
// (used for both backup and restore); autoBackup controls whether a sync
// runs on launch.
func New(root, backupDir string, autoBackup bool) Model {
	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Prompt = "/"
	ti.CharLimit = 128
	si := textinput.New()
	si.Placeholder = "search all session content…"
	si.Prompt = "search: "
	si.CharLimit = 256
	return Model{root: root, backupDir: backupDir, autoBackup: autoBackup, filter: ti, search: si, live: map[string]live.Session{}, transcripts: map[string][]session.Entry{}}
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
	if m.autoBackup && m.backupDir != "" {
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
		// A running session whose transcript file is gone from Claude's
		// projects dir is a rescue candidate: its content lives only in the
		// running process now.
		m.reaped = map[string]bool{}
		for id := range m.live {
			if !hasTranscript(id) {
				m.reaped[id] = true
			}
		}
		if m.mode == modeRunning {
			m.refreshRunning()
		}
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
			m.status = fmt.Sprintf("saved %d new session version(s)", msg.copied)
		} else {
			m.status = fmt.Sprintf("backups current (%d sessions)", msg.total)
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

	case modeConfirmRestore:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "y", "Y":
			s := m.pending
			if s == nil {
				m.mode = modeList
				return m, nil
			}
			var dst string
			var err error
			if m.pendingVersion != "" {
				// Defense in depth: never overwrite a running session's
				// transcript, even if a guard upstream was missed.
				if m.isRunning(s.ID) {
					m.mode = modeList
					m.detailOf, m.pending, m.pendingVersion = nil, nil, ""
					m.err = "session is running — restore aborted"
					return m, nil
				}
				// Restore a specific chosen version (the current live state,
				// if any, is captured first inside RestoreVersion).
				dst, err = backup.RestoreVersion(m.restoreDir(), session.DefaultRoot(), s.ProjectDir, s.ID, m.pendingVersion)
			} else {
				// Default: most complete version. Restore refuses to
				// overwrite a live file, so the destination is clear.
				dst, err = backup.Restore(m.restoreDir(), session.DefaultRoot(), s.ID)
			}
			if err != nil {
				m.mode = modeList
				m.detailOf, m.pending, m.pendingVersion = nil, nil, ""
				m.err = "restore: " + err.Error()
				return m, nil
			}
			m.status = "restored " + filepath.Base(dst)
			return m.launchClaude(s)
		default: // any other key cancels
			m.mode = modeList
			m.detailOf, m.pending, m.pendingVersion = nil, nil, ""
			m.status = "restore cancelled"
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
		case "V":
			if s := m.selected(); s != nil {
				m.openVersions(s)
			}
			return m, nil
		default: // any other key closes
			m.mode = modeList
			return m, nil
		}

	case modeVersions:
		return m.handleVersionsKey(msg)

	case modeRunning:
		return m.handleRunningKey(msg)

	case modeDetail:
		switch msg.String() {
		case "q", "esc", "h", "left":
			back := m.detailBack
			m.detailBack = modeList
			m.mode = back
			m.detailOf = nil
			return m, nil
		case "i":
			m.mode = modeInspect
			m.detailBack = modeList
			m.detailOf = nil
			return m, nil
		case "e":
			if m.detailOf != nil {
				return m.exportMarkdownFile(*m.detailOf, "")
			}
		case "w":
			return m.switchTmux()
		case "enter":
			// Resuming while viewing a specific version restores that
			// version rather than the (possibly newer) live transcript.
			if m.detailBack == modeVersions && m.versionOf != nil && m.detailOf != nil {
				if m.isRunning(m.versionOf.ID) {
					m.detailOf = nil
					m.detailBack = modeList
					m.mode = modeVersions
					m.status = "session is running — stop it (w to switch) before restoring a version"
					return m, nil
				}
				m.pending = m.versionOf
				m.pendingVersion = m.detailOf.Path
				m.detailOf = nil
				m.detailBack = modeList
				m.mode = modeConfirmRestore
				return m, nil
			}
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
			m.status = "no backup location available"
		case "d":
			if m.selected() != nil {
				m.mode = modeConfirmDelete
			}
		case "i":
			if m.selected() != nil {
				m.mode = modeInspect
			}
		case "V":
			if s := m.selected(); s != nil {
				m.openVersions(s)
			}
		case "R":
			if len(m.live) > 0 {
				m.openRunning()
			} else {
				m.status = "no running sessions detected"
			}
		case "e":
			if s := m.selected(); s != nil {
				return m.exportMarkdownFile(*s, "")
			}
		case "v", "l", "right", "tab":
			if s := m.selected(); s != nil {
				m.detailOf = s
				m.detailBack = modeList
				m.mode = modeDetail
				m.setDetailContent()
			}
		case "enter":
			return m.resume()
		}
		return m, nil
	}
}

// handleRunningKey drives the running-sessions (rescue) browser.
func (m Model) handleRunningKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "h", "left":
		m.mode = modeList
		m.running = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.runningCursor > 0 {
			m.runningCursor--
		}
	case "down", "j":
		if m.runningCursor < len(m.running)-1 {
			m.runningCursor++
		}
	case "g", "home":
		m.runningCursor = 0
	case "G", "end":
		m.runningCursor = len(m.running) - 1
		if m.runningCursor < 0 {
			m.runningCursor = 0
		}
	case "r":
		m.status = "rescanning…"
		return m, tea.Batch(scanCmd(m.root), liveCmd())
	case "enter", "w", "l", "right":
		if rr := m.selectedRunning(); rr != nil {
			return m.switchToLive(rr.Session)
		}
	}
	return m, nil
}

// handleVersionsKey drives the version browser: move the selection, view
// a version's transcript, or restore-and-resume a chosen version.
func (m Model) handleVersionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "h", "left":
		m.mode = modeList
		m.versions = nil
		m.versionOf = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.versionCursor > 0 {
			m.versionCursor--
		}
	case "down", "j":
		if m.versionCursor < len(m.versions)-1 {
			m.versionCursor++
		}
	case "g", "home":
		m.versionCursor = 0
	case "G", "end":
		m.versionCursor = len(m.versions) - 1
		if m.versionCursor < 0 {
			m.versionCursor = 0
		}
	case "w":
		return m.switchTmux()
	case "v", "l", "right":
		if v := m.selectedVersion(); v != nil {
			// Viewing a version's transcript is always safe, even while the
			// session is running.
			vs := *m.versionOf
			vs.Path = v.Path
			vs.Title = m.versionOf.Title + "  ·  version " + v.Time.Local().Format("2006-01-02 15:04:05")
			m.detailOf = &vs
			m.detailBack = modeVersions
			m.mode = modeDetail
			m.setDetailContent()
		}
	case "e":
		if v := m.selectedVersion(); v != nil {
			vs, err := session.ParseFile(v.Path)
			if err != nil {
				m.err = "export: " + err.Error()
				return m, nil
			}
			vs.ID = m.versionOf.ID // the version filename isn't the id
			return m.exportMarkdownFile(vs, v.Time.Local().Format("20060102-1504"))
		}
	case "enter":
		if v := m.selectedVersion(); v != nil {
			if m.isRunning(m.versionOf.ID) {
				// Guard: don't overwrite a running session's transcript.
				m.status = "session is running — stop it (w to switch) before restoring a version"
				return m, nil
			}
			m.pending = m.versionOf
			m.pendingVersion = v.Path
			m.mode = modeConfirmRestore
		}
	}
	return m, nil
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
	// If the transcript isn't in Claude Code's own projects dir (we're
	// browsing backups, or it was reaped) but a backup exists, confirm a
	// restore before resuming rather than handing Claude a missing id.
	if !liveExists(s) && len(backup.Versions(m.restoreDir(), s.ID)) > 0 {
		m.pending = s
		m.mode = modeConfirmRestore
		return m, nil
	}
	return m.launchClaude(s)
}

// launchClaude resumes s via `claude --resume` in its original cwd. The
// transcript is assumed present in Claude's projects dir by now.
func (m Model) launchClaude(s *session.Session) (tea.Model, tea.Cmd) {
	m.mode = modeList
	m.detailOf, m.pending, m.pendingVersion = nil, nil, ""
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
	m.status = ""
	return m, tea.ExecProcess(c, func(err error) tea.Msg {
		return resumedMsg{err: err}
	})
}

// livePath is where Claude Code itself keeps this session's transcript,
// regardless of which root the browser is scanning.
func livePath(s *session.Session) string {
	return filepath.Join(session.DefaultRoot(), s.ProjectDir, s.ID+".jsonl")
}

func liveExists(s *session.Session) bool {
	_, err := os.Stat(livePath(s))
	return err == nil
}

// restoreDir is where restore-on-resume reads backups from, falling back
// to the default location when auto-backup was disabled for this run.
func (m Model) restoreDir() string {
	if m.backupDir != "" {
		return m.backupDir
	}
	return backup.DefaultDir()
}

// versionRow is one entry in the version browser.
type versionRow struct {
	backup.Version
	records int  // record (line) count, for a rough sense of content
	largest bool // the most complete version (restore default)
	live    bool // content matches the current live transcript
}

// openVersions loads the stored versions of s (newest first) into the
// version browser and switches to it.
func (m *Model) openVersions(s *session.Session) {
	vs := backup.Versions(m.restoreDir(), s.ID)
	if len(vs) == 0 {
		m.status = "no backups stored for this session"
		return
	}
	liveID := backup.FileID(livePath(s))
	rows := make([]versionRow, 0, len(vs))
	for i := len(vs) - 1; i >= 0; i-- { // newest first
		v := vs[i]
		rows = append(rows, versionRow{
			Version: v,
			records: countRecords(v.Path),
			live:    liveID != "" && backup.VersionID(v.Path) == liveID,
		})
	}
	// Flag exactly one row as most complete: the largest, newest on ties.
	best := -1
	for i := range rows {
		if best == -1 || rows[i].Size > rows[best].Size {
			best = i
		}
	}
	if best >= 0 {
		rows[best].largest = true
	}
	m.versions = rows
	m.versionOf = s
	m.versionCursor = 0
	m.mode = modeVersions
}

func (m Model) selectedVersion() *versionRow {
	if m.versionCursor < 0 || m.versionCursor >= len(m.versions) {
		return nil
	}
	return &m.versions[m.versionCursor]
}

// isRunning reports whether the session is currently live. Restoring a
// version over a running session's transcript is unsafe — the live
// process would clobber the restore on its next save — so it's blocked.
func (m Model) isRunning(id string) bool {
	_, ok := m.live[id]
	return ok
}

// hasTranscript reports whether a session id still has a transcript file
// in Claude Code's projects dir (any project). A running session without
// one has been reaped and can only be rescued from its live process.
func hasTranscript(id string) bool {
	matches, _ := filepath.Glob(filepath.Join(session.DefaultRoot(), "*", id+".jsonl"))
	return len(matches) > 0
}

// runningRow is one entry in the running-sessions (rescue) browser.
type runningRow struct {
	live.Session
	reaped bool // transcript file is gone; rescue from the process
}

// refreshRunning rebuilds the running-sessions list from the live
// registry, reaped (rescuable) sessions first, keeping the cursor valid.
func (m *Model) refreshRunning() {
	rows := make([]runningRow, 0, len(m.live))
	for id, ls := range m.live {
		rows = append(rows, runningRow{Session: ls, reaped: m.reaped[id]})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].reaped != rows[j].reaped {
			return rows[i].reaped // reaped first
		}
		return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
	})
	m.running = rows
	if m.runningCursor >= len(rows) {
		m.runningCursor = len(rows) - 1
	}
	if m.runningCursor < 0 {
		m.runningCursor = 0
	}
}

// openRunning enters the running-sessions (rescue) browser.
func (m *Model) openRunning() {
	m.runningCursor = 0
	m.refreshRunning()
	m.mode = modeRunning
}

func (m Model) selectedRunning() *runningRow {
	if m.runningCursor < 0 || m.runningCursor >= len(m.running) {
		return nil
	}
	return &m.running[m.runningCursor]
}

// reapedCount is the number of running sessions with no saved transcript.
func (m Model) reapedCount() int {
	n := 0
	for range m.reaped {
		n++
	}
	return n
}

// exportMarkdownFile renders s to Markdown and writes it into the current
// working directory; label distinguishes a version export.
func (m Model) exportMarkdownFile(s session.Session, label string) (tea.Model, tea.Cmd) {
	md, err := session.Markdown(s)
	if err != nil {
		m.err = "export: " + err.Error()
		return m, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	path := filepath.Join(dir, exportName(s.Title, s.ID, label))
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		m.err = "export: " + err.Error()
		return m, nil
	}
	m.status = "exported to " + path
	return m, nil
}

// exportName builds a filesystem-friendly Markdown filename from a
// session's title and id (plus an optional label for versions).
func exportName(title, id, label string) string {
	slug := slugify(title)
	if slug == "" {
		slug = "session"
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	name := slug + "-" + short
	if label != "" {
		name += "-" + label
	}
	return name + ".md"
}

func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// countRecords returns the number of newline-delimited records in a
// session file, a cheap proxy for how much it contains.
func countRecords(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
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
	return m.switchToLive(ls)
}

// switchToLive focuses the tmux pane of a specific running session.
func (m Model) switchToLive(ls live.Session) (tea.Model, tea.Cmd) {
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
	case modeConfirmRestore:
		return m.viewConfirmRestore()
	case modeVersions:
		return m.viewVersions()
	case modeRunning:
		return m.viewRunning()
	default:
		return m.viewList()
	}
}

// viewRunning lists currently-running Claude sessions, reaped (rescuable)
// ones first, so long-lived processes whose transcript was cleaned up can
// be found and rescued before the process exits.
func (m Model) viewRunning() string {
	if len(m.running) == 0 {
		return m.viewList()
	}
	head := styleTitle.Render("Running Claude sessions") + "\n" +
		styleDim.Render(fmt.Sprintf("%d running · %d with no saved transcript (rescue below)", len(m.running), m.reapedCount()))

	stW, pidW, verW, whereW := 8, 7, 8, 34
	header := " " + styleHeader.Render(fmt.Sprintf(" %-*s  %*s  %-*s  %-*s  %-s ",
		stW, "STATUS", pidW, "PID", verW, "CC VER", whereW, "WHERE", "TRANSCRIPT"))

	rows := m.height - 6
	if rows < 1 {
		rows = 1
	}
	start := 0
	if m.runningCursor >= rows {
		start = m.runningCursor - rows + 1
	}
	end := start + rows
	if end > len(m.running) {
		end = len(m.running)
	}

	var b strings.Builder
	b.WriteString(head + "\n" + header + "\n")
	for i := start; i < end; i++ {
		rr := m.running[i]
		where := shortPath(rr.CWD)
		if rr.TmuxWindow != "" {
			where = fmt.Sprintf("tmux %s (%s)", rr.TmuxWindow, rr.TmuxLabel)
		} else {
			where += "  [not in tmux]"
		}
		transcript := "saved"
		if rr.reaped {
			transcript = "⚠ REAPED — rescue"
		}
		glyph, gstyle := liveMarker(rr.Status)
		body := fmt.Sprintf(" %s %-*s  %*d  %-*s  %-*s  %-s ",
			gstyle.Render(glyph), stW-2, rr.Status,
			pidW, rr.PID,
			verW, truncate(orDash(rr.Version), verW),
			whereW, truncate(where, whereW),
			transcript)
		if i == m.runningCursor {
			body = styleSelected.Render(body)
		}
		b.WriteString(" " + body + "\n")
	}
	for i := end - start; i < rows; i++ {
		b.WriteString("\n")
	}
	// Guidance / status line.
	guide := "reaped: no transcript on disk — its content lives only in the running process"
	if rr := m.selectedRunning(); rr != nil && rr.reaped {
		guide = "rescue: enter/w to jump in, send any message to rewrite the file, then esc · r · b to back it up"
	}
	if m.err != "" {
		b.WriteString(styleError.Render(" "+m.err) + "\n")
	} else {
		b.WriteString(styleDim.Render(" "+guide) + "\n")
	}
	b.WriteString(styleStatus.Render(" ↑/↓ select · enter/w switch to session · r rescan · esc back"))
	return b.String()
}

// viewVersions renders the stored versions of a session, newest first,
// with the most complete and the currently-live one flagged.
func (m Model) viewVersions() string {
	s := m.versionOf
	if s == nil || len(m.versions) == 0 {
		return m.viewList()
	}
	head := styleTitle.Render(truncate("Versions · "+s.Title, m.width-1)) + "\n" +
		styleDim.Render(truncate(fmt.Sprintf("%s · %s · %d stored", shortPath(s.CWD), s.ID, len(m.versions)), m.width-1))

	whenW, ageW, sizeW, recW := 19, 5, 9, 8
	header := " " + styleHeader.Render(fmt.Sprintf(" %-*s  %*s  %*s  %*s  %-s ",
		whenW, "WHEN", ageW, "AGE", sizeW, "SIZE", recW, "RECORDS", "FLAGS"))

	// A running session blocks restore (it would clobber the live file);
	// show that up front. Otherwise surface any transient status.
	notice := ""
	if m.isRunning(s.ID) {
		notice = styleError.Render(" ⚠ session is running — restoring a version is disabled (w to switch, then stop it)")
	} else if m.status != "" {
		notice = styleStatus.Render(" " + m.status)
	}

	rows := m.height - 5
	if notice != "" {
		rows--
	}
	if rows < 1 {
		rows = 1
	}
	start := 0
	if m.versionCursor >= rows {
		start = m.versionCursor - rows + 1
	}
	end := start + rows
	if end > len(m.versions) {
		end = len(m.versions)
	}

	var b strings.Builder
	b.WriteString(head + "\n" + header + "\n")
	for i := start; i < end; i++ {
		v := m.versions[i]
		flags := ""
		if v.largest {
			flags += "★ most complete  "
		}
		if v.live {
			flags += "● current"
		}
		body := fmt.Sprintf(" %-*s  %*s  %*s  %*d  %-s ",
			whenW, v.Time.Local().Format("2006-01-02 15:04:05"),
			ageW, relTime(v.Time),
			sizeW, humanSize(v.Size),
			recW, v.records,
			strings.TrimSpace(flags))
		if i == m.versionCursor {
			body = styleSelected.Render(body)
		}
		b.WriteString(" " + body + "\n")
	}
	for i := end - start; i < rows; i++ {
		b.WriteString("\n")
	}
	if notice != "" {
		b.WriteString(notice + "\n")
	}
	b.WriteString(styleStatus.Render(" ↑/↓ select · v view · e export · enter restore & resume · w tmux · esc back"))
	return b.String()
}

// viewConfirmRestore is the restore-then-resume dialog shown when the
// selected session isn't in Claude's live directory but has a backup.
func (m Model) viewConfirmRestore() string {
	s := m.pending
	if s == nil {
		return m.viewList()
	}
	label := styleDim.Render
	row := func(k, v string) string {
		return fmt.Sprintf("%s  %s", label(fmt.Sprintf("%8s", k)), v)
	}
	var lines []string
	if m.pendingVersion != "" {
		// Restoring a specific chosen version (from the version browser).
		var ver backup.Version
		for _, v := range backup.Versions(m.restoreDir(), s.ID) {
			if v.Path == m.pendingVersion {
				ver = v
				break
			}
		}
		lines = []string{
			styleTitle.Render("Restore this version & resume?"),
			"",
			styleDim.Render("The current live transcript is saved as a version first,"),
			styleDim.Render("then this version becomes the live session."),
			"",
			row("session", s.ID),
			row("project", shortPath(s.CWD)),
			row("version", fmt.Sprintf("%s · %s · %d records",
				ver.Time.Local().Format("2006-01-02 15:04:05"), humanSize(ver.Size), countRecords(ver.Path))),
			row("resume", shortPath(livePath(s))),
			"",
			styleStatus.Render("y  restore & resume    ·    any other key  cancel"),
		}
	} else {
		backupDesc := "—"
		if vs := backup.Versions(m.restoreDir(), s.ID); len(vs) > 0 {
			largest := vs[0]
			for _, v := range vs {
				if v.Size > largest.Size {
					largest = v
				}
			}
			backupDesc = fmt.Sprintf("%d version(s) · restoring most complete (%s)",
				len(vs), humanSize(largest.Size))
		}
		lines = []string{
			styleTitle.Render("Restore from backup?"),
			"",
			styleDim.Render("This session isn't in Claude Code's live directory —"),
			styleDim.Render("it will be restored, then resumed."),
			"",
			row("session", s.ID),
			row("project", shortPath(s.CWD)),
			row("backup", backupDesc),
			row("restore", shortPath(livePath(s))),
			row("", styleStatus.Render("destination is clear ✓")),
			"",
			styleStatus.Render("y  restore & resume    ·    any other key  cancel"),
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Render(strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
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
	if m.backupDir != "" {
		if vs := backup.Versions(m.backupDir, s.ID); len(vs) > 0 {
			newest, largest := vs[len(vs)-1], vs[0]
			for _, v := range vs {
				if v.Size > largest.Size {
					largest = v
				}
			}
			desc := fmt.Sprintf("%d version(s) · newest %s (%s)",
				len(vs), newest.Time.Local().Format("2006-01-02 15:04"), humanSize(newest.Size))
			// If an older version holds more data, the newest is likely a
			// truncation — flag the more-complete copy Restore would pick.
			if largest.Size > newest.Size {
				desc += fmt.Sprintf(" · largest %s ⚠", humanSize(largest.Size))
			}
			lines = append(lines, row("backups", desc))
		}
	}
	footer := "enter resume · any other key to close"
	if ls, ok := m.live[s.ID]; ok {
		loc := "not in tmux"
		if ls.TmuxWindow != "" {
			loc = fmt.Sprintf("tmux %s (%s)", ls.TmuxWindow, ls.TmuxLabel)
			footer = "enter resume · w switch to tmux · any other key to close"
		}
		glyph, style := liveMarker(ls.Status)
		lines = append(lines, row("running", fmt.Sprintf("%s %s · pid %d · %s",
			style.Render(glyph), ls.Status, ls.PID, loc)))
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

// liveMarker returns the glyph and style for a running-session status.
// Status is encoded redundantly by both shape and color so it stays
// legible under red-green color blindness: the palette avoids the
// green/red axis (blue / orange / magenta) and each status has a
// distinct shape, so color is never the only cue.
func liveMarker(status string) (string, lipgloss.Style) {
	switch status {
	case "busy": // working
		return "◆", lipgloss.NewStyle().Foreground(lipgloss.Color("208")) // orange diamond
	case "waiting": // needs user input
		return "▲", lipgloss.NewStyle().Foreground(lipgloss.Color("165")) // magenta triangle
	default: // idle but alive
		return "●", lipgloss.NewStyle().Foreground(lipgloss.Color("33")) // blue circle
	}
}

// liveCell renders the one-column running indicator for a row, or a
// blank when the session isn't running.
func (m Model) liveCell(id string) string {
	ls, ok := m.live[id]
	if !ok {
		return " "
	}
	glyph, style := liveMarker(ls.Status)
	return style.Render(glyph)
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

	// Two relative-time columns: MODIFIED (last activity, the sort key)
	// and CREATED (first message). Header words set the column widths.
	modW, crtW, projW := 8, 7, 16
	msgsW, modelW := 0, 0
	// CC VER column: the running session's Claude Code version. Shown only
	// when a visible session is actually running, so it costs no width
	// otherwise. It's appended after the fixed columns below.
	verW, verGap := 0, 0
	for _, s := range m.filtered {
		if ls, ok := m.live[s.ID]; ok && ls.Version != "" {
			verW, verGap = 7, 9 // width + " " gap + trailing " "
			break
		}
	}
	var titleW int
	if previewOn {
		projW = 12
		titleW = leftW - modW - crtW - projW - verGap - 8 // leading/trailing + 3 gaps
	} else {
		projW, msgsW, modelW = 20, 4, 8
		titleW = leftW - modW - crtW - projW - msgsW - modelW - verGap - 12
	}
	if titleW < 10 {
		titleW = 10
	}

	rows := m.height - 5 // header + context + status + key-bar lines
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
	verSuffix := func(v string) string {
		if verW == 0 {
			return ""
		}
		return fmt.Sprintf(" %-*s ", verW, truncate(v, verW))
	}
	leftLines := make([]string, 0, rows+1)
	if previewOn {
		leftLines = append(leftLines, " "+styleHeader.Render(fmt.Sprintf(" %*s  %*s  %-*s  %-*s ",
			modW, "MODIFIED", crtW, "CREATED", projW, "PROJECT", titleW, "TITLE")+verSuffix("CC VER")))
	} else {
		leftLines = append(leftLines, " "+styleHeader.Render(fmt.Sprintf(" %*s  %*s  %-*s  %-*s  %*s  %-*s ",
			modW, "MODIFIED", crtW, "CREATED", projW, "PROJECT", titleW, "TITLE", msgsW, "MSGS", modelW, "MODEL")+verSuffix("CC VER")))
	}
	for i := start; i < end; i++ {
		s := m.filtered[i]
		var body string
		if previewOn {
			body = fmt.Sprintf(" %*s  %*s  %-*s  %-*s ",
				modW, relTime(s.LastTime),
				crtW, relTime(s.FirstTime),
				projW, truncate(projName(s.CWD), projW),
				titleW, truncate(s.Title, titleW))
		} else {
			body = fmt.Sprintf(" %*s  %*s  %-*s  %-*s  %*d  %-*s ",
				modW, relTime(s.LastTime),
				crtW, relTime(s.FirstTime),
				projW, truncate(projName(s.CWD), projW),
				titleW, truncate(s.Title, titleW),
				msgsW, s.UserMsgs+s.AsstMsgs,
				modelW, truncate(shortModel(s.Model), modelW))
		}
		body += verSuffix(m.live[s.ID].Version)
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

	// Bottom: context line for selection, a transient status line, and a
	// persistent key bar (see keyBar) so available actions never get
	// hidden behind a search or filter readout.
	var ctx string
	if s := m.selected(); s != nil {
		line := fmt.Sprintf(" %s · last: %s", s.CWD, firstLineOf(s.LastPrompt))
		if m.contentQuery != "" {
			line = fmt.Sprintf(" %s · %d matches · last: %s", s.CWD, m.matches[s.Path], firstLineOf(s.LastPrompt))
		}
		ctx = styleDim.Render(truncate(line, m.width-1))
	}
	b.WriteString(ctx + "\n")

	status, help := m.statusAndHelp()
	b.WriteString(status + "\n")
	b.WriteString(help)
	return b.String()
}

// statusAndHelp builds the two bottom lines. Modal states (typing a
// filter/search, confirming a delete) own both lines and describe their
// own keys; every other state shows transient status on the first line
// and the full key bar on the second, so keys like i and w stay visible
// and usable even while a content search or filter is active.
func (m Model) statusAndHelp() (status, help string) {
	switch {
	case m.mode == modeConfirmDelete:
		id := ""
		if s := m.selected(); s != nil {
			id = s.ID
		}
		return styleError.Render(fmt.Sprintf(" delete session %s?", id)),
			styleStatus.Render(" y confirm · any other key cancel")
	case m.searching:
		return " " + m.search.View(),
			styleStatus.Render(" enter search · esc cancel")
	case m.filtering:
		return " " + m.filter.View(),
			styleStatus.Render(" type to filter · enter apply · esc clear")
	}

	switch {
	case m.err != "":
		status = styleError.Render(truncate(" "+m.err, m.width-1))
	case m.contentQuery != "":
		total := 0
		for _, s := range m.filtered {
			total += m.matches[s.Path]
		}
		status = styleStatus.Render(truncate(fmt.Sprintf(" content:%q · %d sessions · %d matches · esc clear", m.contentQuery, len(m.filtered), total), m.width-1))
	case m.filter.Value() != "":
		status = styleStatus.Render(truncate(fmt.Sprintf(" filter:%q · %d sessions · esc clear", m.filter.Value(), len(m.filtered)), m.width-1))
	default:
		status = styleStatus.Render(fmt.Sprintf(" %d sessions", len(m.filtered))) + m.liveLegend()
		if n := m.reapedCount(); n > 0 {
			status += styleError.Render(fmt.Sprintf(" · ⚠ %d running with no saved transcript — press R to rescue", n))
		}
		if m.status != "" {
			status += styleStatus.Render(" · " + truncate(m.status, m.width-1))
		}
	}
	help = styleStatus.Render(" " + truncate(keyBar, m.width-2))
	return status, help
}

// liveLegend renders the per-status breakdown of running sessions,
// e.g. " · ●11 ◆2 ▲2 live", or "" when nothing is running. The glyphs
// double as a legend for the row markers.
func (m Model) liveLegend() string {
	if len(m.live) == 0 {
		return ""
	}
	var idle, busy, waiting int
	for _, ls := range m.live {
		switch ls.Status {
		case "busy":
			busy++
		case "waiting":
			waiting++
		default:
			idle++
		}
	}
	var parts []string
	add := func(status string, cnt int) {
		if cnt == 0 {
			return
		}
		g, st := liveMarker(status)
		parts = append(parts, fmt.Sprintf("%s%d", st.Render(g), cnt))
	}
	add("idle", idle)
	add("busy", busy)
	add("waiting", waiting)
	return styleStatus.Render(" · ") + strings.Join(parts, " ") + styleStatus.Render(" live")
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
