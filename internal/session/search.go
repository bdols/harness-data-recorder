package session

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SearchContent finds sessions whose transcript text contains query
// (case-insensitive, fixed string) and returns match counts keyed by
// file path. It counts what the reader *shows* — prompts, replies and
// tool-call lines — so the list's count agrees with the reader's and
// n/N walks the same hits.
//
// This is the search most questions want. A raw grep over the .jsonl
// answers a different one: Claude Code stamps every typed prompt with
// `"origin":{"kind":"human"}` and `"turnOrigin":"human"`, so searching
// "human" hit 26 of 29 real sessions on a word no one ever wrote.
// Timestamps, cwds, permission modes, uuids and field names are all in
// there as well. SearchDeep is that grep, kept deliberate rather than
// accidental.
//
// Grep still does the first pass here, because it is quick and rules
// out most files; each surviving candidate is then parsed and its
// rendered text counted.
func SearchContent(root, query string) (map[string]int, error) {
	if query == "" {
		return nil, nil
	}
	cands, err := candidates(root, query)
	if err != nil {
		return nil, err
	}
	matches := map[string]int{}
	for _, path := range cands {
		if n := CountInTranscript(path, query); n > 0 {
			matches[path] = n
		}
	}
	return matches, nil
}

// SearchDeep finds sessions whose raw .jsonl contains query, counting
// everything in the file — thinking blocks, tool arguments and results,
// system reminders, the contents of files Claude read, and the record
// envelope's own metadata. It finds what SearchContent cannot, at the
// cost of matching plumbing no one wrote.
func SearchDeep(root, query string) (map[string]int, error) {
	if query == "" {
		return nil, nil
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		return rgCount(rg, root, query)
	}
	return goCount(root, query)
}

func rgCount(rg, root, query string) (map[string]int, error) {
	cmd := exec.Command(rg,
		"--count-matches", "--ignore-case", "--fixed-strings",
		"--no-messages", "--no-ignore", "--glob", "*.jsonl",
		"--", query, root)
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 means no matches; anything else is a real error.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return map[string]int{}, nil
		}
		return nil, err
	}
	matches := map[string]int{}
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		i := strings.LastIndexByte(ln, ':')
		if i <= 0 {
			continue
		}
		n, err := strconv.Atoi(ln[i+1:])
		if err != nil || n == 0 {
			continue
		}
		matches[ln[:i]] = n
	}
	return matches, nil
}

func goCount(root, query string) (map[string]int, error) {
	q := strings.ToLower(query)
	matches := map[string]int{}
	err := walkSessions(root, func(path string) {
		if n := countInFile(path, q); n > 0 {
			matches[path] = n
		}
	})
	return matches, err
}

func countInFile(path, lowerQuery string) int {
	n := 0
	eachLine(path, func(line string) bool {
		n += strings.Count(strings.ToLower(line), lowerQuery)
		return true
	})
	return n
}

// CountInTranscript counts occurrences of query in a session's rendered
// text — the prompts, replies and tool-call lines the reader draws, in
// the same order it draws them.
func CountInTranscript(path, query string) int {
	entries, err := LoadTranscript(path)
	if err != nil {
		return 0
	}
	q := strings.ToLower(query)
	n := 0
	for _, e := range entries {
		n += strings.Count(strings.ToLower(e.Text), q)
	}
	return n
}

// candidates lists the files that could possibly match, by raw content.
// The set is a superset of the real matches except where a needle only
// exists across JSON escaping (a quote, a backslash, a \n), which the
// reader could not show on one line anyway.
func candidates(root, query string) ([]string, error) {
	if rg, err := exec.LookPath("rg"); err == nil {
		return rgCandidates(rg, root, query)
	}
	return goCandidates(root, query)
}

func rgCandidates(rg, root, query string) ([]string, error) {
	cmd := exec.Command(rg,
		"--files-with-matches", "--ignore-case", "--fixed-strings",
		"--no-messages", "--no-ignore", "--glob", "*.jsonl",
		"--", query, root)
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 means no matches; anything else is a real error.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln != "" {
			paths = append(paths, ln)
		}
	}
	return paths, nil
}

func goCandidates(root, query string) ([]string, error) {
	q := strings.ToLower(query)
	var paths []string
	err := walkSessions(root, func(path string) {
		if containsRaw(path, q) {
			paths = append(paths, path)
		}
	})
	return paths, err
}

func containsRaw(path, lowerQuery string) bool {
	found := false
	eachLine(path, func(line string) bool {
		found = strings.Contains(strings.ToLower(line), lowerQuery)
		return !found
	})
	return found
}

func walkSessions(root string, fn func(path string)) error {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, d.Name()))
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			fn(filepath.Join(root, d.Name(), f.Name()))
		}
	}
	return nil
}

// eachLine calls fn per line until it returns false. Transcript lines
// run long, so the scanner gets the same oversized buffer the parser
// uses.
func eachLine(path string, fn func(line string) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	for sc.Scan() {
		if !fn(sc.Text()) {
			return
		}
	}
}
