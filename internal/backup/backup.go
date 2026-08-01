// Package backup keeps versioned, append-only copies of session .jsonl
// files outside ~/.claude, where Claude Code's cleanupPeriodDays pruning
// (30 days by default) cannot reach them — and, just as importantly,
// where a truncated or corrupted source can never destroy a good copy.
//
// Layout under dest, per session file <project>/<id>.jsonl:
//
//	<project>/<id>.jsonl            "current" file (a hard link to the
//	                                newest version) so the browser can
//	                                still read the backups via --root.
//	<project>/<id>.versions/        append-only history, one immutable
//	    20260801T120000-ab12cd.jsonl  file per distinct (mtime, content)
//	    20260801T153000-9f0e11.jsonl  state ever seen.
//
// Each Sync captures the source as a new version only when its content
// differs from what's already stored; versions are never overwritten or
// deleted. A truncation therefore lands as a smaller *new* version next
// to the good one, and Restore defaults to the most-complete version, so
// the corruption is inert.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultDir returns the backup destination, honoring XDG_DATA_HOME.
func DefaultDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "claude-session-browser", "backups")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "claude-session-browser", "backups")
}

// Sync snapshots every session file under root into dest. A session
// whose current content isn't already stored gets a new immutable
// version; unchanged sessions are skipped. Nothing is ever overwritten
// or removed. Returns the number of new versions written and the total
// number of session files seen.
func Sync(root, dest string) (copied, total int, err error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			total++
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			src := filepath.Join(root, d.Name(), f.Name())
			wrote, err := snapshot(src, dest, d.Name(), id)
			if err != nil {
				return copied, total, fmt.Errorf("backing up %s: %w", f.Name(), err)
			}
			if wrote {
				copied++
			}
		}
	}
	return copied, total, nil
}

// snapshot stores the source as a version (if new) and repoints the
// current flat file at the newest version. It reports whether a new
// version was written.
func snapshot(src, dest, project, id string) (bool, error) {
	vdir := filepath.Join(dest, project, id+".versions")
	flat := filepath.Join(dest, project, id+".jsonl")

	// Migration: a pre-versioning backup left a plain <id>.jsonl with no
	// history. Seed it as the first version so its content isn't lost the
	// moment the (possibly already-truncated) source is captured.
	if !isDir(vdir) {
		if fi, err := os.Stat(flat); err == nil && fi.Mode().IsRegular() {
			if _, err := storeVersion(flat, vdir); err != nil {
				return false, err
			}
		}
	}

	wrote, err := storeVersion(src, vdir)
	if err != nil {
		return false, err
	}
	if err := updateCurrent(flat, vdir); err != nil {
		return false, err
	}
	return wrote, nil
}

// storeVersion writes src into vdir under a name derived from its mtime
// and content hash, so identical states map to the same file and are
// captured at most once. It reports whether a new file was written.
func storeVersion(src, vdir string) (bool, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	sum, err := sha256File(src)
	if err != nil {
		return false, err
	}
	vpath := filepath.Join(vdir, versionName(fi.ModTime(), sum))
	if _, err := os.Stat(vpath); err == nil {
		return false, nil // this exact state is already stored
	}
	if err := copyPreserving(src, vpath); err != nil {
		return false, err
	}
	return true, nil
}

// versionName is <mtime>-<hash12>.jsonl. The leading timestamp makes the
// names sort chronologically; the hash disambiguates distinct content
// sharing an mtime and makes storage idempotent.
func versionName(mtime time.Time, sum string) string {
	return fmt.Sprintf("%s-%s.jsonl", mtime.UTC().Format("20060102T150405"), sum[:12])
}

// updateCurrent points flat at the newest version via a hard link (so
// browsing dest with --root sees the latest state at no extra space),
// falling back to a copy across filesystems.
func updateCurrent(flat, vdir string) error {
	newest := newestVersion(vdir)
	if newest == "" {
		return nil
	}
	// If flat already links to the newest version there's nothing to do —
	// and skipping avoids a rename() no-op that would otherwise leak the
	// temp link (POSIX rename does nothing when both paths are hard links
	// to the same inode).
	if fa, err := os.Stat(flat); err == nil {
		if fn, err := os.Stat(newest); err == nil && os.SameFile(fa, fn) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(flat), 0o755); err != nil {
		return err
	}
	tmp := flat + ".csblink"
	_ = os.Remove(tmp)
	if err := os.Link(newest, tmp); err != nil {
		return copyPreserving(newest, flat) // e.g. cross-device
	}
	if err := os.Rename(tmp, flat); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Restore copies the most complete backed-up version of the session with
// the given ID back into root so `claude --resume` finds it again.
// Defaulting to the largest version means a later truncation can't mask
// the good copy. It refuses to overwrite a live session file.
func Restore(dest, root, id string) (string, error) {
	dirs, err := os.ReadDir(dest)
	if err != nil {
		return "", err
	}
	name := id + ".jsonl"
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		vdir := filepath.Join(dest, d.Name(), id+".versions")
		best := bestVersion(vdir)
		if best == "" {
			// Legacy backup with no version history: fall back to the
			// plain file if it's there.
			flat := filepath.Join(dest, d.Name(), name)
			if _, err := os.Stat(flat); err != nil {
				continue
			}
			best = flat
		}
		dst := filepath.Join(root, d.Name(), name)
		if _, err := os.Stat(dst); err == nil {
			return "", fmt.Errorf("session %s already exists at %s", id, dst)
		}
		if err := copyPreserving(best, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("session %s not found under %s", id, dest)
}

// Version describes one stored version of a session.
type Version struct {
	Path string
	Size int64
	Time time.Time
}

// Versions returns every stored version of the session with the given
// ID, oldest first, across all projects in dest.
func Versions(dest, id string) []Version {
	dirs, err := os.ReadDir(dest)
	if err != nil {
		return nil
	}
	var out []Version
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		out = append(out, listVersions(filepath.Join(dest, d.Name(), id+".versions"))...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// listVersions returns the version files in vdir with their sizes and
// mtimes.
func listVersions(vdir string) []Version {
	entries, err := os.ReadDir(vdir)
	if err != nil {
		return nil
	}
	var out []Version
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Version{
			Path: filepath.Join(vdir, e.Name()),
			Size: fi.Size(),
			Time: fi.ModTime(),
		})
	}
	return out
}

// newestVersion returns the path of the most recently modified version
// in vdir, or "" if there are none. It compares actual mtimes (breaking
// ties toward the larger file) rather than the second-resolution
// filename, so two versions written in the same second still order
// correctly.
func newestVersion(vdir string) string {
	vs := listVersions(vdir)
	var best Version
	for _, v := range vs {
		if v.Time.After(best.Time) || (v.Time.Equal(best.Time) && v.Size > best.Size) {
			best = v
		}
	}
	return best.Path
}

// bestVersion returns the path of the most complete (largest) version in
// vdir, breaking ties toward the newest, or "" if there are none.
func bestVersion(vdir string) string {
	vs := listVersions(vdir)
	best := Version{}
	for _, v := range vs {
		if v.Size > best.Size || (v.Size == best.Size && v.Time.After(best.Time)) {
			best = v
		}
	}
	return best.Path
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyPreserving copies src to dst atomically (tmp file + rename) and
// carries over the source mtime so age displays stay meaningful.
func copyPreserving(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".csb-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), si.ModTime(), si.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
