// Package audit builds a chronological log of every tool call (Bash,
// Edit, Read, …) Claude Code made, across live sessions and — so reaped
// sessions are still covered — the versioned backup store.
package audit

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bdols/claude-session-browser/internal/backup"
	"github.com/bdols/claude-session-browser/internal/session"
)

// Entry is one tool call with its session/project context.
type Entry struct {
	Time    time.Time
	Project string
	Session string // session id
	Tool    string
	Detail  string
	Model   string // e.g. "claude-opus-5-5"; empty if unrecorded
	Sandbox session.Sandbox
	// Subagent is set for calls made by an Agent-tool subagent.
	Subagent bool
}

// ModelShort is the model without its "claude-" prefix, marked "sub:"
// for subagent calls.
func (e Entry) ModelShort() string {
	m := strings.TrimPrefix(e.Model, "claude-")
	if m == "" {
		m = "?"
	}
	if e.Subagent {
		m = "sub:" + m
	}
	return m
}

// SandboxLabel is "BYPASS" for a Bash call that ran with
// dangerouslyDisableSandbox, "default" for other Bash calls (sandboxed
// only if the project enabled it), and "" for other tools.
func (e Entry) SandboxLabel() string {
	switch e.Sandbox {
	case session.SandboxDefault:
		return "default"
	case session.SandboxBypassed:
		return "BYPASS"
	}
	return ""
}

// Matches reports whether every (lowercased) term appears in the entry's
// tool, detail, project, or model. Bash calls also match
// "sandbox:default" or "sandbox:bypass", and subagent calls match "sub:".
func (e Entry) Matches(terms []string) bool {
	hay := strings.ToLower(e.Tool + " " + e.Detail + " " + e.Project + " " + e.ModelShort())
	if l := e.SandboxLabel(); l != "" {
		hay += " sandbox:" + strings.ToLower(l)
	}
	for _, t := range terms {
		if !strings.Contains(hay, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

// Collect gathers tool calls from every live session under root plus any
// backed-up session that no longer exists live, newest first.
func Collect(root, backupDir string) []Entry {
	var out []Entry
	seen := map[string]bool{}

	sessions, _ := session.Scan(root)
	for _, s := range sessions {
		seen[s.ID] = true
		out = append(out, callsOf(s.Path, projName(s.CWD), s.ID)...)
	}

	for _, st := range backup.Sessions(backupDir) {
		if seen[st.ID] {
			continue
		}
		project := unmunge(st.Project)
		if s, err := session.ParseFile(st.Path); err == nil && s.CWD != "" {
			project = projName(s.CWD)
		}
		out = append(out, callsOf(st.Path, project, st.ID)...)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

func callsOf(path, project, id string) []Entry {
	calls, err := session.ToolCalls(path)
	if err != nil {
		return nil
	}
	out := make([]Entry, 0, len(calls))
	for _, c := range calls {
		out = append(out, Entry{Time: c.Time, Project: project, Session: id, Tool: c.Tool, Detail: c.Detail,
			Model: c.Model, Sandbox: c.Sandbox, Subagent: c.Subagent})
	}
	return out
}

func projName(cwd string) string {
	if cwd == "" {
		return "?"
	}
	return filepath.Base(cwd)
}

// unmunge turns a munged project dir ("-var-home-bd-obsidian") back into a
// short name, best-effort — only used when a backup lacks a cwd record.
func unmunge(m string) string {
	return projName(strings.ReplaceAll(strings.TrimPrefix(m, "-"), "-", "/"))
}
