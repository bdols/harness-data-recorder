// Package backup mirrors session .jsonl files to a directory outside
// ~/.claude, where Claude Code's cleanupPeriodDays pruning (30 days
// by default) cannot reach them. The mirror keeps the projects-dir
// layout, so the browser can read it directly via --root, and files
// are never deleted from it.
package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// Sync copies every session file under root into dest that is new or
// changed (size or mtime differs), preserving mtimes. Nothing is ever
// removed from dest. Returns the number of files copied and the total
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
			src := filepath.Join(root, d.Name(), f.Name())
			dst := filepath.Join(dest, d.Name(), f.Name())
			changed, err := needsCopy(src, dst)
			if err != nil || !changed {
				continue
			}
			if err := copyPreserving(src, dst); err != nil {
				return copied, total, fmt.Errorf("backing up %s: %w", f.Name(), err)
			}
			copied++
		}
	}
	return copied, total, nil
}

func needsCopy(src, dst string) (bool, error) {
	si, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	di, err := os.Stat(dst)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return si.Size() != di.Size() || si.ModTime().After(di.ModTime()), nil
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

// Restore copies the backed-up session with the given ID back into
// root so `claude --resume` can find it again. It refuses to
// overwrite a live session file.
func Restore(dest, root, id string) (string, error) {
	name := id + ".jsonl"
	dirs, err := os.ReadDir(dest)
	if err != nil {
		return "", err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		src := filepath.Join(dest, d.Name(), name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(root, d.Name(), name)
		if _, err := os.Stat(dst); err == nil {
			return "", fmt.Errorf("session %s already exists at %s", id, dst)
		}
		if err := copyPreserving(src, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("session %s not found under %s", id, dest)
}
