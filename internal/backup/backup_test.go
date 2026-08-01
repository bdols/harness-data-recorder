package backup

import (
	"os"
	"path/filepath"
	"strings"
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

// TestSyncLeavesNoTempFiles guards against the temp hard link that
// updateCurrent creates leaking when a sync leaves the current file
// pointing at the same version (POSIX rename is a no-op between hard
// links to one inode). Repeated syncs must leave only the current file
// and its versions.
func TestSyncLeavesNoTempFiles(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	mt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeSession(t, root, "-proj", "s.jsonl", "body", mt)

	for i := 0; i < 3; i++ {
		if _, _, err := Sync(root, dest); err != nil {
			t.Fatal(err)
		}
	}

	var stray []string
	_ = filepath.Walk(dest, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && !strings.HasSuffix(p, ".jsonl") {
			stray = append(stray, p)
		}
		return nil
	})
	if len(stray) != 0 {
		t.Errorf("stray non-.jsonl files left behind: %v", stray)
	}

	// The current flat file must be a hard link to the sole version, not
	// a second independent copy.
	fa, err := os.Stat(filepath.Join(dest, "-proj", "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	vs := Versions(dest, "s")
	if len(vs) != 1 {
		t.Fatalf("got %d versions, want 1", len(vs))
	}
	fv, err := os.Stat(vs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fa, fv) {
		t.Error("current file is not hard-linked to its version")
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

// TestTruncationKeepsRecoverableVersion is the regression test for the
// failure that motivated versioning: a good backup existed, the source
// was later truncated, a sync ran, and the truncation must NOT be able
// to destroy the good copy. Restore must return the complete version.
func TestTruncationKeepsRecoverableVersion(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	full := "line1\nline2\nline3\nline4\n"
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	path := writeSession(t, root, "-proj", "sid.jsonl", full, t0)

	// First sync captures the full transcript.
	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}

	// The source is truncated in place and its mtime advances, exactly as
	// a real truncation looks. A second sync captures the truncated state.
	if err := os.WriteFile(path, []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Hour)
	if err := os.Chtimes(path, t1, t1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}

	// Both states are retained as versions.
	if vs := Versions(dest, "sid"); len(vs) != 2 {
		t.Fatalf("got %d versions, want 2 (full + truncated)", len(vs))
	}

	// The live file is gone; Restore must bring back the FULL version,
	// not the truncated one that synced most recently.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(dest, root, "sid")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != full {
		t.Errorf("restored truncated data %q, want full transcript %q", got, full)
	}
}

// TestSyncMigratesLegacyFlatBackup guards the upgrade path: an existing
// pre-versioning backup (a plain <id>.jsonl with no history) must be
// seeded as a version before the current source is captured, so the
// migration itself can't lose the old copy — even if the live source is
// already truncated by the time the new code first runs.
func TestSyncMigratesLegacyFlatBackup(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	good := "line1\nline2\nline3\n"

	// A legacy flat backup sits in dest with no .versions dir.
	writeSession(t, dest, "-proj", "sid.jsonl", good, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	// The live source has already been truncated.
	writeSession(t, root, "-proj", "sid.jsonl", "line1\n", time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC))

	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}
	if vs := Versions(dest, "sid"); len(vs) != 2 {
		t.Fatalf("got %d versions after migration, want 2 (legacy + truncated)", len(vs))
	}

	// The pre-existing good backup must still be recoverable.
	if err := os.Remove(filepath.Join(root, "-proj", "sid.jsonl")); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(dest, root, "sid")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(restored)
	if string(got) != good {
		t.Errorf("migration lost the legacy backup: restored %q, want %q", got, good)
	}
}

// TestRestoreVersionPreservesCurrent checks that restoring an older
// version over a live file first captures the live file as a version, so
// going back is reversible and nothing is lost.
func TestRestoreVersionPreservesCurrent(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	path := writeSession(t, root, "-proj", "sid.jsonl", "old-v1\n", t0)
	if _, _, err := Sync(root, dest); err != nil { // version 1: old
		t.Fatal(err)
	}
	// The session grows; sync captures the newer state as version 2.
	if err := os.WriteFile(path, []byte("old-v1\nnew-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Hour)
	_ = os.Chtimes(path, t1, t1)
	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}

	vs := Versions(dest, "sid")
	if len(vs) != 2 {
		t.Fatalf("got %d versions, want 2", len(vs))
	}
	oldest := vs[0] // Versions is oldest-first

	// Restore the OLDER version over the (newer) live file.
	dst, err := RestoreVersion(dest, root, "-proj", "sid", oldest.Path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "old-v1\n" {
		t.Errorf("live is %q after restore, want the older version %q", got, "old-v1\n")
	}
	// The newer state must still be recoverable — it was captured before
	// the overwrite, so all versions remain (no net loss).
	if vs2 := Versions(dest, "sid"); len(vs2) != 2 {
		t.Errorf("got %d versions after version-restore, want 2 (nothing lost)", len(vs2))
	}
}

func TestMatchLive(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	mt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	live := writeSession(t, root, "-proj", "sid.jsonl", "content-A\n", mt)
	if _, _, err := Sync(root, dest); err != nil {
		t.Fatal(err)
	}
	vs := Versions(dest, "sid")
	if got := MatchLive(vs, live); got != vs[0].Path {
		t.Errorf("MatchLive = %q, want the stored version %q", got, vs[0].Path)
	}
	// A file with different content matches nothing.
	other := writeSession(t, root, "-proj", "other.jsonl", "content-B\n", mt)
	if got := MatchLive(vs, other); got != "" {
		t.Errorf("MatchLive of unrelated content = %q, want \"\"", got)
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
