package session

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// searchFixture: two files, known match counts for "needle"
// (case-insensitive). fileA has 3 (two on one line, one capitalized on
// another); fileB has 0.
func writeSearchFixture(t *testing.T) string {
	t.Helper()
	return writeProjects(t, map[string]map[string]string{
		"-a": {"a.jsonl": "needle needle in a line\nnothing here\nNeedle again\n"},
		"-b": {"b.jsonl": "haystack only\nno match\n"},
	})
}

func TestGoSearch(t *testing.T) {
	root := writeSearchFixture(t)
	m, err := goSearch(root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("matched %d files, want 1: %v", len(m), m)
	}
	pathA := filepath.Join(root, "-a", "a.jsonl")
	if m[pathA] != 3 {
		t.Errorf("match count = %d, want 3", m[pathA])
	}
}

// The ripgrep path and the Go fallback must agree on counts, so the
// experience is identical whether or not rg is installed.
func TestSearchContentMatchesFallback(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed; nothing to cross-check")
	}
	root := writeSearchFixture(t)

	viaRg, err := SearchContent(root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	viaGo, err := goSearch(root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	if len(viaRg) != len(viaGo) {
		t.Fatalf("rg matched %d files, go matched %d", len(viaRg), len(viaGo))
	}
	for path, n := range viaGo {
		if viaRg[path] != n {
			t.Errorf("%s: rg=%d go=%d", path, viaRg[path], n)
		}
	}
}

func TestSearchNoMatches(t *testing.T) {
	root := writeSearchFixture(t)
	m, err := SearchContent(root, "definitely-absent-xyzzy")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("got %d matches, want 0", len(m))
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	root := writeSearchFixture(t)
	m, err := SearchContent(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Errorf("empty query returned %v, want nil", m)
	}
}
