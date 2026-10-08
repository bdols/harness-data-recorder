package ui

import (
	"testing"

	"github.com/bdols/claude-session-browser/internal/session"
)

// The ramp has to be logarithmic: real sessions span 75K to 350M billed
// tokens, four orders of magnitude that no linear bar could show.
func TestTokenGlyphRamp(t *testing.T) {
	cases := []struct {
		billed int
		want   string
	}{
		{1, "▁"},
		{99_999, "▁"},
		{100_000, "▂"},
		{299_999, "▂"},
		// Boundaries are exclusive: a value exactly on one belongs to the
		// step above it.
		{300_000, "▃"},
		{1_000_000, "▄"},
		{3_000_000, "▅"},
		{10_000_000, "▆"},
		{30_000_000, "▇"},
		{100_000_000, "█"},
		{354_747_634, "█"}, // largest real session
	}
	for _, c := range cases {
		if got := tokenGlyph(c.billed); got != c.want {
			t.Errorf("tokenGlyph(%d) = %q, want %q", c.billed, got, c.want)
		}
	}
	// Monotonic: more tokens never produces a shorter bar.
	prev := ""
	for _, n := range []int{1, 150_000, 500_000, 2_000_000, 5_000_000, 20_000_000, 50_000_000, 200_000_000} {
		g := tokenGlyph(n)
		if prev != "" && g < prev {
			t.Errorf("ramp not monotonic at %d: %q after %q", n, g, prev)
		}
		prev = g
	}
}

func TestUncachedShare(t *testing.T) {
	// Cache reads are the cheap bulk; input and cache writes are not.
	wellCached := session.Tokens{Input: 10, CacheWrite: 200, CacheRead: 9_000, Output: 790}
	if got := uncachedShare(wellCached); got != 2 {
		t.Errorf("uncachedShare = %d%%, want 2%%", got)
	}
	// A tiny session never amortises its first cache write, so it reads
	// as mostly-uncached even though it is cheap in absolute terms.
	tiny := session.Tokens{Input: 0, CacheWrite: 500, CacheRead: 500, Output: 0}
	if got := uncachedShare(tiny); got != 50 {
		t.Errorf("uncachedShare = %d%%, want 50%%", got)
	}
	if got := uncachedShare(session.Tokens{}); got != 0 {
		t.Errorf("empty uncachedShare = %d, want 0 (no divide by zero)", got)
	}
}

// A session with no recorded usage must occupy the column without
// drawing a bar, so rows stay aligned.
func TestTokenCellBlankWithoutUsage(t *testing.T) {
	if got := tokenCell(session.Session{}); got != "  " {
		t.Errorf("tokenCell(empty) = %q, want two spaces", got)
	}
	if got := tokenCell(session.Session{Tokens: session.Tokens{Output: 1}}); got == "  " {
		t.Error("a session with usage should draw a bar")
	}
}
