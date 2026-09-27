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

// Matches reports whether the entry satisfies every term (terms are
// ANDed). A term carrying a known selector prefix — "sandbox:", "sub:",
// "model:" — is tested against that field alone; any other term is a
// free-text substring over the tool, detail, project and model.
//
// The selectors have to be structured rather than folded into the
// free-text haystack. As a substring, "sandbox:bypass" also matches any
// command whose *text* mentions it — including csb's own audit commands,
// so the audit pollutes its own results as it grows — which silently
// inflates precisely the query a compliance review depends on being
// exact.
func (e Entry) Matches(terms []string) bool {
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		switch {
		case t == "":
			continue
		case strings.HasPrefix(t, "sandbox:"):
			if !e.matchesSandbox(strings.TrimPrefix(t, "sandbox:")) {
				return false
			}
		case strings.HasPrefix(t, "sub:"):
			// "sub:" alone means any subagent call; "sub:opus" narrows it
			// to the issuing model.
			m := strings.TrimPrefix(t, "sub:")
			if !e.Subagent || !strings.Contains(strings.ToLower(e.Model), m) {
				return false
			}
		case strings.HasPrefix(t, "model:"):
			if !strings.Contains(strings.ToLower(e.ModelShort()), strings.TrimPrefix(t, "model:")) {
				return false
			}
		default:
			if !strings.Contains(e.haystack(), t) {
				return false
			}
		}
	}
	return true
}

// haystack is the free-text search target: everything a bare term may
// match. Sandbox state is deliberately absent — it is reachable only
// through the sandbox: selector, so command text can never impersonate
// it.
func (e Entry) haystack() string {
	return strings.ToLower(e.Tool + " " + e.Detail + " " + e.Project + " " + e.ModelShort())
}

// matchesSandbox tests a sandbox: selector value. An unrecognised value
// matches nothing rather than falling back to free text: for an audit, a
// mistyped filter returning zero rows is a visible failure, while one
// returning every row quietly looks like a clean bill of health.
func (e Entry) matchesSandbox(v string) bool {
	switch v {
	case "": // any Bash call, whatever its sandbox state
		return e.Sandbox != session.SandboxNA
	case "bypass", "bypassed":
		return e.Sandbox == session.SandboxBypassed
	case "default":
		return e.Sandbox == session.SandboxDefault
	}
	return false
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
