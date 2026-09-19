package rewrite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMungeDir(t *testing.T) {
	cases := map[string]string{
		"/var/mnt/videos/GarthMarenghi/Clips":      "-var-mnt-videos-GarthMarenghi-Clips",
		"/var/home/bd/code/claude-session-browser": "-var-home-bd-code-claude-session-browser",
		"/tmp":                   "-tmp",
		"/var/home/bd/.config/x": "-var-home-bd--config-x",
	}
	for in, want := range cases {
		if got := MungeDir(in); got != want {
			t.Errorf("MungeDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeTranscript creates a session file whose records carry the given cwds.
func writeTranscript(t *testing.T, dir, id string, cwds ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`{"type":"ai-title","aiTitle":"T","sessionId":"` + id + `"}` + "\n")
	for i, c := range cwds {
		b.WriteString(`{"type":"user","message":{"role":"user","content":"hi ` +
			string(rune('a'+i)) + `"},"cwd":"` + c + `","timestamp":"2026-05-01T10:00:00Z"}` + "\n")
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRehomeRewritesCWDAndLandsInMungedDir(t *testing.T) {
	root := t.TempDir()
	newDir := t.TempDir()
	src := writeTranscript(t, filepath.Join(root, "-old-path"), "sid",
		"/old/path", "/old/path", "/tmp")

	res, err := Rehome(root, src, "sid", newDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, MungeDir(newDir), "sid.jsonl"); res.Dest != want {
		t.Errorf("dest = %q, want %q", res.Dest, want)
	}
	if res.Changed != 3 {
		t.Errorf("changed = %d, want 3", res.Changed)
	}
	if len(res.OldCWDs) != 2 {
		t.Errorf("old cwds = %v, want 2 distinct", res.OldCWDs)
	}
	// The original must survive: re-homing copies, never moves.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source was removed: %v", err)
	}

	data, err := os.ReadFile(res.Dest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/old/path") || strings.Contains(string(data), `"cwd":"/tmp"`) {
		t.Errorf("old cwd survived the rewrite:\n%s", data)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("record %d is not valid JSON after rewrite: %v", i, err)
		}
	}
	// Content other than cwd must be preserved byte-for-byte.
	if !strings.Contains(string(data), `"aiTitle":"T"`) || !strings.Contains(string(data), `"hi a"`) {
		t.Errorf("rewrite lost content:\n%s", data)
	}
}

func TestRehomeCopiesProjectMemory(t *testing.T) {
	root, newDir := t.TempDir(), t.TempDir()
	old := filepath.Join(root, "-old")
	src := writeTranscript(t, old, "sid", "/old")
	if err := os.MkdirAll(filepath.Join(old, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "memory", "note.md"), []byte("remember"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Rehome(root, src, "sid", newDir, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(res.Memory, "note.md"))
	if err != nil || string(got) != "remember" {
		t.Errorf("memory not carried over: %v %q", err, got)
	}
}

func TestRehomeRefusesToClobber(t *testing.T) {
	root, newDir := t.TempDir(), t.TempDir()
	src := writeTranscript(t, filepath.Join(root, "-old"), "sid", "/old")
	if _, err := Rehome(root, src, "sid", newDir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Rehome(root, src, "sid", newDir, false); err == nil {
		t.Error("second rehome should refuse to overwrite without force")
	}
	if _, err := Rehome(root, src, "sid", newDir, true); err != nil {
		t.Errorf("force should overwrite: %v", err)
	}
}

func TestRehomeRejectsRelativeAndMissingDirs(t *testing.T) {
	root := t.TempDir()
	src := writeTranscript(t, filepath.Join(root, "-old"), "sid", "/old")
	if _, err := Rehome(root, src, "sid", "relative/path", false); err == nil {
		t.Error("relative destination should be rejected")
	}
	if _, err := Rehome(root, src, "sid", filepath.Join(root, "nope"), false); err == nil {
		t.Error("nonexistent destination should be rejected")
	}
}

func TestReplaceRedactsAndCounts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"token sk-secret-123 here"},"cwd":"/x"}
{"type":"user","message":{"role":"user","content":"again sk-secret-123"},"cwd":"/x"}
{"type":"user","message":{"role":"user","content":"clean"},"cwd":"/x"}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Replace(p, "sk-secret-123", "[REDACTED]")
	if err != nil {
		t.Fatal(err)
	}
	if res.Replacements != 2 || res.Records != 2 {
		t.Errorf("got %d replacements in %d records, want 2 in 2", res.Replacements, res.Records)
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "sk-secret-123") {
		t.Errorf("secret survived: %s", data)
	}
	if !strings.Contains(string(data), "[REDACTED]") || !strings.Contains(string(data), "clean") {
		t.Errorf("replacement or untouched record lost: %s", data)
	}
}

// A replacement that would produce invalid JSON must abort without
// writing, rather than leave a corrupted transcript behind.
func TestReplaceRefusesToCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"secret"},"cwd":"/x"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Replace(p, "secret", `un"escaped`); err == nil {
		t.Error("expected an error for a corrupting replacement")
	}
	data, _ := os.ReadFile(p)
	if string(data) != body {
		t.Errorf("file was modified despite the error:\n%s", data)
	}
}

func TestReplaceNoMatchLeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	body := `{"type":"user","cwd":"/x"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	res, err := Replace(p, "absent", "x")
	if err != nil || res.Replacements != 0 {
		t.Fatalf("err=%v replacements=%d", err, res.Replacements)
	}
	after, _ := os.Stat(p)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("file was rewritten despite no matches")
	}
}
