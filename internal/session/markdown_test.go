package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownRendersTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sid.jsonl")
	content := `{"type":"ai-title","aiTitle":"Hydroponics Plan","sessionId":"sid"}
{"type":"user","message":{"role":"user","content":"how much lettuce"},"timestamp":"2026-05-28T10:00:00Z","cwd":"/home/x"}
{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"about six heads"}]},"timestamp":"2026-05-28T10:01:00Z"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md, err := Markdown(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Hydroponics Plan",
		"**Session:** `sid`",
		"**Project:** `/home/x`",
		"1 user · 1 assistant",
		"## 👤 User",
		"how much lettuce",
		"## 🤖 Assistant",
		"about six heads",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n--- got ---\n%s", want, md)
		}
	}
}
