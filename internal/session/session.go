// Package session parses Claude Code session .jsonl files from
// ~/.claude/projects/<munged-cwd>/<session-id>.jsonl.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Session is the summary of one .jsonl session file.
type Session struct {
	ID         string
	Path       string // path to the .jsonl file
	ProjectDir string // munged directory name under the projects root
	CWD        string // working directory recorded in the session
	Title      string
	LastPrompt string
	FirstTime  time.Time
	LastTime   time.Time
	UserMsgs   int
	AsstMsgs   int
	ToolCalls  int
	Model      string
	GitBranch  string
	Version    string // Claude Code version that last wrote the session
	Size       int64

	OutputTokens  int // sum of output tokens across assistant messages
	ContextTokens int // context size at the last assistant message
}

// line is a partial decode of one jsonl record; only the fields we
// care about, everything else is ignored.
type line struct {
	Type        string   `json:"type"`
	AITitle     string   `json:"aiTitle"`
	LastPrompt  string   `json:"lastPrompt"`
	Timestamp   string   `json:"timestamp"`
	CWD         string   `json:"cwd"`
	GitBranch   string   `json:"gitBranch"`
	IsMeta      bool     `json:"isMeta"`
	IsSidechain bool     `json:"isSidechain"`
	Version     string   `json:"version"`
	Message     *message `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *usage          `json:"usage"`
}

type usage struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
}

// maxLineSize bounds a single jsonl record; pasted images can make
// lines run to many megabytes.
const maxLineSize = 128 * 1024 * 1024

// DefaultRoot returns ~/.claude/projects.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// Scan parses every session file under root, newest first.
func Scan(root string) ([]Session, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(root, d.Name(), f.Name())
			s, err := parseFile(path)
			if err != nil {
				continue
			}
			s.ProjectDir = d.Name()
			if s.CWD == "" {
				s.CWD = unmungeDir(d.Name())
			}
			sessions = append(sessions, s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastTime.After(sessions[j].LastTime)
	})
	return sessions, nil
}

// unmungeDir approximates the original cwd from a munged project dir
// name ("-var-home-bd-code-foo" -> "/var/home/bd/code/foo"). Lossy:
// dashes in real path components stay slashes, so it is only a
// fallback when the session records no cwd.
func unmungeDir(name string) string {
	return strings.ReplaceAll(name, "-", "/")
}

func parseFile(path string) (Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer f.Close()

	s := Session{
		ID:   strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Path: path,
	}
	var mtime time.Time
	if fi, err := f.Stat(); err == nil {
		s.Size = fi.Size()
		mtime = fi.ModTime()
	}

	var firstPrompt string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, l.Timestamp); err == nil {
			if s.FirstTime.IsZero() || ts.Before(s.FirstTime) {
				s.FirstTime = ts
			}
			if ts.After(s.LastTime) {
				s.LastTime = ts
			}
		}
		if l.CWD != "" {
			s.CWD = l.CWD
		}
		if l.GitBranch != "" {
			s.GitBranch = l.GitBranch
		}
		switch l.Type {
		case "ai-title":
			if l.AITitle != "" {
				s.Title = l.AITitle
			}
		case "last-prompt":
			if l.LastPrompt != "" {
				s.LastPrompt = l.LastPrompt
			}
		case "user":
			if text, ok := userPromptText(l); ok {
				s.UserMsgs++
				if firstPrompt == "" {
					firstPrompt = text
				}
			}
		case "assistant":
			if !l.IsSidechain && l.Message != nil {
				s.AsstMsgs++
				if l.Message.Model != "" && !strings.HasPrefix(l.Message.Model, "<") {
					s.Model = l.Message.Model
				}
				if u := l.Message.Usage; u != nil {
					s.OutputTokens += u.OutputTokens
					if ctx := u.InputTokens + u.CacheRead + u.CacheCreation; ctx > 0 {
						s.ContextTokens = ctx
					}
				}
				var blocks []contentBlock
				if err := json.Unmarshal(l.Message.Content, &blocks); err == nil {
					for _, b := range blocks {
						if b.Type == "tool_use" {
							s.ToolCalls++
						}
					}
				}
			}
		}
		if l.Version != "" {
			s.Version = l.Version
		}
	}
	if err := sc.Err(); err != nil {
		return Session{}, err
	}
	// Record timestamps are the truth for activity; Claude Code can
	// touch/rewrite files long after the last message, so mtime is
	// only a fallback when no timestamps parsed at all.
	if s.LastTime.IsZero() {
		s.LastTime = mtime
	}
	if s.Title == "" {
		s.Title = firstLine(firstPrompt)
	}
	if s.LastPrompt == "" {
		s.LastPrompt = firstPrompt
	}
	return s, nil
}

// userPromptText reports whether l is a human-typed prompt and
// returns its text. Meta records, sidechains, tool results and
// <command-*>/<local-command-*> harness records are excluded.
func userPromptText(l line) (string, bool) {
	if l.IsMeta || l.IsSidechain || l.Message == nil {
		return "", false
	}
	var text string
	if err := json.Unmarshal(l.Message.Content, &text); err != nil {
		return "", false
	}
	t := strings.TrimSpace(text)
	if t == "" || strings.HasPrefix(t, "<command-") || strings.HasPrefix(t, "<local-command-") {
		return "", false
	}
	return t, true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Entry is one displayable transcript item.
type Entry struct {
	Role string // "user", "assistant" or "tool"
	Text string
	Time time.Time
}

var commandNameRe = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
var commandArgsRe = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)

// ParseFile parses a single session .jsonl into a Session, for callers
// that have a file path rather than a scan root (e.g. exporting a
// specific backup version).
func ParseFile(path string) (Session, error) {
	return parseFile(path)
}

// Markdown renders a session's transcript as a Markdown document. Header
// labels come from s (title, id, project, model); message counts and the
// date range are derived from the transcript itself, so it's accurate for
// any file — a live session, a backup, or a specific version.
func Markdown(s Session) (string, error) {
	entries, err := LoadTranscript(s.Path)
	if err != nil {
		return "", err
	}
	var users, assts, tools int
	var first, last time.Time
	for _, e := range entries {
		switch e.Role {
		case "user":
			users++
		case "assistant":
			assts++
		case "tool":
			tools++
		}
		if !e.Time.IsZero() {
			if first.IsZero() || e.Time.Before(first) {
				first = e.Time
			}
			if e.Time.After(last) {
				last = e.Time
			}
		}
	}

	var b strings.Builder
	title := s.Title
	if title == "" {
		title = s.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	if s.ID != "" {
		fmt.Fprintf(&b, "- **Session:** `%s`\n", s.ID)
	}
	if s.CWD != "" {
		fmt.Fprintf(&b, "- **Project:** `%s`\n", s.CWD)
	}
	if s.Model != "" {
		fmt.Fprintf(&b, "- **Model:** %s\n", s.Model)
	}
	if !first.IsZero() {
		fmt.Fprintf(&b, "- **Started:** %s\n", first.Local().Format("2006-01-02 15:04"))
	}
	if !last.IsZero() {
		fmt.Fprintf(&b, "- **Last active:** %s\n", last.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(&b, "- **Messages:** %d user · %d assistant · %d tool calls\n\n---\n\n", users, assts, tools)

	for _, e := range entries {
		switch e.Role {
		case "user":
			fmt.Fprintf(&b, "## 👤 User\n\n%s\n\n", strings.TrimSpace(e.Text))
		case "assistant":
			fmt.Fprintf(&b, "## 🤖 Assistant\n\n%s\n\n", strings.TrimSpace(e.Text))
		case "tool":
			fmt.Fprintf(&b, "> `⚙` %s\n\n", strings.TrimSpace(e.Text))
		}
	}
	return b.String(), nil
}

// LoadTranscript parses the full conversation of a session file into
// displayable entries.
func LoadTranscript(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		if l.Message == nil || l.IsSidechain {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, l.Timestamp)
		switch l.Type {
		case "user":
			if l.IsMeta {
				continue
			}
			var text string
			if err := json.Unmarshal(l.Message.Content, &text); err != nil {
				continue // block content: tool results etc.
			}
			t := strings.TrimSpace(text)
			if t == "" || strings.HasPrefix(t, "<local-command-") {
				continue
			}
			if m := commandNameRe.FindStringSubmatch(t); m != nil {
				cmd := strings.TrimSpace(m[1])
				if am := commandArgsRe.FindStringSubmatch(t); am != nil && strings.TrimSpace(am[1]) != "" {
					cmd += " " + strings.TrimSpace(am[1])
				}
				t = cmd
			}
			entries = append(entries, Entry{Role: "user", Text: t, Time: ts})
		case "assistant":
			var blocks []contentBlock
			if err := json.Unmarshal(l.Message.Content, &blocks); err != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if t := strings.TrimSpace(b.Text); t != "" {
						entries = append(entries, Entry{Role: "assistant", Text: t, Time: ts})
					}
				case "tool_use":
					entries = append(entries, Entry{Role: "tool", Text: b.Name + toolInputSummary(b.Input), Time: ts})
				}
			}
		}
	}
	return entries, sc.Err()
}

// ToolCall is one tool invocation (Bash, Edit, Read, …) from a
// transcript, for the command/tool audit.
type ToolCall struct {
	Time     time.Time
	Tool     string  // tool name, e.g. "Bash", "Edit"
	Detail   string  // the salient input (command, file path, pattern, …)
	Model    string  // model that issued the call, e.g. "claude-opus-5-5"
	Sandbox  Sandbox // Bash only: whether the sandbox was bypassed
	Subagent bool    // issued by a subagent (Agent tool), not the main loop
}

// Sandbox records whether a Bash command bypassed Claude Code's sandbox.
// Transcripts don't record whether sandboxing was enabled at all, so
// SandboxDefault means "sandboxed if the project had sandbox.enabled".
type Sandbox int

const (
	SandboxNA       Sandbox = iota // not a Bash call
	SandboxDefault                 // ran under the configured sandbox setting
	SandboxBypassed                // dangerouslyDisableSandbox took effect
)

// ToolCalls extracts every tool invocation from a session file, in order,
// including those made by its subagents (<id>/subagents/*.jsonl beside a
// live transcript).
func ToolCalls(path string) ([]ToolCall, error) {
	out, err := toolCallsFile(path, false)
	if err != nil {
		return nil, err
	}
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	if len(subs) == 0 {
		return out, nil
	}
	for _, p := range subs {
		if calls, err := toolCallsFile(p, true); err == nil {
			out = append(out, calls...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// toolCallLine is the subset of a record ToolCalls needs; toolUseResult is
// kept out of the shared line struct since it can be large.
type toolCallLine struct {
	Type          string          `json:"type"`
	Timestamp     string          `json:"timestamp"`
	IsSidechain   bool            `json:"isSidechain"`
	Message       *message        `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

func toolCallsFile(path string, subagent bool) ([]ToolCall, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []ToolCall
	byID := map[string]int{} // tool_use id → index into out (Bash only)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	for sc.Scan() {
		var l toolCallLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		if l.Message == nil || (l.IsSidechain && !subagent) {
			continue
		}
		var blocks []contentBlock
		if err := json.Unmarshal(l.Message.Content, &blocks); err != nil {
			continue
		}
		switch l.Type {
		case "assistant":
			ts, _ := time.Parse(time.RFC3339, l.Timestamp)
			model := l.Message.Model
			if model == "<synthetic>" {
				model = ""
			}
			for _, b := range blocks {
				if b.Type != "tool_use" {
					continue
				}
				c := ToolCall{Time: ts, Tool: b.Name, Detail: toolDetail(b.Input), Model: model, Subagent: subagent}
				if b.Name == "Bash" {
					c.Sandbox = sandboxOf(b.Input)
					if b.ID != "" {
						byID[b.ID] = len(out)
					}
				}
				out = append(out, c)
			}
		case "user":
			// The tool result echoes the sandbox setting that actually
			// took effect, which wins over what the input asked for.
			if len(l.ToolUseResult) == 0 || l.ToolUseResult[0] != '{' {
				continue
			}
			for _, b := range blocks {
				if i, ok := byID[b.ToolUseID]; ok && b.Type == "tool_result" {
					if disable, ok := disableSandbox(l.ToolUseResult); ok {
						out[i].Sandbox = sandboxFrom(disable)
					}
				}
			}
		}
	}
	return out, sc.Err()
}

// sandboxOf reports how a Bash input asked to run; a missing flag means
// the default.
func sandboxOf(input json.RawMessage) Sandbox {
	disable, _ := disableSandbox(input)
	return sandboxFrom(disable)
}

func sandboxFrom(disable bool) Sandbox {
	if disable {
		return SandboxBypassed
	}
	return SandboxDefault
}

// disableSandbox reads dangerouslyDisableSandbox (a bool, or occasionally
// the string "true") and whether it was present at all.
func disableSandbox(raw json.RawMessage) (disable, present bool) {
	var in struct {
		Disable any `json:"dangerouslyDisableSandbox"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.Disable == nil {
		return false, false
	}
	return in.Disable == true || in.Disable == "true", true
}

// toolDetail picks the most audit-relevant field from a tool input,
// preferring the command actually run over its description.
func toolDetail(raw json.RawMessage) string {
	var in struct {
		Command     string `json:"command"`
		FilePath    string `json:"file_path"`
		Path        string `json:"path"`
		Pattern     string `json:"pattern"`
		Query       string `json:"query"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	for _, v := range []string{in.Command, in.FilePath, in.Path, in.Pattern, in.Query, in.URL, in.Prompt, in.Description} {
		if v != "" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

// toolInputSummary extracts a short human hint from a tool_use input.
func toolInputSummary(raw json.RawMessage) string {
	var in struct {
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
		Pattern     string `json:"pattern"`
		Command     string `json:"command"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	for _, v := range []string{in.Description, in.FilePath, in.Pattern, firstLine(in.Command)} {
		if v != "" {
			return ": " + v
		}
	}
	return ""
}
