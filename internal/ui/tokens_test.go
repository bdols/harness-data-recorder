package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

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
	if got := tokenCell(session.Session{}); got != "   " {
		t.Errorf("tokenCell(empty) = %q, want three spaces", got)
	}
	if got := tokenCell(session.Session{Tokens: session.Tokens{Output: 1}}); strings.TrimSpace(got) == "" {
		t.Error("a session with usage should draw a bar")
	}
}

// The cache signal must survive on shape alone: red-green colour
// blindness erases a green-to-red ramp, so density does the work and
// colour only reinforces it.
func TestCacheShadeIsDistinctByShape(t *testing.T) {
	low, _ := cacheShade(0)
	mid, _ := cacheShade(5)
	high, _ := cacheShade(15)
	if low == mid || mid == high || low == high {
		t.Fatalf("shades must differ by glyph: %q %q %q", low, mid, high)
	}
	// Denser glyph for a larger uncached share, so the ordering reads
	// correctly without any colour at all.
	if low != "░" || mid != "▒" || high != "▓" {
		t.Errorf("got %q/%q/%q, want ░/▒/▓ in increasing density", low, mid, high)
	}
	// Boundaries are inclusive at the lower edge of each band.
	if g, _ := cacheShade(4); g != "░" {
		t.Errorf("4%% = %q, want ░", g)
	}
	if g, _ := cacheShade(14); g != "▒" {
		t.Errorf("14%% = %q, want ▒", g)
	}
	if g, _ := cacheShade(100); g != "▓" {
		t.Errorf("100%% = %q, want ▓", g)
	}
}

// None of the three colours may sit on the red-green axis.
func TestCacheShadeAvoidsRedGreen(t *testing.T) {
	banned := map[string]string{
		"42": "green", "46": "green", "40": "green", "2": "green",
		"196": "red", "203": "red", "160": "red", "1": "red",
	}
	for _, share := range []int{0, 5, 15} {
		_, style := cacheShade(share)
		c := fmt.Sprintf("%v", style.GetForeground())
		if name, bad := banned[c]; bad {
			t.Errorf("share %d%% uses %s (colour %s); the palette must avoid the red-green axis", share, name, c)
		}
	}
}

func TestPctGuardsZero(t *testing.T) {
	if got := pct(5, 0); got != 0 {
		t.Errorf("pct(5,0) = %d, want 0", got)
	}
	if got := pct(1, 4); got != 25 {
		t.Errorf("pct(1,4) = %d, want 25", got)
	}
}

func TestTurnBarRamp(t *testing.T) {
	// upTo is exclusive, so a boundary value belongs to the step above.
	cases := []struct {
		context int
		want    string
	}{
		{0, "▁"}, {9_999, "▁"},
		{10_000, "▂"}, {24_999, "▂"},
		{25_000, "▃"},
		{50_000, "▄"},
		{100_000, "▅"},
		{150_000, "▆"},
		{200_000, "▇"},
		{400_000, "█"}, {2_000_000, "█"},
	}
	for _, c := range cases {
		if got := turnBar(c.context); got != c.want {
			t.Errorf("turnBar(%d) = %q, want %q", c.context, got, c.want)
		}
	}
}

func TestTurnTokenGutterBlankWithoutUsage(t *testing.T) {
	if g := turnTokenGutter(session.Entry{Role: "tool", Text: "ls"}); g != "" {
		t.Errorf("gutter for an unbilled entry = %q, want empty", g)
	}
}

func TestTurnTokenGutterMarksRebuild(t *testing.T) {
	e := session.Entry{Context: 40_000, Tokens: session.Tokens{Output: 120, CacheWrite: 30_000}}
	g := turnTokenGutter(e)
	if !strings.Contains(g, "⟳") {
		t.Errorf("a turn that rebuilt its cache should be marked: %q", g)
	}
	if !strings.Contains(g, "40.0k") || !strings.Contains(g, "↓120") {
		t.Errorf("gutter should carry context and output: %q", g)
	}
}

// The gutter has to sit flush against the right edge whether or not it
// shares a line with the text, and the padding has to be computed from
// the rendered width so ANSI escapes don't push it off the edge.
func TestHangRightAlignsToWidth(t *testing.T) {
	gutter := turnTokenGutter(session.Entry{Context: 20_000, Tokens: session.Tokens{Output: 357}})
	for _, block := range []string{
		styleTool.Render("  ⚙ Bash: short"),
		styleAsst.Render("a line long enough that the gutter cannot share it, by quite some margin indeed"),
	} {
		out := hangRight(block, gutter, 80)
		lines := strings.Split(out, "\n")
		last := lines[len(lines)-1]
		if w := lipgloss.Width(last); w != 80 {
			t.Errorf("last line width = %d, want 80: %q", w, last)
		}
		if !strings.Contains(last, "↓357") {
			t.Errorf("gutter missing from last line: %q", last)
		}
	}
}

func TestHangRightKeepsBlockWhenTooNarrow(t *testing.T) {
	gutter := turnTokenGutter(session.Entry{Context: 20_000, Tokens: session.Tokens{Output: 357}})
	block := styleAsst.Render("hello")
	if out := hangRight(block, gutter, 8); out != block {
		t.Errorf("a window too narrow for the gutter should render unchanged, got %q", out)
	}
	if out := hangRight(block, "", 80); out != block {
		t.Errorf("an empty gutter should render unchanged, got %q", out)
	}
}
