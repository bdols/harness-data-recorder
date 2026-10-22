// hdr (harness data recorder): a TUI for browsing, resuming and auditing
// Claude Code sessions stored as .jsonl files under ~/.claude/projects.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bdols/harness-data-recorder/internal/audit"
	"github.com/bdols/harness-data-recorder/internal/backup"
	"github.com/bdols/harness-data-recorder/internal/live"
	"github.com/bdols/harness-data-recorder/internal/rewrite"
	"github.com/bdols/harness-data-recorder/internal/session"
	"github.com/bdols/harness-data-recorder/internal/ui"
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
	rehome := flag.String("rehome", "", "re-home session `id` into another directory (use with --to); copies the transcript so Claude finds it at the new path")
	to := flag.String("to", "", "destination directory for --rehome, or the replacement text for --replace")
	adopt := flag.Bool("adopt-config", false, "with --rehome, also copy the old path's trust/tool/MCP settings in ~/.claude.json to the new path")
	force := flag.Bool("force", false, "with --rehome, overwrite an existing transcript at the destination; with --name-windows, also rename windows you named by hand")
	replace := flag.String("replace", "", "session `id` to run a literal text substitution over (use with --from and --to); for redacting secrets")
	from := flag.String("from", "", "text to search for with --replace")
	allVersions := flag.Bool("all-versions", false, "with --replace, also rewrite every stored backup version (needed for a secret to be truly gone)")
	tokens := flag.Bool("tokens", false, "report billed token usage per session (biggest first) and exit; optional filter args match project/title/id")
	byModel := flag.Bool("by-model", false, "with --tokens, break the totals down by issuing model instead of by session")
	nameWindows := flag.Bool("name-windows", false, "rename each tmux window holding a Claude session after the project running in it, and exit")
	dryRun := flag.Bool("dry-run", false, "with --name-windows, show what would be renamed without changing anything")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("hdr", version)
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

	if *tokens {
		printTokens(*root, *byModel, flag.Args())
		return
	}

	if *nameWindows {
		nameTmuxWindows(*root, *dryRun, *force)
		return
	}

	if *rehome != "" {
		if err := rehomeSession(*root, *backupDir, *rehome, *to, *adopt, *force); err != nil {
			fatal(err)
		}
		return
	}

	if *replace != "" {
		if err := replaceInSession(*root, *backupDir, *replace, *from, *to, *allVersions); err != nil {
			fatal(err)
		}
		return
	}

	if *restore != "" {
		versions := backup.Versions(*backupDir, *restore)
		before := backup.Records(transcriptPath(*root, "", *restore))
		dst, err := backup.Restore(*backupDir, *root, *restore, *force)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("restored %s (%d records)\n", dst, backup.Records(dst))
		if before > 0 {
			fmt.Printf("(replaced a live file of %d records, kept as a version)\n", before)
		}
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

// printTokens reports billed token usage per session, biggest first.
//
// The columns stay split rather than totalled because the split is the
// whole story: cache reads routinely account for the great majority of a
// session's token volume while costing a fraction of base input, so one
// combined number says little about either cost or context size. CONTEXT
// is the size of the last call — not a sum — since every call resends
// the conversation.
//
// Cost estimation is deliberately absent: it needs a per-model price
// table (base input, output, 5m and 1h cache writes, cache reads) that
// would silently go stale. ByModel already carries what such an estimate
// needs, for when it lands behind its own flag.
func printTokens(root string, byModel bool, terms []string) {
	sessions, err := session.Scan(root)
	if err != nil {
		fatal(err)
	}
	if byModel {
		printTokensByModel(sessions, terms)
		return
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Tokens.Billed() > sessions[j].Tokens.Billed()
	})
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "BILLED\tINPUT\tCACHE W\tCACHE R\tOUTPUT\tTHINKING\tCONTEXT\tPROJECT\t  TITLE")
	var total session.Tokens
	shown := 0
	for _, s := range sessions {
		if !matchesSession(s, terms) || s.Tokens.Billed() == 0 {
			continue
		}
		shown++
		total.Add(s.Tokens)
		t := s.Tokens
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			comma(t.Billed()), comma(t.Input), comma(t.CacheWrite), comma(t.CacheRead),
			comma(t.Output), comma(t.Thinking), comma(s.ContextTokens),
			projName(s.CWD), "  "+clip(s.Title, 40))
	}
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t\t%s\t\n",
		comma(total.Billed()), comma(total.Input), comma(total.CacheWrite), comma(total.CacheRead),
		comma(total.Output), comma(total.Thinking), fmt.Sprintf("%d sessions", shown))
	w.Flush()
	if b := total.Billed(); b > 0 {
		fmt.Printf("\ncache reads are %d%% of billed tokens\n", 100*total.CacheRead/b)
	}
}

// printTokensByModel aggregates the same totals per issuing model — the
// granularity a cost estimate needs, since one session commonly spans
// several models.
func printTokensByModel(sessions []session.Session, terms []string) {
	agg := map[string]session.Tokens{}
	for _, s := range sessions {
		if !matchesSession(s, terms) {
			continue
		}
		for model, t := range s.ByModel {
			cur := agg[model]
			cur.Add(t)
			agg[model] = cur
		}
	}
	models := make([]string, 0, len(agg))
	for m := range agg {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return agg[models[i]].Billed() > agg[models[j]].Billed() })
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "BILLED\tINPUT\tCACHE W\tCACHE 1H\tCACHE 5M\tCACHE R\tOUTPUT\tTHINKING\t  MODEL")
	for _, m := range models {
		t := agg[m]
		if t.Billed() == 0 {
			continue // e.g. <synthetic>, which carries no usage
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			comma(t.Billed()), comma(t.Input), comma(t.CacheWrite), comma(t.CacheWrite1h),
			comma(t.CacheWrite5m), comma(t.CacheRead), comma(t.Output), comma(t.Thinking), "  "+m)
	}
	w.Flush()
}

// matchesSession reports whether every term appears in the session's
// project, title or id.
func matchesSession(s session.Session, terms []string) bool {
	hay := strings.ToLower(s.CWD + " " + s.Title + " " + s.ID)
	for _, t := range terms {
		if !strings.Contains(hay, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

// comma formats n with thousands separators. Token counts are compared
// by magnitude, and "288,296,075" reads at a glance where "288296075"
// does not.
func comma(n int) string {
	if n == 0 {
		return "-"
	}
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// projName is the last path element of a cwd, for a compact column.
func projName(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	if cwd == "" {
		return "-"
	}
	return filepath.Base(cwd)
}

// nameTmuxWindows renames every tmux window holding a Claude session
// after the project running in it. With several long-lived sessions in
// tmux, the default names all read "claude" or "zsh", which says nothing
// about which one you want.
func nameTmuxWindows(root string, dryRun, force bool) {
	titles := map[string]string{}
	if sessions, err := session.Scan(root); err == nil {
		for _, s := range sessions {
			titles[s.ID] = s.Title
		}
	}
	plan := live.PlanRenames(live.Scan(), titles, force)
	if len(plan) == 0 {
		fmt.Println("nothing to rename: no Claude sessions in tmux, every window is already named after its project, " +
			"or the remaining windows were named by hand (--force renames those too)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "WINDOW\tFROM\tTO")
	failed := 0
	for _, r := range plan {
		status := ""
		if !dryRun {
			if err := live.RenameWindow(r.Pane, r.New); err != nil {
				status, failed = "\t(failed)", failed+1
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s%s\n", r.Window, r.Old, r.New, status)
	}
	w.Flush()
	switch {
	case dryRun:
		fmt.Printf("\n%d window(s) would be renamed (dry run)\n", len(plan))
	case failed > 0:
		fmt.Printf("\nrenamed %d window(s), %d failed\n", len(plan)-failed, failed)
	default:
		fmt.Printf("\nrenamed %d window(s)\n", len(plan))
	}
}

// rehomeSession copies a session's transcript into the project directory
// for a new path, rewriting the recorded cwd, so a project that moved on
// disk keeps its sessions resumable from the new location.
func rehomeSession(root, backupDir, id, dir string, adopt, force bool) error {
	if dir == "" {
		return fmt.Errorf("--rehome needs --to <directory>")
	}
	src := transcriptPath(root, backupDir, id)
	if src == "" {
		return fmt.Errorf("session %s not found under %s or backups", id, root)
	}
	// Snapshot before touching anything, so the pre-rehome state is a
	// restorable version even though the copy itself is non-destructive.
	if backupDir != "" {
		backup.Sync(root, backupDir)
	}
	res, err := rewrite.Rehome(root, src, id, dir, force)
	if err != nil {
		return err
	}
	fmt.Printf("re-homed %s → %s\n", id, res.Dest)
	fmt.Printf("  %d/%d records had their cwd rewritten to %s\n", res.Changed, res.Records, dir)
	if len(res.OldCWDs) > 0 {
		fmt.Printf("  replaced: %s\n", strings.Join(res.OldCWDs, ", "))
	}
	if res.Memory != "" {
		fmt.Printf("  copied project memory → %s\n", res.Memory)
	}
	if adopt {
		if len(res.OldCWDs) == 0 {
			fmt.Println("  no previous cwd to adopt config from")
		} else {
			added, err := rewrite.AdoptProjectConfig(res.OldCWDs[0], dir)
			switch {
			case err != nil:
				fmt.Fprintln(os.Stderr, "  config not adopted:", err)
			case len(added) == 0:
				fmt.Println("  config already present at the new path")
			default:
				fmt.Printf("  adopted config keys: %s\n", strings.Join(added, ", "))
			}
		}
	}
	fmt.Printf("resume it with: cd %s && claude --resume %s\n", dir, id)
	return nil
}

// replaceInSession runs a literal substitution across a session's
// transcript — for scrubbing a secret that was pasted into a chat, or
// fixing a path that changed. With allVersions it also rewrites the backup
// history, which is what a redaction actually requires.
func replaceInSession(root, backupDir, id, from, to string, allVersions bool) error {
	if from == "" {
		return fmt.Errorf("--replace needs --from <text> (and --to <replacement>)")
	}
	targets, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
	if allVersions && backupDir != "" {
		for _, v := range backup.Versions(backupDir, id) {
			targets = append(targets, v.Path)
		}
		flat, _ := filepath.Glob(filepath.Join(backupDir, "*", id+".jsonl"))
		targets = append(targets, flat...)
	}
	if len(targets) == 0 {
		return fmt.Errorf("session %s not found under %s", id, root)
	}
	total, files := 0, 0
	for _, t := range targets {
		res, err := rewrite.Replace(t, from, to)
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		if res.Replacements == 0 {
			continue
		}
		total += res.Replacements
		files++
		fmt.Printf("%s: %d replacement(s) in %d record(s)\n", t, res.Replacements, res.Records)
		// A rewritten version file's name still carries the old content
		// hash; reseal it so dedup keeps working.
		if strings.Contains(t, ".versions"+string(filepath.Separator)) {
			if _, err := backup.ResealVersion(t); err != nil {
				fmt.Fprintln(os.Stderr, "  warning: could not reseal version:", err)
			}
		}
	}
	if files == 0 {
		fmt.Printf("no occurrences of %q in %d file(s)\n", from, len(targets))
		return nil
	}
	// Version files were renamed; repoint the flat current file.
	if allVersions && backupDir != "" {
		for _, flat := range mustGlob(filepath.Join(backupDir, "*", id+".jsonl")) {
			if err := backup.RelinkCurrent(flat); err != nil {
				fmt.Fprintln(os.Stderr, "  warning: could not relink current:", err)
			}
		}
	}
	fmt.Printf("%d replacement(s) across %d file(s)\n", total, files)
	if !allVersions && backupDir != "" && len(backup.Versions(backupDir, id)) > 0 {
		fmt.Println("note: backup versions still contain the original text; " +
			"re-run with --all-versions to scrub those too")
	}
	return nil
}

func mustGlob(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	return m
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
// optionally filtered to rows matching all of the given terms (see
// audit.Entry.Matches).
func printCommands(root, backupDir string, terms []string) {
	entries := audit.Collect(root, backupDir)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tPROJECT\tMODEL\tTOOL\tSANDBOX\tDETAIL")
	for _, e := range entries {
		if !e.Matches(terms) {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Time.Local().Format("2006-01-02 15:04"), e.Project, e.ModelShort(), e.Tool,
			e.SandboxLabel(), clip(e.Detail, 200))
	}
	w.Flush()
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
