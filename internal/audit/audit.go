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
		out = append(out, Entry{Time: c.Time, Project: project, Session: id, Tool: c.Tool, Detail: c.Detail})
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
