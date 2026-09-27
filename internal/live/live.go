// Package live detects Claude Code sessions that are currently running
// and, when they live inside tmux, locates the pane so the browser can
// switch to it.
//
// Claude Code writes a registry file per running process at
// ~/.claude/sessions/<pid>.json containing the resolved session id,
// cwd, and status. That is the authoritative running-session signal —
// it resolves sessions regardless of how they were launched (fresh, by
// id, or by name). We cross-check the pid against /proc to drop stale
// registry files, then walk the process's parents until we reach a tmux
// pane pid to find its window.
package live

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session describes one running Claude Code session.
type Session struct {
	PID        int
	SessionID  string
	CWD        string
	Status     string // "idle", "busy", "waiting", …
	Name       string
	NameSource string // "user" = from --resume, "derived" = auto-generated
	Version    string // Claude Code version, e.g. "2.1.204"
	UpdatedAt  time.Time

	// tmux location, empty when the session isn't running inside tmux.
	TmuxPane   string // pane id, e.g. "%3" — the switch target
	TmuxWindow string // "session:window", e.g. "0:5" — for display
	TmuxLabel  string // tmux window name — for display
	TmuxNamed  bool   // window name was set by hand, not by tmux
	TmuxMarker string // name csb last assigned to this window (@csb_named)
}

// registryEntry is the on-disk shape of ~/.claude/sessions/<pid>.json.
type registryEntry struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	CWD        string `json:"cwd"`
	Status     string `json:"status"`
	Name       string `json:"name"`
	NameSource string `json:"nameSource"` // "user" = from --resume, "derived" = auto-generated
	Version    string `json:"version"`
	UpdatedAt  int64  `json:"updatedAt"` // epoch millis
}

// registryDir returns ~/.claude/sessions, overridable via
// CSB_SESSIONS_DIR (used by tests).
func registryDir() string {
	if d := os.Getenv("CSB_SESSIONS_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions")
}

// tmuxCmd builds a tmux invocation, targeting a private server socket
// when CSB_TMUX_SOCKET is set. That env var lets the browser (and the
// test suite) address a specific tmux server instead of the default.
func tmuxCmd(args ...string) *exec.Cmd {
	if sock := os.Getenv("CSB_TMUX_SOCKET"); sock != "" {
		args = append([]string{"-L", sock}, args...)
	}
	return exec.Command("tmux", args...)
}

// Scan returns the running sessions keyed by session id. Stale registry
// entries (whose process is gone or is no longer a claude process) are
// dropped. tmux enrichment is best-effort: a session with no tmux pane
// still appears, just without a switch target.
func Scan() map[string]Session {
	return scan(registryDir(), isClaudeProcess, tmuxPanes())
}

// scan is the testable core of Scan: alive decides whether a registry
// pid is still a running claude process, and panes maps pane pids to
// tmux panes. Both are injected so tests avoid touching /proc and the
// real tmux server.
func scan(dir string, alive func(int) bool, panes map[int]pane) map[string]Session {
	out := map[string]Session{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r registryEntry
		if err := json.Unmarshal(b, &r); err != nil || r.SessionID == "" || r.PID == 0 {
			continue
		}
		if !alive(r.PID) {
			continue // stale: process exited or pid reused
		}
		s := Session{
			PID:        r.PID,
			SessionID:  r.SessionID,
			CWD:        r.CWD,
			Status:     r.Status,
			Name:       r.Name,
			NameSource: r.NameSource,
			Version:    r.Version,
			UpdatedAt:  time.UnixMilli(r.UpdatedAt),
		}
		if pane, ok := paneForPID(r.PID, panes); ok {
			s.TmuxPane = pane.id
			s.TmuxWindow = pane.window
			s.TmuxLabel = pane.label
			s.TmuxNamed = pane.named
			s.TmuxMarker = pane.marker
		}
		// Keep the most recently updated entry if a pid was reused
		// across two registry files for the same session id.
		if prev, ok := out[s.SessionID]; !ok || s.UpdatedAt.After(prev.UpdatedAt) {
			out[s.SessionID] = s
		}
	}
	return out
}

type pane struct {
	id     string
	pid    int
	window string
	label  string
	named  bool   // window was renamed by hand (automatic-rename off)
	marker string // @csb_named: the name csb last gave this window
}

// tmuxPanes lists every tmux pane keyed by its pane pid (the shell), or
// an empty map if tmux isn't installed or no server is running.
func tmuxPanes() map[int]pane {
	panes := map[int]pane{}
	out, err := tmuxCmd("list-panes", "-a", "-F",
		"#{pane_id}\t#{pane_pid}\t#{session_name}:#{window_index}\t#{window_name}\t#{?automatic-rename,on,off}\t#{@csb_named}").Output()
	if err != nil {
		return panes
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		// automatic-rename off means someone named this window by hand —
		// though that "someone" may have been csb, which marker records.
		named := len(f) > 4 && f[4] == "off"
		marker := ""
		if len(f) > 5 {
			marker = f[5]
		}
		panes[pid] = pane{id: f[0], pid: pid, window: f[2], label: f[3], named: named, marker: marker}
	}
	return panes
}

// paneForPID walks pid's parent chain until it reaches a process that is
// a tmux pane's shell, and returns that pane.
func paneForPID(pid int, panes map[int]pane) (pane, bool) {
	for hops := 0; hops < 16 && pid > 1; hops++ {
		if p, ok := panes[pid]; ok {
			return p, true
		}
		ppid := ppidOf(pid)
		if ppid == 0 || ppid == pid {
			break
		}
		pid = ppid
	}
	return pane{}, false
}

// ppidOf reads the parent pid from /proc/<pid>/stat. The comm field is
// parenthesized and may itself contain spaces or ')', so ppid is taken
// as the second whitespace field after the final ')'.
func ppidOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// isClaudeProcess reports whether pid is a live process whose command is
// claude, guarding against both exited processes and pid reuse.
func isClaudeProcess(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte("claude"))
}

// Rename describes one planned tmux window rename.
type Rename struct {
	Pane   string // pane id to target the window through
	Window string // "session:index", for display
	Old    string // current window name
	New    string // project-derived name
}

// PlanRenames works out what each tmux window holding a Claude session
// should be called, based on the project each session is running in.
//
// Windows are the unit, not sessions: several Claude processes commonly
// share one window (repeated resumes in the same pane), and tmux names
// windows, not panes. When the sessions in a window disagree about the
// project, the most recently active ones win and the name is joined with
// "+", so a shared window still says what is in it.
//
// Two things keep the result useful rather than merely tidy:
//
//   - Windows you named yourself are left alone unless force is set. tmux
//     records that as automatic-rename off, which is a reliable signal of
//     intent — clobbering a hand-picked "jetson" with a generic project
//     name is a downgrade, not an improvement. Windows csb named itself
//     are exempt from that guard (see csbOwns), or its own past renames
//     would lock it out of ever improving them.
//   - Names that would collide are disambiguated using labels (session id
//     to a short title), because several sessions in one project produce
//     several identical window names, which is no more informative than
//     the "claude" they replace.
//
// Renames that would be no-ops are omitted, so an empty result means
// every window is already named correctly.
func PlanRenames(sessions map[string]Session, labels map[string]string, force bool) []Rename {
	type group struct {
		pane, window, old string
		names             []string
		ids               []string
		seen              map[string]bool
	}
	groups := map[string]*group{}
	// Newest first, so the most recently active session names a shared window.
	ordered := make([]Session, 0, len(sessions))
	for _, s := range sessions {
		if s.TmuxWindow == "" || s.TmuxPane == "" {
			continue
		}
		if s.TmuxNamed && !force && !csbOwns(s) {
			continue // deliberately named by hand, and not by us
		}
		ordered = append(ordered, s)
	}
	// User-named sessions (--resume) take priority; within the same source,
	// the most recently active session names the window.
	sort.Slice(ordered, func(i, j int) bool {
		ui := ordered[i].NameSource == "user"
		uj := ordered[j].NameSource == "user"
		if ui != uj {
			return ui
		}
		return ordered[i].UpdatedAt.After(ordered[j].UpdatedAt)
	})

	var order []string
	for _, s := range ordered {
		g, ok := groups[s.TmuxWindow]
		if !ok {
			g = &group{pane: s.TmuxPane, window: s.TmuxWindow, old: s.TmuxLabel, seen: map[string]bool{}}
			groups[s.TmuxWindow] = g
			order = append(order, s.TmuxWindow)
		}
		g.ids = append(g.ids, s.SessionID)
		n := s.Name
		if n == "" {
			n = ProjectName(s.CWD)
		}
		if n != "" && !g.seen[n] {
			g.seen[n] = true
			g.names = append(g.names, n)
		}
	}

	// First pass: the project-only name each window would like.
	base := map[string]string{}
	counts := map[string]int{}
	for _, w := range order {
		g := groups[w]
		if len(g.names) == 0 {
			continue
		}
		name := g.names[0]
		if len(g.names) > 1 {
			name = strings.Join(g.names[:2], "+")
			if len(g.names) > 2 {
				name += "+…"
			}
		}
		base[w] = name
		counts[name]++
	}

	// Second pass: qualify the ones that would be ambiguous.
	var out []Rename
	for _, w := range order {
		name, ok := base[w]
		if !ok {
			continue
		}
		g := groups[w]
		if counts[name] > 1 {
			if q := qualifier(g.ids, labels); q != "" {
				name += "-" + q
			}
		}
		if name == g.old {
			continue
		}
		out = append(out, Rename{Pane: g.pane, Window: g.window, Old: g.old, New: name})
	}
	return out
}

// csbOwns reports whether this window's current name is one csb gave it
// and nobody has changed since.
//
// rename-window turns automatic-rename off, so a window csb renamed looks
// exactly like one the user named by hand. The @csb_named marker breaks
// that tie: it holds the name csb last assigned, so if the window still
// carries that name, csb may update it. If the name has since diverged
// the user has taken the window over, and the hand-named guard applies
// again.
func csbOwns(s Session) bool {
	return s.TmuxMarker != "" && s.TmuxMarker == s.TmuxLabel
}

// qualifier picks a short distinguishing suffix for a window from the
// titles of the sessions in it.
func qualifier(ids []string, labels map[string]string) string {
	for _, id := range ids {
		if s := slug(labels[id]); s != "" {
			return s
		}
	}
	return ""
}

// slug reduces a session title to a couple of lowercase words usable in a
// tmux window name. The second word is dropped rather than cut short, so
// names break on word boundaries instead of trailing off mid-word.
func slug(title string) string {
	const max = 16
	var words []string
	for _, w := range strings.Fields(strings.ToLower(title)) {
		var clean strings.Builder
		for _, r := range w {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				clean.WriteRune(r)
			}
		}
		if clean.Len() > 0 {
			words = append(words, clean.String())
		}
		if len(words) == 2 {
			break
		}
	}
	if len(words) == 0 {
		return ""
	}
	s := words[0]
	if len(s) > max {
		return s[:max]
	}
	if len(words) > 1 && len(s)+1+len(words[1]) <= max {
		s += "-" + words[1]
	}
	return s
}

// ProjectName reduces a working directory to a short window label.
func ProjectName(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	if cwd == "" {
		return "root"
	}
	return filepath.Base(cwd)
}

// RenameWindow renames the tmux window containing pane.
//
// tmux turns automatic-rename off for a window that is renamed by hand,
// so the name sticks instead of reverting to the running command the next
// time the pane's process changes. That is the point — but it also means
// this is a change the user may want undone, which is what
// RestoreAutoRename is for.
func RenameWindow(pane, name string) error {
	if pane == "" {
		return fmt.Errorf("no tmux pane for this session")
	}
	if err := tmuxCmd("rename-window", "-t", pane, name).Run(); err != nil {
		return err
	}
	// Record what we named it. rename-window turns automatic-rename off,
	// which is indistinguishable from the user naming the window by hand;
	// without this marker csb would mistake its own past work for the
	// user's and refuse to ever update the window again.
	_ = tmuxCmd("set-option", "-w", "-t", pane, "@csb_named", name).Run()
	return nil
}

// RestoreAutoRename hands a window back to tmux's automatic naming,
// undoing a RenameWindow.
func RestoreAutoRename(pane string) error {
	if pane == "" {
		return fmt.Errorf("no tmux pane for this session")
	}
	_ = tmuxCmd("set-option", "-w", "-t", pane, "-u", "@csb_named").Run()
	return tmuxCmd("set-window-option", "-t", pane, "automatic-rename", "on").Run()
}

// InTmux reports whether the browser itself is running inside tmux, a
// precondition for switching the attached client to another pane.
func InTmux() bool {
	return os.Getenv("TMUX") != ""
}

// SwitchTmux points the attached tmux client at pane, bringing its
// window and pane to the foreground. The browser keeps running in its
// own pane.
func SwitchTmux(pane string) error {
	if pane == "" {
		return fmt.Errorf("no tmux pane for this session")
	}
	// switch-client moves the attached client to the pane's session; it
	// needs a client, so it's best-effort. select-window/-pane set the
	// active window and pane within that session and are the
	// authoritative focus, so select-window's result is what we return.
	_ = tmuxCmd("switch-client", "-t", pane).Run()
	err := tmuxCmd("select-window", "-t", pane).Run()
	_ = tmuxCmd("select-pane", "-t", pane).Run()
	return err
}
