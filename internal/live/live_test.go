package live

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestPlanRenamesUsesProjectNames(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/var/mnt/videos/Clips", TmuxPane: "%1", TmuxWindow: "0:1", TmuxLabel: "claude"},
		"b": {SessionID: "b", CWD: "/home/x/code/api", TmuxPane: "%2", TmuxWindow: "0:2", TmuxLabel: "zsh"},
	}
	plan := PlanRenames(sessions, nil, false)
	if len(plan) != 2 {
		t.Fatalf("got %d renames, want 2: %+v", len(plan), plan)
	}
	got := map[string]string{}
	for _, r := range plan {
		got[r.Window] = r.New
	}
	if got["0:1"] != "Clips" || got["0:2"] != "api" {
		t.Errorf("names = %v, want 0:1=Clips 0:2=api", got)
	}
}

// Several Claude processes commonly share one window; tmux names windows,
// not panes, so the window gets one combined name.
func TestPlanRenamesCombinesSharedWindow(t *testing.T) {
	newer := time.Now()
	older := newer.Add(-time.Hour)
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/p/alpha", TmuxPane: "%1", TmuxWindow: "0:4", TmuxLabel: "claude", UpdatedAt: newer},
		"b": {SessionID: "b", CWD: "/p/beta", TmuxPane: "%1", TmuxWindow: "0:4", TmuxLabel: "claude", UpdatedAt: older},
	}
	plan := PlanRenames(sessions, nil, false)
	if len(plan) != 1 {
		t.Fatalf("got %d renames, want 1: %+v", len(plan), plan)
	}
	// Most recently active session names the window first.
	if plan[0].New != "alpha+beta" {
		t.Errorf("name = %q, want alpha+beta", plan[0].New)
	}
}

// Two sessions in the same project must not produce "proj+proj".
func TestPlanRenamesDedupesSameProject(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/p/same", TmuxPane: "%1", TmuxWindow: "0:4", TmuxLabel: "claude"},
		"b": {SessionID: "b", CWD: "/p/same", TmuxPane: "%1", TmuxWindow: "0:4", TmuxLabel: "claude"},
	}
	plan := PlanRenames(sessions, nil, false)
	if len(plan) != 1 || plan[0].New != "same" {
		t.Fatalf("plan = %+v, want one rename to \"same\"", plan)
	}
}

// A window already named after its project is left alone, so repeated
// runs are no-ops rather than churn.
func TestPlanRenamesSkipsCorrectNames(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/p/done", TmuxPane: "%1", TmuxWindow: "0:1", TmuxLabel: "done"},
	}
	if plan := PlanRenames(sessions, nil, false); len(plan) != 0 {
		t.Errorf("plan = %+v, want empty", plan)
	}
}

// Sessions outside tmux have no window to rename.
func TestPlanRenamesIgnoresNonTmux(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/p/x"},
	}
	if plan := PlanRenames(sessions, nil, false); len(plan) != 0 {
		t.Errorf("plan = %+v, want empty", plan)
	}
}

func TestProjectName(t *testing.T) {
	cases := map[string]string{
		"/var/mnt/videos/Clips":  "Clips",
		"/var/mnt/videos/Clips/": "Clips",
		"/":                      "root",
		"":                       "root",
	}
	for in, want := range cases {
		if got := ProjectName(in); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A window the user named by hand (automatic-rename off) is left alone:
// clobbering a deliberate name with a generic project name is a
// downgrade. force overrides that.
func TestPlanRenamesRespectsHandNamedWindows(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/p/proj", TmuxPane: "%1", TmuxWindow: "0:1",
			TmuxLabel: "jetson", TmuxNamed: true},
	}
	if plan := PlanRenames(sessions, nil, false); len(plan) != 0 {
		t.Errorf("plan = %+v, want empty (window was named by hand)", plan)
	}
	plan := PlanRenames(sessions, nil, true)
	if len(plan) != 1 || plan[0].New != "proj" {
		t.Errorf("force plan = %+v, want one rename to \"proj\"", plan)
	}
}

// Several windows in one project would otherwise all get the same name,
// which is no more useful than the "claude" they replace.
func TestPlanRenamesDisambiguatesCollisions(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/home/x/obsidian", TmuxPane: "%1", TmuxWindow: "0:1", TmuxLabel: "claude"},
		"b": {SessionID: "b", CWD: "/home/x/obsidian", TmuxPane: "%2", TmuxWindow: "0:2", TmuxLabel: "claude"},
	}
	labels := map[string]string{"a": "Dub pack creation", "b": "Sonarr newsgroup options"}
	plan := PlanRenames(sessions, labels, false)
	if len(plan) != 2 {
		t.Fatalf("got %d renames, want 2: %+v", len(plan), plan)
	}
	names := map[string]bool{}
	for _, r := range plan {
		if r.New == "obsidian" {
			t.Errorf("ambiguous name survived: %+v", plan)
		}
		names[r.New] = true
	}
	if len(names) != 2 {
		t.Errorf("names collided after disambiguation: %+v", plan)
	}
}

// With no titles to draw on there is nothing to disambiguate with; the
// plain project name is still better than "claude".
func TestPlanRenamesCollisionWithoutTitles(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/p/dup", TmuxPane: "%1", TmuxWindow: "0:1", TmuxLabel: "claude"},
		"b": {SessionID: "b", CWD: "/p/dup", TmuxPane: "%2", TmuxWindow: "0:2", TmuxLabel: "claude"},
	}
	plan := PlanRenames(sessions, nil, false)
	if len(plan) != 2 {
		t.Fatalf("got %d renames, want 2: %+v", len(plan), plan)
	}
	for _, r := range plan {
		if r.New != "dup" {
			t.Errorf("name = %q, want dup", r.New)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Dub pack creation":                    "dub-pack",
		"Plan & implement the session browser": "plan-implement",
		"":                                     "",
		"!!!":                                  "",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// A session named with --resume should name its window, since the user's
// own label beats a directory basename.
func TestPlanRenamesPrefersSessionName(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", Name: "csb", NameSource: "user", CWD: "/home/x/code/claude-session-browser",
			TmuxPane: "%1", TmuxWindow: "0:1", TmuxLabel: "claude"},
	}
	plan := PlanRenames(sessions, nil, false)
	if len(plan) != 1 || plan[0].New != "csb" {
		t.Fatalf("plan = %+v, want one rename to \"csb\"", plan)
	}
}

// Several sessions in one project used to collide on the same basename;
// their distinct session names now separate them without any qualifier.
func TestPlanRenamesSessionNamesAvoidCollisions(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", Name: "ebooks", NameSource: "user", CWD: "/home/x/obsidian",
			TmuxPane: "%1", TmuxWindow: "0:1", TmuxLabel: "claude"},
		"b": {SessionID: "b", Name: "pi5", NameSource: "user", CWD: "/home/x/obsidian",
			TmuxPane: "%2", TmuxWindow: "0:2", TmuxLabel: "claude"},
	}
	got := map[string]string{}
	for _, r := range PlanRenames(sessions, nil, false) {
		got[r.Window] = r.New
	}
	if got["0:1"] != "ebooks" || got["0:2"] != "pi5" {
		t.Errorf("names = %v, want 0:1=ebooks 0:2=pi5 (no qualifier suffix)", got)
	}
}

// In a shared window a user-chosen name wins over an auto-derived one
// even when the derived session is more recently active.
func TestPlanRenamesUserNameBeatsDerived(t *testing.T) {
	newer := time.Now()
	sessions := map[string]Session{
		"a": {SessionID: "a", Name: "derived-one", NameSource: "derived", CWD: "/p/x",
			TmuxPane: "%1", TmuxWindow: "0:4", TmuxLabel: "claude", UpdatedAt: newer},
		"b": {SessionID: "b", Name: "mine", NameSource: "user", CWD: "/p/x",
			TmuxPane: "%1", TmuxWindow: "0:4", TmuxLabel: "claude", UpdatedAt: newer.Add(-time.Hour)},
	}
	plan := PlanRenames(sessions, nil, false)
	if len(plan) != 1 {
		t.Fatalf("got %d renames, want 1: %+v", len(plan), plan)
	}
	if plan[0].New != "mine+derived-one" {
		t.Errorf("name = %q, want mine+derived-one (user name first)", plan[0].New)
	}
}

// An unnamed session still falls back to the project basename.
func TestPlanRenamesFallsBackToProjectName(t *testing.T) {
	sessions := map[string]Session{
		"a": {SessionID: "a", CWD: "/home/x/code/netlogo", TmuxPane: "%1", TmuxWindow: "0:1", TmuxLabel: "claude"},
	}
	plan := PlanRenames(sessions, nil, false)
	if len(plan) != 1 || plan[0].New != "netlogo" {
		t.Fatalf("plan = %+v, want one rename to \"netlogo\"", plan)
	}
}

// scan must carry nameSource through from the registry, or the user/
// derived priority silently degrades to "everything is derived".
func TestScanReadsNameSource(t *testing.T) {
	dir := t.TempDir()
	writeRegistry(t, dir, "100.json",
		`{"pid":100,"sessionId":"s","cwd":"/p","status":"idle","name":"mine","nameSource":"user","updatedAt":1}`)
	got := scan(dir, func(int) bool { return true }, map[int]pane{})
	if s := got["s"]; s.Name != "mine" || s.NameSource != "user" {
		t.Errorf("name=%q nameSource=%q, want mine/user", s.Name, s.NameSource)
	}
}
