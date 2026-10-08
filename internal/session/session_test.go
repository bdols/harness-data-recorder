package session

import (
	"os"
	"path/filepath"
	"strings"
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
		{"Tokens.Output", s.Tokens.Output, 50},  // 20 + 30
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

// Token accounting: billed counters sum per API call, context does not
// (each call resends the conversation), and the per-model split is what
// a later cost estimate needs.
func TestTokenAccounting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sid.jsonl")
	body := `{"type":"ai-title","aiTitle":"T","sessionId":"sid"}
{"type":"assistant","message":{"model":"claude-opus-5","content":[],"usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200,"output_tokens_details":{"thinking_tokens":40},"cache_creation":{"ephemeral_1h_input_tokens":150,"ephemeral_5m_input_tokens":50}}},"timestamp":"2026-05-01T10:00:00Z"}
{"type":"assistant","message":{"model":"claude-sonnet-5","content":[],"usage":{"input_tokens":5,"output_tokens":20,"cache_read_input_tokens":3000,"cache_creation_input_tokens":0}},"timestamp":"2026-05-01T10:01:00Z"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Tokens
	want := Tokens{Input: 15, CacheWrite: 200, CacheWrite1h: 150, CacheWrite5m: 50,
		CacheRead: 4000, Output: 120, Thinking: 40}
	if got != want {
		t.Errorf("Tokens = %+v, want %+v", got, want)
	}
	if b := got.Billed(); b != 15+200+4000+120 {
		t.Errorf("Billed = %d, want %d", b, 15+200+4000+120)
	}
	// Context is the last call's size, not a running sum.
	if s.ContextTokens != 3005 {
		t.Errorf("ContextTokens = %d, want 3005 (last call only)", s.ContextTokens)
	}
	if len(s.ByModel) != 2 {
		t.Fatalf("ByModel has %d entries, want 2: %+v", len(s.ByModel), s.ByModel)
	}
	if o := s.ByModel["claude-opus-5"]; o.Output != 100 || o.CacheRead != 1000 || o.Thinking != 40 {
		t.Errorf("opus split = %+v", o)
	}
	if n := s.ByModel["claude-sonnet-5"]; n.Output != 20 || n.CacheRead != 3000 {
		t.Errorf("sonnet split = %+v", n)
	}
}

// A session with no usage blocks must report zero, not crash or invent.
func TestTokenAccountingAbsent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sid.jsonl")
	body := `{"type":"ai-title","aiTitle":"T","sessionId":"sid"}
{"type":"user","message":{"role":"user","content":"hi"},"timestamp":"2026-05-01T10:00:00Z"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tokens.Billed() != 0 || s.ContextTokens != 0 || len(s.ByModel) != 0 {
		t.Errorf("want zero usage, got %+v ctx=%d byModel=%v", s.Tokens, s.ContextTokens, s.ByModel)
	}
}

// Claude Code writes one API response as several records when it makes
// tool calls — the text, then one per call — each carrying an identical
// copy of the usage block. Counting per record inflated real totals by
// ~149%, so usage must be counted once per response id.
func TestTokenAccountingDedupesSplitResponses(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sid.jsonl")
	// One response (msg_a) spread over three records, then a separate one.
	body := `{"type":"ai-title","aiTitle":"T","sessionId":"sid"}
{"type":"assistant","message":{"id":"msg_a","model":"claude-opus-5","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}},"timestamp":"2026-05-01T10:00:00Z"}
{"type":"assistant","message":{"id":"msg_a","model":"claude-opus-5","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}},"timestamp":"2026-05-01T10:00:00Z"}
{"type":"assistant","message":{"id":"msg_a","model":"claude-opus-5","content":[{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}],"usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}},"timestamp":"2026-05-01T10:00:00Z"}
{"type":"assistant","message":{"id":"msg_b","model":"claude-opus-5","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":5,"output_tokens":50,"cache_read_input_tokens":2000,"cache_creation_input_tokens":0}},"timestamp":"2026-05-01T10:01:00Z"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// msg_a counted once (10/200/1000/100) plus msg_b (5/0/2000/50).
	want := Tokens{Input: 15, CacheWrite: 200, CacheRead: 3000, Output: 150}
	if s.Tokens != want {
		t.Errorf("Tokens = %+v, want %+v (msg_a must count once, not three times)", s.Tokens, want)
	}
	if m := s.ByModel["claude-opus-5"]; m != want {
		t.Errorf("ByModel = %+v, want %+v", m, want)
	}

	// The transcript annotation must likewise appear once per response,
	// on the turn's final entry, not repeated after every tool call.
	entries, err := LoadTranscript(p)
	if err != nil {
		t.Fatal(err)
	}
	var annotated int
	var lastAnnotatedRole string
	for _, e := range entries {
		if e.Context > 0 || e.Tokens.Output > 0 {
			annotated++
			lastAnnotatedRole = e.Role
		}
	}
	if annotated != 2 {
		t.Errorf("%d annotated entries, want 2 (one per response): %+v", annotated, entries)
	}
	if lastAnnotatedRole != "assistant" {
		t.Errorf("last annotation on %q, want the assistant text of msg_b", lastAnnotatedRole)
	}
	// msg_a's annotation belongs to its final entry, the Read tool call.
	for i, e := range entries {
		if e.Role == "tool" && strings.HasPrefix(e.Text, "Read") {
			if e.Context == 0 {
				t.Errorf("entry %d (%s) should carry msg_a's usage", i, e.Text)
			}
		}
		if e.Role == "tool" && strings.HasPrefix(e.Text, "Bash") && e.Context != 0 {
			t.Errorf("entry %d (%s) should not repeat msg_a's usage", i, e.Text)
		}
	}
}
