// claude-session-browser: a TUI for browsing and resuming Claude Code
// sessions stored as .jsonl files under ~/.claude/projects.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bdols/claude-session-browser/internal/audit"
	"github.com/bdols/claude-session-browser/internal/backup"
	"github.com/bdols/claude-session-browser/internal/live"
	"github.com/bdols/claude-session-browser/internal/session"
	"github.com/bdols/claude-session-browser/internal/ui"
)

var version = "0.2.0"

func main() {
	root := flag.String("root", session.DefaultRoot(), "projects directory to scan (point at a mounted remote ~/.claude/projects, or at the backup dir, to browse those)")
	list := flag.Bool("list", false, "print sessions to stdout instead of launching the TUI")
	runBackup := flag.Bool("backup", false, "back up session files and exit")
	backupDir := flag.String("backup-dir", backup.DefaultDir(), "where session backups are kept")
	noAutoBackup := flag.Bool("no-auto-backup", false, "don't back up sessions automatically when the TUI starts")
	restore := flag.String("restore", "", "restore a backed-up session `id` into the projects dir and exit")
	running := flag.Bool("running", false, "list currently-running Claude sessions (flagging reaped ones) and exit")
	export := flag.String("export", "", "export session `id` as Markdown to stdout and exit (uses the most complete copy: live or backup)")
	commands := flag.Bool("commands", false, "audit every tool call across sessions (live + backups) and exit; optional filter args match tool/detail/project")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("claude-session-browser", version)
		return
	}

	if *running {
		printRunning()
		return
	}

	if *export != "" {
		if err := exportMarkdown(*root, *backupDir, *export); err != nil {
			fatal(err)
		}
		return
	}

	if *commands {
		printCommands(*root, *backupDir, flag.Args())
		return
	}

	if *restore != "" {
		versions := backup.Versions(*backupDir, *restore)
		dst, err := backup.Restore(*backupDir, *root, *restore)
		if err != nil {
			fatal(err)
		}
		fmt.Println("restored", dst)
		if len(versions) > 1 {
			fmt.Printf("(chose the most complete of %d stored versions)\n", len(versions))
		}
		return
	}

	if *runBackup {
		copied, total, err := backup.Sync(*root, *backupDir)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("saved %d new session version(s) across %d sessions to %s\n", copied, total, *backupDir)
		return
	}

	if *list {
		if err := printList(*root); err != nil {
			fatal(err)
		}
		return
	}

	p := tea.NewProgram(ui.New(*root, *backupDir, !*noAutoBackup), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func printList(root string) error {
	sessions, err := session.Scan(root)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LAST\tPROJECT\tTITLE\tMSGS\tMODEL\tID")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			s.LastTime.Local().Format("2006-01-02 15:04"),
			s.CWD, clip(s.Title, 48), s.UserMsgs+s.AsstMsgs, s.Model, s.ID)
	}
	return w.Flush()
}

// exportMarkdown writes the Markdown of the session with the given id to
// stdout, choosing the most complete transcript available — the live
// file or the largest backup version — so a reaped session still exports
// its full history from backups.
func exportMarkdown(root, backupDir, id string) error {
	path := transcriptPath(root, backupDir, id)
	if path == "" {
		return fmt.Errorf("session %s not found under %s or backups", id, root)
	}
	s, err := session.ParseFile(path)
	if err != nil {
		return err
	}
	s.ID = id // a version file's name isn't the session id
	md, err := session.Markdown(s)
	if err != nil {
		return err
	}
	fmt.Print(md)
	return nil
}

// transcriptPath returns the largest available transcript for id among
// the live projects dir, the backup versions, and the flat backup file.
func transcriptPath(root, backupDir, id string) string {
	var candidates []string
	if m, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl")); len(m) > 0 {
		candidates = append(candidates, m...)
	}
	for _, v := range backup.Versions(backupDir, id) {
		candidates = append(candidates, v.Path)
	}
	if m, _ := filepath.Glob(filepath.Join(backupDir, "*", id+".jsonl")); len(m) > 0 {
		candidates = append(candidates, m...)
	}
	best := ""
	var bestSize int64 = -1
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.Size() > bestSize {
			best, bestSize = c, fi.Size()
		}
	}
	return best
}

// printCommands prints the tool-call audit as a table, newest first,
// optionally filtered to rows matching all of the given terms (against
// tool, detail, or project).
func printCommands(root, backupDir string, terms []string) {
	entries := audit.Collect(root, backupDir)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tPROJECT\tTOOL\tDETAIL")
	for _, e := range entries {
		if !matchesAll(e, terms) {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			e.Time.Local().Format("2006-01-02 15:04"), e.Project, e.Tool, clip(e.Detail, 200))
	}
	w.Flush()
}

func matchesAll(e audit.Entry, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	hay := strings.ToLower(e.Tool + " " + e.Detail + " " + e.Project)
	for _, t := range terms {
		if !strings.Contains(hay, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

// printRunning lists running Claude sessions, reaped ones first, so a
// long-lived process whose transcript was cleaned up can be found and
// rescued (jump in, send a message to rewrite the file, then back it up).
func printRunning() {
	sessions := live.Scan()
	if len(sessions) == 0 {
		fmt.Println("no running Claude sessions detected")
		return
	}
	type row struct {
		live.Session
		reaped bool
	}
	rows := make([]row, 0, len(sessions))
	for id, ls := range sessions {
		matches, _ := filepath.Glob(filepath.Join(session.DefaultRoot(), "*", id+".jsonl"))
		rows = append(rows, row{Session: ls, reaped: len(matches) == 0})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].reaped != rows[j].reaped {
			return rows[i].reaped
		}
		return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
	})
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tPID\tCC VER\tTRANSCRIPT\tWHERE\tSESSION")
	for _, r := range rows {
		where := r.CWD
		if r.TmuxWindow != "" {
			where = fmt.Sprintf("tmux %s (%s)", r.TmuxWindow, r.TmuxLabel)
		}
		transcript := "saved"
		if r.reaped {
			transcript = "REAPED — rescue"
		}
		ver := r.Version
		if ver == "" {
			ver = "—"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n", r.Status, r.PID, ver, transcript, where, r.SessionID)
	}
	w.Flush()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
