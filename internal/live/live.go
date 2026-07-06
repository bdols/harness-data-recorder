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
	"strconv"
	"strings"
	"time"
)

// Session describes one running Claude Code session.
type Session struct {
	PID       int
	SessionID string
	CWD       string
	Status    string // "idle", "busy", "waiting", …
	Name      string
	UpdatedAt time.Time

	// tmux location, empty when the session isn't running inside tmux.
	TmuxPane   string // pane id, e.g. "%3" — the switch target
	TmuxWindow string // "session:window", e.g. "0:5" — for display
	TmuxLabel  string // tmux window name — for display
}

// registryEntry is the on-disk shape of ~/.claude/sessions/<pid>.json.
type registryEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Status    string `json:"status"`
	Name      string `json:"name"`
	UpdatedAt int64  `json:"updatedAt"` // epoch millis
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
			PID:       r.PID,
			SessionID: r.SessionID,
			CWD:       r.CWD,
			Status:    r.Status,
			Name:      r.Name,
			UpdatedAt: time.UnixMilli(r.UpdatedAt),
		}
		if pane, ok := paneForPID(r.PID, panes); ok {
			s.TmuxPane = pane.id
			s.TmuxWindow = pane.window
			s.TmuxLabel = pane.label
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
}

// tmuxPanes lists every tmux pane keyed by its pane pid (the shell), or
// an empty map if tmux isn't installed or no server is running.
func tmuxPanes() map[int]pane {
	panes := map[int]pane{}
	out, err := tmuxCmd("list-panes", "-a", "-F",
		"#{pane_id}\t#{pane_pid}\t#{session_name}:#{window_index}\t#{window_name}").Output()
	if err != nil {
		return panes
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		panes[pid] = pane{id: f[0], pid: pid, window: f[2], label: f[3]}
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
