package audit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bdols/harness-data-recorder/internal/backup"
	"github.com/bdols/harness-data-recorder/internal/session"
)

// writeSess writes a minimal session with one Bash tool call.
func writeSess(t *testing.T, root, proj, id, cwd, cmd, ts string) {
	t.Helper()
	dir := filepath.Join(root, proj)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"ai-title","aiTitle":"T","sessionId":"` + id + `","cwd":"` + cwd + `"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"` + cmd + `"}}]},"timestamp":"` + ts + `","cwd":"` + cwd + `"}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectOrdersAndExtracts(t *testing.T) {
	root, backupDir := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "-home-x-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"ai-title","aiTitle":"T","sessionId":"sid","cwd":"/home/x/proj"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","name":"Bash","input":{"command":"ls -la /tmp"}}]},"timestamp":"2026-05-01T10:00:00Z","cwd":"/home/x/proj"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/home/x/proj/main.go"}}]},"timestamp":"2026-05-01T10:05:00Z","cwd":"/home/x/proj"}
`
	if err := os.WriteFile(filepath.Join(dir, "sid.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Collect(root, backupDir)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	// Newest first: the Edit (10:05) precedes the Bash (10:00).
	if got[0].Tool != "Edit" || got[1].Tool != "Bash" {
		t.Errorf("order wrong: %+v", got)
	}
	if got[1].Detail != "ls -la /tmp" {
		t.Errorf("bash detail = %q, want %q", got[1].Detail, "ls -la /tmp")
	}
	if got[0].Project != "proj" {
		t.Errorf("project = %q, want proj", got[0].Project)
	}
}

// A reaped session (gone from live but in backups) must still contribute
// its tool calls to the audit.
func TestCollectIncludesReapedFromBackup(t *testing.T) {
	root, backupDir := t.TempDir(), t.TempDir()
	writeSess(t, root, "-p", "live1", "/home/x/live", "echo live-cmd", "2026-05-01T10:00:00Z")
	writeSess(t, root, "-p", "reaped1", "/home/x/reap", "echo reaped-cmd", "2026-05-01T09:00:00Z")
	if _, _, err := backup.Sync(root, backupDir); err != nil {
		t.Fatal(err)
	}
	// The reaped session's live file disappears.
	if err := os.Remove(filepath.Join(root, "-p", "reaped1.jsonl")); err != nil {
		t.Fatal(err)
	}

	var live, reaped bool
	for _, e := range Collect(root, backupDir) {
		switch e.Detail {
		case "echo live-cmd":
			live = true
		case "echo reaped-cmd":
			reaped = true
		}
	}
	if !live || !reaped {
		t.Errorf("live=%v reaped=%v, want both (reaped must come from backups)", live, reaped)
	}
}

func bashEntry(detail string, sb session.Sandbox) Entry {
	return Entry{Tool: "Bash", Detail: detail, Project: "hdr", Model: "claude-opus-5", Sandbox: sb}
}

// The regression this guards: a command whose text mentions the selector
// used to match it, so running the audit polluted the audit.
func TestMatchesSandboxSelectorIgnoresCommandText(t *testing.T) {
	decoy := bashEntry("hdr --commands sandbox:bypass | wc -l", session.SandboxDefault)
	if decoy.Matches([]string{"sandbox:bypass"}) {
		t.Error("a command merely mentioning sandbox:bypass must not match the selector")
	}
	real := bashEntry("curl https://example.com", session.SandboxBypassed)
	if !real.Matches([]string{"sandbox:bypass"}) {
		t.Error("an actually-bypassed call must match")
	}
	// The text is still findable as free text.
	if !decoy.Matches([]string{"--commands"}) {
		t.Error("free-text search over the command must still work")
	}
}

func TestMatchesSandboxValues(t *testing.T) {
	byp := bashEntry("a", session.SandboxBypassed)
	def := bashEntry("b", session.SandboxDefault)
	read := Entry{Tool: "Read", Detail: "/x", Sandbox: session.SandboxNA}

	cases := []struct {
		term string
		e    Entry
		want bool
	}{
		{"sandbox:bypass", byp, true},
		{"sandbox:bypassed", byp, true},
		{"sandbox:default", byp, false},
		{"sandbox:default", def, true},
		{"sandbox:bypass", def, false},
		{"sandbox:", byp, true},   // any Bash call
		{"sandbox:", read, false}, // not a Bash call
		{"sandbox:default", read, false},
		{"sandbox:nonsense", byp, false}, // typo matches nothing, not everything
		{"SANDBOX:BYPASS", byp, true},    // case-insensitive
	}
	for _, c := range cases {
		if got := c.e.Matches([]string{c.term}); got != c.want {
			t.Errorf("%q against %v = %v, want %v", c.term, c.e.SandboxLabel(), got, c.want)
		}
	}
}

func TestMatchesSubagentSelector(t *testing.T) {
	sub := Entry{Tool: "Bash", Detail: "ls", Model: "claude-sonnet-5", Subagent: true}
	main := Entry{Tool: "Bash", Detail: "talking about sub: agents", Model: "claude-opus-5"}

	if !sub.Matches([]string{"sub:"}) {
		t.Error("subagent call should match sub:")
	}
	if main.Matches([]string{"sub:"}) {
		t.Error("a main-loop call whose text says 'sub:' must not match the selector")
	}
	if !sub.Matches([]string{"sub:sonnet"}) {
		t.Error("sub:sonnet should match a sonnet subagent")
	}
	if sub.Matches([]string{"sub:opus"}) {
		t.Error("sub:opus must not match a sonnet subagent")
	}
}

// model: scopes a model term to the model field, so it can't be faked by
// a command that happens to name a model.
func TestMatchesModelSelector(t *testing.T) {
	e := Entry{Tool: "Bash", Detail: "echo claude-sonnet-5", Model: "claude-opus-5"}
	if !e.Matches([]string{"model:opus"}) {
		t.Error("model:opus should match the issuing model")
	}
	if e.Matches([]string{"model:sonnet"}) {
		t.Error("model:sonnet must not match a model named only in the command text")
	}
	// Bare terms stay free-text, so the old behaviour is intact.
	if !e.Matches([]string{"sonnet"}) {
		t.Error("a bare term should still search the command text")
	}
}

// Terms are ANDed, and an empty term list matches everything.
func TestMatchesCombinesTerms(t *testing.T) {
	e := bashEntry("curl https://example.com", session.SandboxBypassed)
	if !e.Matches(nil) {
		t.Error("no terms should match everything")
	}
	if !e.Matches([]string{"sandbox:bypass", "curl"}) {
		t.Error("selector AND free text should match")
	}
	if e.Matches([]string{"sandbox:bypass", "wget"}) {
		t.Error("a failing free-text term must reject the entry")
	}
}
