package session

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SearchContent finds sessions whose raw .jsonl content contains
// query (case-insensitive, fixed string) and returns match counts
// keyed by file path. It uses ripgrep when available and falls back
// to a Go scan otherwise.
func SearchContent(root, query string) (map[string]int, error) {
	if query == "" {
		return nil, nil
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		return rgSearch(rg, root, query)
	}
	return goSearch(root, query)
}

func rgSearch(rg, root, query string) (map[string]int, error) {
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

func goSearch(root, query string) (map[string]int, error) {
	q := strings.ToLower(query)
	matches := map[string]int{}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
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
			path := filepath.Join(root, d.Name(), f.Name())
			if n := countInFile(path, q); n > 0 {
				matches[path] = n
			}
		}
	}
	return matches, nil
}

func countInFile(path, lowerQuery string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	for sc.Scan() {
		n += strings.Count(strings.ToLower(sc.Text()), lowerQuery)
	}
	return n
}
