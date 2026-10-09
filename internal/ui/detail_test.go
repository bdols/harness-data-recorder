package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdols/claude-session-browser/internal/session"
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
