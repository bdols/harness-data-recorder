package session

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// searchFixture: three sessions.
//
//   - a.jsonl  four occurrences of "needle" in what the reader shows:
//     one capitalised in the prompt, two on one assistant line, and
//     one in a tool-call line (its rendered detail is the command).
//     A fifth sits in a thinking block, which only a deep search sees.
//   - b.jsonl  no occurrence anywhere.
//   - c.jsonl  no occurrence in the conversation, but the word "human"
//     in the record envelope — the shape that made a raw grep match
//     almost every real session.
func writeSearchFixture(t *testing.T) string {
	t.Helper()
	a := `{"type":"user","timestamp":"2026-10-01T12:00:00Z","message":{"role":"user","content":"find the Needle please"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-10-01T12:00:01Z","message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"a needle in thinking"},{"type":"text","text":"needle and needle"}]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-10-01T12:00:02Z","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"grep needle /tmp/x"}}]}}` + "\n"
	b := `{"type":"user","timestamp":"2026-10-01T12:00:00Z","message":{"role":"user","content":"haystack only"}}` + "\n"
	c := `{"type":"user","timestamp":"2026-10-01T12:00:00Z","origin":{"kind":"human"},"turnOrigin":"human","message":{"role":"user","content":"nothing to see"}}` + "\n"
	return writeProjects(t, map[string]map[string]string{
		"-a": {"a.jsonl": a},
		"-b": {"b.jsonl": b},
		"-c": {"c.jsonl": c},
	})
}

// The content search counts what the reader shows: the prompt, the two
// hits on the reply line, and the tool-call line (whose rendered detail
// carries the command). The thinking block is not shown, so it is not
// counted.
func TestSearchContentCountsShownText(t *testing.T) {
	root := writeSearchFixture(t)
	m, err := SearchContent(root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("matched %d files, want 1: %v", len(m), m)
	}
	if got := m[filepath.Join(root, "-a", "a.jsonl")]; got != 4 {
		t.Errorf("match count = %d, want 4", got)
	}
}

// The regression that started this: Claude Code stamps every typed
// prompt with "origin":{"kind":"human"}, so a raw grep for a common
// word matched sessions that never used it.
func TestSearchContentIgnoresRecordMetadata(t *testing.T) {
	root := writeSearchFixture(t)
	m, err := SearchContent(root, "human")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("content search matched envelope metadata: %v", m)
	}

	deep, err := SearchDeep(root, "human")
	if err != nil {
		t.Fatal(err)
	}
	if deep[filepath.Join(root, "-c", "c.jsonl")] != 2 {
		t.Errorf("deep search should still find both stamps: %v", deep)
	}
}

// Deep search reaches what the reader never draws.
func TestSearchDeepReachesThinkingAndMetadata(t *testing.T) {
	root := writeSearchFixture(t)
	content, err := SearchContent(root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	deep, err := SearchDeep(root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	pathA := filepath.Join(root, "-a", "a.jsonl")
	if deep[pathA] <= content[pathA] {
		t.Errorf("deep (%d) should exceed content (%d): thinking is only in the file",
			deep[pathA], content[pathA])
	}
}

// The ripgrep path and the Go fallback must agree, so the experience is
// identical whether or not rg is installed.
func TestSearchEnginesAgree(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed; nothing to cross-check")
	}
	root := writeSearchFixture(t)
	for _, q := range []string{"needle", "human", "haystack"} {
		viaRg, err := rgCount("rg", root, q)
		if err != nil {
			t.Fatal(err)
		}
		viaGo, err := goCount(root, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(viaRg) != len(viaGo) {
			t.Fatalf("%q: rg matched %d files, go matched %d", q, len(viaRg), len(viaGo))
		}
		for path, n := range viaGo {
			if viaRg[path] != n {
				t.Errorf("%q in %s: rg=%d go=%d", q, path, viaRg[path], n)
			}
		}
	}

	// And the two candidate lists must agree, or the content search
	// would miss files depending on which engine ran the prefilter.
	rgC, err := rgCandidates("rg", root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	goC, err := goCandidates(root, "needle")
	if err != nil {
		t.Fatal(err)
	}
	if len(rgC) != len(goC) {
		t.Errorf("candidates differ: rg=%v go=%v", rgC, goC)
	}
}

func TestSearchNoMatches(t *testing.T) {
	root := writeSearchFixture(t)
	for _, f := range []func(string, string) (map[string]int, error){SearchContent, SearchDeep} {
		m, err := f(root, "definitely-absent-xyzzy")
		if err != nil {
			t.Fatal(err)
		}
		if len(m) != 0 {
			t.Errorf("got %d matches, want 0", len(m))
		}
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	root := writeSearchFixture(t)
	for _, f := range []func(string, string) (map[string]int, error){SearchContent, SearchDeep} {
		m, err := f(root, "")
		if err != nil {
			t.Fatal(err)
		}
		if m != nil {
			t.Errorf("empty query returned %v, want nil", m)
		}
	}
}
