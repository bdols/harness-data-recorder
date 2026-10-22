package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bdols/harness-data-recorder/internal/session"
)

// writeTranscript builds a transcript long enough to scroll, where each
// assistant turn carries usage so every annotation mode changes the
// rendered line count.
func writeTranscript(t *testing.T, turns int) string {
	t.Helper()
	var b strings.Builder
	for i := range turns {
		fmt.Fprintf(&b, `{"type":"user","timestamp":"2026-10-01T12:00:00Z","message":{"role":"user","content":"prompt %d"}}`+"\n", i)
		fmt.Fprintf(&b, `{"type":"assistant","timestamp":"2026-10-01T12:00:01Z","message":{"id":"msg_%d","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"answer %d"}],"usage":{"input_tokens":10,"cache_read_input_tokens":20000,"output_tokens":120}}}`+"\n", i, i)
	}
	path := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func detailModel(t *testing.T, path string) *Model {
	t.Helper()
	m := New(t.TempDir(), t.TempDir(), false)
	m.width, m.height = 80, 30
	m.detail.Width, m.detail.Height = 80, 27
	m.mode = modeDetail
	m.detailOf = &session.Session{ID: "s", Path: path, Title: "t"}
	m.setDetailContent()
	return &m
}

// Cycling the annotation mode changes how many lines sit above the
// reader, so without an anchor the viewport would jump — in practice
// all the way back to the top.
func TestDetailKeepsPositionAcrossAnnotationModes(t *testing.T) {
	m := detailModel(t, writeTranscript(t, 60))
	m.detail.SetYOffset(100)
	anchor, row := m.anchorEntry()
	if anchor <= 0 {
		t.Fatalf("expected an anchor entry partway down the transcript, got %d", anchor)
	}

	for _, mode := range []int{turnTokensFull, turnTokensOff, turnTokensMargin} {
		m.turnTokens = mode
		m.setDetailContent()
		gotAnchor, gotRow := m.anchorEntry()
		if gotAnchor != anchor {
			t.Errorf("mode %s: anchored on entry %d, want %d", turnTokenMode(mode), gotAnchor, anchor)
		}
		if gotRow != row {
			t.Errorf("mode %s: anchor sits on row %d, want %d", turnTokenMode(mode), gotRow, row)
		}
		if m.detail.YOffset == 0 {
			t.Errorf("mode %s: viewport jumped back to the top", turnTokenMode(mode))
		}
	}
}

// A different transcript is a fresh read and starts at the top, as does
// re-opening one after leaving the reader.
func TestDetailStartsAtTopForANewTranscript(t *testing.T) {
	m := detailModel(t, writeTranscript(t, 60))
	m.detail.SetYOffset(100)

	m.detailOf = &session.Session{ID: "other", Path: writeTranscript(t, 60), Title: "t"}
	m.setDetailContent()
	if m.detail.YOffset != 0 {
		t.Errorf("a new transcript opened at offset %d, want the top", m.detail.YOffset)
	}
}

func TestAnchorEntryWithoutContent(t *testing.T) {
	var m Model
	if idx, row := m.anchorEntry(); idx != -1 || row != 0 {
		t.Errorf("anchorEntry with no render = (%d, %d), want (-1, 0)", idx, row)
	}
}

// writeHitTranscript puts the needle in a user prompt, twice on one
// assistant line (so occurrences, not lines, are the unit), and in a
// tool line, padded with filler turns so the hits are pages apart.
func writeHitTranscript(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	filler := func(n int) {
		for range n {
			b.WriteString(`{"type":"user","timestamp":"2026-10-01T12:00:00Z","message":{"role":"user","content":"filler"}}` + "\n")
			b.WriteString(`{"type":"assistant","timestamp":"2026-10-01T12:00:01Z","message":{"id":"f","role":"assistant","content":[{"type":"text","text":"filler"}]}}` + "\n")
		}
	}
	filler(20)
	b.WriteString(`{"type":"user","timestamp":"2026-10-01T12:00:00Z","message":{"role":"user","content":"find the Needle please"}}` + "\n")
	filler(20)
	b.WriteString(`{"type":"assistant","timestamp":"2026-10-01T12:00:01Z","message":{"id":"a","role":"assistant","content":[{"type":"text","text":"needle and needle"}]}}` + "\n")
	filler(20)
	b.WriteString(`{"type":"assistant","timestamp":"2026-10-01T12:00:01Z","message":{"id":"b","role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"grep needle"}}]}}` + "\n")
	path := filepath.Join(t.TempDir(), "hits.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSearchHitsAreFoundInReadingOrder(t *testing.T) {
	m := detailModel(t, writeHitTranscript(t))
	m.contentQuery = "needle"
	m.setDetailContent()

	// Four occurrences: the prompt, two on one assistant line, the tool
	// call. Case-insensitive, matching how the content search greps.
	if len(m.detailHits) != 4 {
		t.Fatalf("found %d hits, want 4: %v", len(m.detailHits), m.detailHits)
	}
	if m.detailHits[1] != m.detailHits[2] {
		t.Errorf("the two hits on one line should share a line: %v", m.detailHits)
	}
	for i := 1; i < len(m.detailHits); i++ {
		if m.detailHits[i] < m.detailHits[i-1] {
			t.Fatalf("hits out of reading order: %v", m.detailHits)
		}
	}
}

// Opening a transcript during a content search should land on the first
// hit, which here is far enough down that the top would not show it.
func TestOpeningDuringASearchJumpsToTheFirstHit(t *testing.T) {
	m := detailModel(t, writeTranscript(t, 5)) // some other transcript
	m.contentQuery = "needle"
	m.detailOf = &session.Session{ID: "hits", Path: writeHitTranscript(t), Title: "t"}
	m.setDetailContent()

	if len(m.detailHits) == 0 {
		t.Fatal("no hits recorded")
	}
	if m.detail.YOffset == 0 {
		t.Fatal("opened at the top instead of at the first hit")
	}
	if !onScreen(m, m.detailHits[0]) {
		t.Errorf("first hit on line %d is off screen at offset %d", m.detailHits[0], m.detail.YOffset)
	}
}

func TestHitNavigationWrapsAndStaysOnScreen(t *testing.T) {
	m := detailModel(t, writeHitTranscript(t))
	m.contentQuery = "needle"
	m.setDetailContent()

	for i := 1; i <= len(m.detailHits); i++ { // one past the end, to wrap
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
		next := mm.(Model)
		m = &next
		want := i % len(m.detailHits)
		if m.hitCursor != want {
			t.Fatalf("after %d presses the cursor is %d, want %d", i, m.hitCursor, want)
		}
		if !onScreen(m, m.detailHits[m.hitCursor]) {
			t.Errorf("hit %d on line %d is off screen at offset %d", m.hitCursor, m.detailHits[m.hitCursor], m.detail.YOffset)
		}
	}
	// N walks back the other way, wrapping to the last hit.
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
	back := mm.(Model)
	if back.hitCursor != len(back.detailHits)-1 {
		t.Errorf("N from the first hit landed on %d, want the last", back.hitCursor)
	}
}

func onScreen(m *Model, line int) bool {
	return line >= m.detail.YOffset && line < m.detail.YOffset+m.detail.Height
}

func TestHitNoteReportsPositionAndAbsence(t *testing.T) {
	var m Model
	if got := m.hitNote(); got != "" {
		t.Errorf("no search active: %q, want empty", got)
	}
	m.contentQuery = "needle"
	if got := m.hitNote(); !strings.Contains(got, "not in the shown text") {
		t.Errorf("a query with no hits should say so: %q", got)
	}
	m.detailHits = []int{4, 9}
	m.hitCursor = 1
	if got := m.hitNote(); !strings.Contains(got, "hit 2/2") {
		t.Errorf("hitNote = %q, want it to report 2/2", got)
	}
}
