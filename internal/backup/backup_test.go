package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSession creates root/<dir>/<name> with content and an explicit
// mtime, returning the file path.
func writeSession(t *testing.T, root, dir, name, content string, mtime time.Time) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncCopiesAllThenIdempotent(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	mtime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeSession(t, root, "-a", "s1.jsonl", "one", mtime)
	writeSession(t, root, "-b", "s2.jsonl", "two", mtime)

	copied, total, err := Sync(root, dest)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 2 || total != 2 {
		t.Fatalf("first sync copied=%d total=%d, want 2/2", copied, total)
	}

	// Nothing changed: a second sync copies nothing.
	copied, total, err = Sync(root, dest)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 0 || total != 2 {
		t.Fatalf("second sync copied=%d total=%d, want 0/2", copied, total)
	}
}

func TestSyncPreservesContentAndMtime(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	mtime := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	writeSession(t, root, "-proj", "s.jsonl", "hello world", mtime)

	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dest, "-proj", "s.jsonl")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Errorf("content = %q, want %q", got, "hello world")
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v, want preserved %v", fi.ModTime(), mtime)
	}
}

// A source that grows (new content, newer mtime) must be re-copied,
// but an unchanged one must not.
func TestSyncReCopiesChangedOnly(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	changing := writeSession(t, root, "-a", "s1.jsonl", "short", t0)
	writeSession(t, root, "-b", "s2.jsonl", "stable", t0)

	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}

	// Grow s1 and advance its mtime; leave s2 alone.
	if err := os.WriteFile(changing, []byte("short plus more"), 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Hour)
	if err := os.Chtimes(changing, t1, t1); err != nil {
		t.Fatal(err)
	}

	copied, _, err := Sync(root, dest)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 1 {
		t.Fatalf("copied=%d, want 1 (only the changed file)", copied)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "-a", "s1.jsonl"))
	if string(got) != "short plus more" {
		t.Errorf("changed file not updated: %q", got)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	mtime := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	writeSession(t, root, "-proj", "abc123.jsonl", "session body", mtime)
	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}

	// Simulate Claude Code's 30-day cleanup removing the live file.
	if err := os.Remove(filepath.Join(root, "-proj", "abc123.jsonl")); err != nil {
		t.Fatal(err)
	}

	restored, err := Restore(dest, root, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "session body" {
		t.Errorf("restored content = %q, want original", got)
	}
}

func TestRestoreRefusesOverwrite(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	mtime := time.Now()
	writeSession(t, root, "-proj", "abc123.jsonl", "live", mtime)
	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}
	// The live file still exists; restore must refuse.
	if _, err := Restore(dest, root, "abc123"); err == nil {
		t.Fatal("Restore overwrote a live session; want error")
	}
}

func TestRestoreMissing(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	if _, err := Restore(dest, root, "nope"); err == nil {
		t.Fatal("Restore of unknown id succeeded; want error")
	}
}
