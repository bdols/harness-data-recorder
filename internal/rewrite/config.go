package rewrite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ConfigPath returns Claude Code's top-level config file.
func ConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// These are the per-project settings worth carrying to a moved project.
// Everything else in a project entry is accounting (last cost, token
// counts, durations) that belongs to the old location's history and would
// be misleading if copied forward.
var adoptKeys = []string{
	"allowedTools",
	"mcpContextUris",
	"mcpServers",
	"enabledMcpjsonServers",
	"disabledMcpjsonServers",
	"hasTrustDialogAccepted",
	"hasClaudeMdExternalIncludesApproved",
	"hasClaudeMdExternalIncludesWarningShown",
}

// AdoptProjectConfig copies the settings Claude Code keeps for oldCWD onto
// newCWD in ~/.claude.json, so a moved project keeps its trust decision,
// tool permissions and MCP servers instead of prompting from scratch.
//
// It only fills in keys the destination doesn't already have — an existing
// value at the new path always wins — and reports which keys it added.
//
// Claude Code rewrites this file itself, so a running process can clobber
// this (or be clobbered by it). Prefer running it while no session in
// either directory is live.
func AdoptProjectConfig(oldCWD, newCWD string) ([]string, error) {
	path := ConfigPath()
	if path == "" {
		return nil, fmt.Errorf("cannot locate ~/.claude.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Decode into RawMessage so the rest of the (large) config survives
	// untouched; only the projects map is re-encoded.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var projects map[string]map[string]json.RawMessage
	if raw, ok := top["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return nil, fmt.Errorf("parsing projects in %s: %w", path, err)
		}
	}
	if projects == nil {
		projects = map[string]map[string]json.RawMessage{}
	}
	src, ok := projects[oldCWD]
	if !ok {
		return nil, fmt.Errorf("no config recorded for %s", oldCWD)
	}
	dst, ok := projects[newCWD]
	if !ok {
		dst = map[string]json.RawMessage{}
	}
	var added []string
	for _, k := range adoptKeys {
		v, ok := src[k]
		if !ok {
			continue
		}
		if _, exists := dst[k]; exists {
			continue
		}
		dst[k] = v
		added = append(added, k)
	}
	if len(added) == 0 {
		return nil, nil
	}
	projects[newCWD] = dst
	enc, err := json.Marshal(projects)
	if err != nil {
		return nil, err
	}
	top["projects"] = enc
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(path, out); err != nil {
		return nil, err
	}
	return added, nil
}
