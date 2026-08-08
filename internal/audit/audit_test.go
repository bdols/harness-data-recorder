package audit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bdols/claude-session-browser/internal/backup"
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
