// Package rewrite edits session transcripts in place-safe ways: moving a
// session to a different project directory (re-homing), and literal text
// substitution across a transcript (redaction).
//
// Both operations work line-by-line on the .jsonl and re-verify that every
// rewritten line still parses as JSON before anything is written, so a bad
// substitution fails loudly instead of corrupting a transcript. Writes are
// atomic (temp file + rename) and preserve the 0600 mode Claude Code uses.
package rewrite

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MungeDir converts an absolute working directory into the directory name
// Claude Code uses under ~/.claude/projects. Every character outside
// [A-Za-z0-9] becomes a dash, so /var/mnt/videos/Clips becomes
// -var-mnt-videos-Clips.
//
// The mapping is lossy and NOT reversible: a path that already contains a
// dash collides with one containing a slash in the same spot. That is
// Claude Code's own behavior, not something this package can fix — it is
// why a session's true cwd must be read from the records, never inferred
// from the directory name.
func MungeDir(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// RehomeResult reports what a Rehome did.
type RehomeResult struct {
	Dest       string   // path of the transcript written
	ProjectDir string   // munged project directory name it landed in
	Records    int      // records in the transcript
	Changed    int      // records whose cwd was rewritten
	OldCWDs    []string // distinct cwd values that were replaced, in first-seen order
	Memory     string   // memory dir copied alongside, if any
}

// Rehome copies the transcript at src into the project directory for
// newCWD, rewriting every record's top-level cwd so Claude Code resumes
// the session in the right place and lists it under the new project.
//
// This exists because Claude Code locates resumable sessions solely by the
// munged name of the *current* directory. Move a project on disk and its
// sessions stay behind in the old directory — invisible from the new path,
// and (if the running process notices the pull and chdirs to /tmp) tagged
// with a cwd that no longer means anything.
//
// The copy is non-destructive: src is left untouched, and an existing
// destination is never overwritten unless force is set.
func Rehome(root, src, id, newCWD string, force bool) (RehomeResult, error) {
	var res RehomeResult
	if !filepath.IsAbs(newCWD) {
		return res, fmt.Errorf("new directory must be an absolute path, got %q", newCWD)
	}
	newCWD = filepath.Clean(newCWD)
	if fi, err := os.Stat(newCWD); err != nil {
		return res, fmt.Errorf("new directory %s: %w", newCWD, err)
	} else if !fi.IsDir() {
		return res, fmt.Errorf("new directory %s is not a directory", newCWD)
	}

	res.ProjectDir = MungeDir(newCWD)
	destDir := filepath.Join(root, res.ProjectDir)
	res.Dest = filepath.Join(destDir, id+".jsonl")
	if _, err := os.Stat(res.Dest); err == nil && !force {
		return res, fmt.Errorf("%s already exists (use force to overwrite)", res.Dest)
	}
	if sameFile(src, res.Dest) {
		return res, fmt.Errorf("session is already homed at %s", newCWD)
	}

	out, seen, changed, records, err := rewriteCWD(src, newCWD)
	if err != nil {
		return res, err
	}
	res.Records, res.Changed, res.OldCWDs = records, changed, seen

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return res, err
	}
	if err := writeAtomic(res.Dest, out); err != nil {
		return res, err
	}

	// Project memory lives beside the transcripts and is keyed to the
	// project dir, so it has to travel with the session or Claude starts
	// the resumed conversation having forgotten the project.
	srcMem := filepath.Join(filepath.Dir(src), "memory")
	dstMem := filepath.Join(destDir, "memory")
	if isDir(srcMem) && !isDir(dstMem) {
		if err := copyTree(srcMem, dstMem); err == nil {
			res.Memory = dstMem
		}
	}
	return res, nil
}

// rewriteCWD returns the transcript with every top-level cwd set to
// newCWD, along with the distinct old values, how many records changed,
// and the total record count.
func rewriteCWD(src, newCWD string) (out []byte, seen []string, changed, records int, err error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	defer f.Close()

	quoted, err := json.Marshal(newCWD)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	var buf bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	index := map[string]bool{}
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			buf.Write(line)
			buf.WriteByte('\n')
			continue
		}
		records++
		var obj map[string]json.RawMessage
		if json.Unmarshal(line, &obj) != nil {
			buf.Write(line) // not an object we understand; pass through
			buf.WriteByte('\n')
			continue
		}
		raw, ok := obj["cwd"]
		if !ok {
			buf.Write(line)
			buf.WriteByte('\n')
			continue
		}
		var cur string
		if json.Unmarshal(raw, &cur) != nil || cur == newCWD {
			buf.Write(line)
			buf.WriteByte('\n')
			continue
		}
		if !index[cur] {
			index[cur] = true
			seen = append(seen, cur)
		}
		rewritten, ok := replaceField(line, raw, quoted)
		if !ok {
			// Fall back to re-encoding the record. Key order changes, but
			// JSON objects are unordered and the content is preserved.
			obj["cwd"] = json.RawMessage(quoted)
			if rewritten, err = json.Marshal(obj); err != nil {
				return nil, nil, 0, 0, err
			}
		}
		if json.Unmarshal(rewritten, &map[string]json.RawMessage{}) != nil {
			return nil, nil, 0, 0, fmt.Errorf("rewrite produced invalid JSON on record %d", records)
		}
		changed++
		buf.Write(rewritten)
		buf.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, nil, 0, 0, err
	}
	return buf.Bytes(), seen, changed, records, nil
}

// replaceField swaps the "cwd" value in a record, touching only those
// bytes so the rest of the line survives byte-for-byte. It reports false
// if the literal couldn't be located (unusual spacing), leaving the caller
// to re-encode instead.
func replaceField(line, old, new []byte) ([]byte, bool) {
	key := []byte(`"cwd":`)
	for i := 0; ; {
		j := bytes.Index(line[i:], key)
		if j < 0 {
			return nil, false
		}
		at := i + j + len(key)
		v := at
		for v < len(line) && (line[v] == ' ' || line[v] == '\t') {
			v++
		}
		if bytes.HasPrefix(line[v:], old) {
			out := make([]byte, 0, len(line)-len(old)+len(new))
			out = append(out, line[:v]...)
			out = append(out, new...)
			out = append(out, line[v+len(old):]...)
			return out, true
		}
		i = at
	}
}

// ReplaceResult reports what a Replace did to one file.
type ReplaceResult struct {
	Path         string
	Replacements int
	Records      int
}

// Replace substitutes every literal occurrence of from with to across the
// transcript at path, rewriting the file in place (atomically).
//
// The substitution is literal, not a regexp, and every modified record is
// re-parsed as JSON before the file is written: if a replacement would
// break the transcript — an unescaped quote or backslash landing inside a
// string — the whole operation fails and the file is left alone. That
// makes it safe for redacting a secret that was pasted into a session.
//
// Note that redaction is only complete if the backup versions are treated
// too; a secret scrubbed from the live file still sits in the version
// history. The caller decides that scope.
func Replace(path, from, to string) (ReplaceResult, error) {
	res := ReplaceResult{Path: path}
	if from == "" {
		return res, fmt.Errorf("nothing to replace: the search text is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()

	var buf bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		n := bytes.Count(line, []byte(from))
		if n == 0 || len(bytes.TrimSpace(line)) == 0 {
			buf.Write(line)
			buf.WriteByte('\n')
			continue
		}
		res.Records++
		out := bytes.ReplaceAll(line, []byte(from), []byte(to))
		if json.Unmarshal(out, &map[string]json.RawMessage{}) != nil {
			return res, fmt.Errorf("replacement would corrupt the transcript "+
				"(record %d no longer parses as JSON); nothing was written", res.Records)
		}
		res.Replacements += n
		buf.Write(out)
		buf.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if res.Replacements == 0 {
		return res, nil
	}
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return res, err
	}
	return res, nil
}

// writeAtomic writes data to path via a temp file in the same directory,
// so a crash mid-write can never leave a half-written transcript.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".hdrtmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}
