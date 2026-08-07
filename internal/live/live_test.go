package live

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeRegistry writes a ~/.claude/sessions-style registry file.
func writeRegistry(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanKeepsLiveDropsStale(t *testing.T) {
	dir := t.TempDir()
	writeRegistry(t, dir, "100.json",
		`{"pid":100,"sessionId":"live-one","cwd":"/p","status":"busy","name":"n","version":"2.1.204","updatedAt":1000}`)
	writeRegistry(t, dir, "200.json",
		`{"pid":200,"sessionId":"stale-one","cwd":"/q","status":"idle","updatedAt":2000}`)
	writeRegistry(t, dir, "notjson.txt", `ignored`)

	// Only pid 100 is "alive".
	alive := func(pid int) bool { return pid == 100 }
	got := scan(dir, alive, map[int]pane{})

	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1: %v", len(got), got)
	}
	s, ok := got["live-one"]
	if !ok {
		t.Fatal("live session missing")
	}
	if s.PID != 100 || s.Status != "busy" || s.CWD != "/p" || s.Version != "2.1.204" {
		t.Errorf("unexpected fields: %+v", s)
	}
	if _, ok := got["stale-one"]; ok {
		t.Error("stale session should have been dropped")
	}
}

func TestScanEnrichesTmux(t *testing.T) {
	dir := t.TempDir()
	writeRegistry(t, dir, "100.json",
		`{"pid":100,"sessionId":"s","cwd":"/p","status":"idle","updatedAt":1}`)

	panes := map[int]pane{
		100: {id: "%7", pid: 100, window: "0:3", label: "claude"},
	}
	got := scan(dir, func(int) bool { return true }, panes)

	s := got["s"]
	if s.TmuxPane != "%7" || s.TmuxWindow != "0:3" || s.TmuxLabel != "claude" {
		t.Errorf("tmux enrichment wrong: pane=%q window=%q label=%q",
			s.TmuxPane, s.TmuxWindow, s.TmuxLabel)
	}
}

func TestScanMissingDir(t *testing.T) {
	got := scan(filepath.Join(t.TempDir(), "nope"), func(int) bool { return true }, nil)
	if len(got) != 0 {
		t.Errorf("missing dir should yield no sessions, got %v", got)
	}
}

// paneForPID must find a pane by walking real parent pids: the test
// process's own pid isn't a pane, but its parent is registered here, so
// the walk should climb one hop via /proc.
func TestPaneForPIDWalksParents(t *testing.T) {
	self := os.Getpid()
	parent := os.Getppid()
	if parent <= 1 {
		t.Skip("no usable parent pid in this environment")
	}
	panes := map[int]pane{parent: {id: "%1", pid: parent, window: "0:0"}}
	p, ok := paneForPID(self, panes)
	if !ok {
		t.Fatalf("paneForPID(%d) did not reach parent %d", self, parent)
	}
	if p.id != "%1" {
		t.Errorf("found pane %q, want %%1", p.id)
	}
}

func TestPaneForPIDNoMatch(t *testing.T) {
	if _, ok := paneForPID(os.Getpid(), map[int]pane{999999999: {}}); ok {
		t.Error("expected no pane match")
	}
}

func TestPpidOfSelf(t *testing.T) {
	if got := ppidOf(os.Getpid()); got != os.Getppid() {
		t.Errorf("ppidOf(self) = %d, want %d", got, os.Getppid())
	}
}

func TestIsClaudeProcess(t *testing.T) {
	// The test binary itself is alive but is not "claude".
	if isClaudeProcess(os.Getpid()) {
		t.Error("test process should not be detected as claude")
	}
	if isClaudeProcess(999999999) {
		t.Error("nonexistent pid should not be alive")
	}
}

// TestSwitchTmux exercises the real tmux focus commands against a
// private, throwaway tmux server (via CSB_TMUX_SOCKET) so it can never
// disturb the developer's own tmux session.
func TestSwitchTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := "csb-test-" + strings.ReplaceAll(t.Name(), "/", "-")
	tm := func(args ...string) *exec.Cmd {
		return exec.Command("tmux", append([]string{"-L", sock}, args...)...)
	}
	// -f /dev/null: ignore the user's tmux.conf for a predictable server.
	if err := tm("-f", "/dev/null", "new-session", "-d", "-s", "t", "-x", "80", "-y", "24").Run(); err != nil {
		t.Skipf("cannot start private tmux server: %v", err)
	}
	t.Cleanup(func() { _ = tm("kill-server").Run() })
	if err := tm("new-window", "-t", "t:1").Run(); err != nil {
		t.Fatal(err)
	}
	if err := tm("new-window", "-t", "t:2").Run(); err != nil {
		t.Fatal(err)
	}

	// Window 2 is active; grab window 0's pane and switch focus to it.
	pane0, err := tm("list-panes", "-t", "t:0", "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatal(err)
	}
	target := strings.TrimSpace(string(pane0))

	t.Setenv("CSB_TMUX_SOCKET", sock)
	if err := SwitchTmux(target); err != nil {
		t.Fatalf("SwitchTmux(%q) = %v", target, err)
	}

	active, err := tm("display-message", "-p", "-t", "t", "#{active_window_index}").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(active)); got != "0" {
		t.Errorf("active window = %q after switch, want 0", got)
	}
}
