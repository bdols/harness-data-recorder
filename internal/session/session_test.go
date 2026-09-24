package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A realistic session fixture exercising every record type the parser
// cares about: ai-title, last-prompt, a real user prompt, a meta
// record, a slash-command record, and two assistant turns carrying
// usage and tool_use blocks.
const fullSession = `{"type":"ai-title","aiTitle":"Test Session","sessionId":"sid"}
{"type":"last-prompt","lastPrompt":"final prompt","sessionId":"sid"}
{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>ignore me"},"timestamp":"2026-07-04T18:50:00Z","cwd":"/home/test/proj","version":"2.1.201","gitBranch":"main"}
{"type":"user","message":{"role":"user","content":"<command-name>/model</command-name>"},"timestamp":"2026-07-04T18:50:30Z","cwd":"/home/test/proj"}
{"type":"user","message":{"role":"user","content":"first real prompt"},"timestamp":"2026-07-04T18:51:09Z","cwd":"/home/test/proj","gitBranch":"main"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"working on it"},{"type":"tool_use","name":"Bash","input":{"command":"ls","description":"list files"}}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":100,"cache_creation_input_tokens":5}},"timestamp":"2026-07-04T18:52:09Z"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}],"usage":{"input_tokens":50,"output_tokens":30,"cache_read_input_tokens":150,"cache_creation_input_tokens":0}},"timestamp":"2026-07-04T18:53:09Z","version":"2.1.201"}
`

// writeProjects builds a temp projects root: root/<dir>/<name>.jsonl.
func writeProjects(t *testing.T, files map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for dir, sessions := range files {
		d := filepath.Join(root, dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range sessions {
			if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestParseFileFields(t *testing.T) {
	root := writeProjects(t, map[string]map[string]string{
		"-home-test-proj": {"sid.jsonl": fullSession},
	})
	path := filepath.Join(root, "-home-test-proj", "sid.jsonl")

	s, err := parseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ID", s.ID, "sid"},
		{"Title", s.Title, "Test Session"},
		{"LastPrompt", s.LastPrompt, "final prompt"},
		{"CWD", s.CWD, "/home/test/proj"},
		{"GitBranch", s.GitBranch, "main"},
		{"Version", s.Version, "2.1.201"},
		{"Model", s.Model, "claude-opus-4-8"},
		{"UserMsgs", s.UserMsgs, 1}, // meta + command excluded
		{"AsstMsgs", s.AsstMsgs, 2},
		{"ToolCalls", s.ToolCalls, 2},
		{"OutputTokens", s.OutputTokens, 50},    // 20 + 30
		{"ContextTokens", s.ContextTokens, 200}, // 50+150+0 at last turn
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	wantFirst := time.Date(2026, 7, 4, 18, 50, 0, 0, time.UTC)
	if !s.FirstTime.Equal(wantFirst) {
		t.Errorf("FirstTime = %v, want %v", s.FirstTime, wantFirst)
	}
	wantLast := time.Date(2026, 7, 4, 18, 53, 9, 0, time.UTC)
	if !s.LastTime.Equal(wantLast) {
		t.Errorf("LastTime = %v, want %v", s.LastTime, wantLast)
	}
}

// A session with no ai-title should fall back to the first line of the
// first real user prompt for its title.
func TestParseFileTitleFallback(t *testing.T) {
	const noTitle = `{"type":"user","message":{"role":"user","content":"do a big thing\nwith details"},"timestamp":"2026-07-04T10:00:00Z","cwd":"/p"}
{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}]},"timestamp":"2026-07-04T10:00:05Z"}
`
	root := writeProjects(t, map[string]map[string]string{"-p": {"s.jsonl": noTitle}})
	s, err := parseFile(filepath.Join(root, "-p", "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "do a big thing" {
		t.Errorf("Title = %q, want first line of prompt", s.Title)
	}
	if s.LastPrompt != "do a big thing\nwith details" {
		t.Errorf("LastPrompt = %q, want full first prompt", s.LastPrompt)
	}
}

// When no record carries a parseable timestamp, LastTime falls back to
// the file mtime rather than the zero time.
func TestParseFileMtimeFallback(t *testing.T) {
	const noTimestamps = `{"type":"user","message":{"role":"user","content":"hi"},"cwd":"/p"}
`
	root := writeProjects(t, map[string]map[string]string{"-p": {"s.jsonl": noTimestamps}})
	path := filepath.Join(root, "-p", "s.jsonl")
	mtime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	s, err := parseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.LastTime.Equal(mtime) {
		t.Errorf("LastTime = %v, want mtime %v", s.LastTime, mtime)
	}
}

func TestScanSortsNewestFirst(t *testing.T) {
	older := `{"type":"user","message":{"role":"user","content":"old"},"timestamp":"2026-07-01T00:00:00Z","cwd":"/a"}
`
	newer := `{"type":"user","message":{"role":"user","content":"new"},"timestamp":"2026-07-04T00:00:00Z","cwd":"/b"}
`
	root := writeProjects(t, map[string]map[string]string{
		"-a": {"old.jsonl": older},
		"-b": {"new.jsonl": newer},
	})
	sessions, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	if sessions[0].ID != "new" {
		t.Errorf("first session = %q, want newest (new)", sessions[0].ID)
	}
}

// A session that records no cwd falls back to the un-munged project
// directory name.
func TestScanCWDFallback(t *testing.T) {
	const noCWD = `{"type":"user","message":{"role":"user","content":"hi"},"timestamp":"2026-07-04T00:00:00Z"}
`
	root := writeProjects(t, map[string]map[string]string{"-var-home-bd-code-foo": {"s.jsonl": noCWD}})
	sessions, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if got := sessions[0].CWD; got != "/var/home/bd/code/foo" {
		t.Errorf("CWD fallback = %q, want un-munged dir", got)
	}
}

func TestLoadTranscript(t *testing.T) {
	root := writeProjects(t, map[string]map[string]string{
		"-home-test-proj": {"sid.jsonl": fullSession},
	})
	entries, err := LoadTranscript(filepath.Join(root, "-home-test-proj", "sid.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	var roles []string
	for _, e := range entries {
		roles = append(roles, e.Role)
	}
	// Expected: /model command, real user prompt, assistant text,
	// tool_use, tool_use. Meta record is dropped.
	want := []string{"user", "user", "assistant", "tool", "tool"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	if entries[0].Text != "/model" {
		t.Errorf("command entry = %q, want %q", entries[0].Text, "/model")
	}
	if entries[3].Text != "Bash: list files" {
		t.Errorf("tool entry = %q, want name + description", entries[3].Text)
	}
}

func TestUnmungeDir(t *testing.T) {
	if got := unmungeDir("-var-home-bd-code-foo"); got != "/var/home/bd/code/foo" {
		t.Errorf("unmungeDir = %q", got)
	}
}

// ToolCalls records the issuing model, whether Bash bypassed the sandbox
// (the tool result's echo wins over the input), and subagent calls.
func TestToolCallsModelSandboxSubagent(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "sid.jsonl")
	body := `{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},"timestamp":"2026-05-01T10:00:00Z"}
{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"curl x","dangerouslyDisableSandbox":true}}]},"timestamp":"2026-05-01T10:01:00Z"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2"}]},"toolUseResult":{"stdout":"","dangerouslyDisableSandbox":false}}
{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"kubectl get po","dangerouslyDisableSandbox":"true"}}]},"timestamp":"2026-05-01T10:03:00Z"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3"}]},"toolUseResult":"Error: Exit code 1"}
{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"tool_use","id":"t4","name":"Read","input":{"file_path":"/x"}}]},"timestamp":"2026-05-01T10:04:00Z"}
`
	if err := os.WriteFile(main, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sid", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	subBody := `{"type":"assistant","isSidechain":true,"message":{"model":"claude-sonnet-5","content":[{"type":"tool_use","id":"s1","name":"Bash","input":{"command":"grep y"}}]},"timestamp":"2026-05-01T10:02:00Z"}
`
	if err := os.WriteFile(filepath.Join(sub, "agent-a1.jsonl"), []byte(subBody), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ToolCalls(main)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolCall{
		{Tool: "Bash", Detail: "ls", Model: "claude-opus-5-5", Sandbox: SandboxDefault},
		{Tool: "Bash", Detail: "curl x", Model: "claude-opus-5-5", Sandbox: SandboxDefault},
		{Tool: "Bash", Detail: "grep y", Model: "claude-sonnet-5", Sandbox: SandboxDefault, Subagent: true},
		{Tool: "Bash", Detail: "kubectl get po", Model: "claude-sonnet-4-6", Sandbox: SandboxBypassed},
		{Tool: "Read", Detail: "/x", Model: "", Sandbox: SandboxNA},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d calls, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		g.Time = time.Time{}
		if g != w {
			t.Errorf("call %d = %+v, want %+v", i, g, w)
		}
	}
}
