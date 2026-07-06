// claude-session-browser: a TUI for browsing and resuming Claude Code
// sessions stored as .jsonl files under ~/.claude/projects.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bdols/claude-session-browser/internal/backup"
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
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("claude-session-browser", version)
		return
	}

	if *restore != "" {
		dst, err := backup.Restore(*backupDir, *root, *restore)
		if err != nil {
			fatal(err)
		}
		fmt.Println("restored", dst)
		return
	}

	if *runBackup {
		copied, total, err := backup.Sync(*root, *backupDir)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("backed up %d of %d sessions to %s\n", copied, total, *backupDir)
		return
	}

	if *list {
		if err := printList(*root); err != nil {
			fatal(err)
		}
		return
	}

	dir := *backupDir
	if *noAutoBackup {
		dir = ""
	}
	p := tea.NewProgram(ui.New(*root, dir), tea.WithAltScreen())
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

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
