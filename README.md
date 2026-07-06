# claude-session-browser

A TUI for browsing and resuming [Claude Code](https://claude.com/claude-code) sessions across all your projects.

Claude Code stores every session as a `.jsonl` file under `~/.claude/projects/<project>/<session-id>.jsonl`. Its built-in `--resume` picker only shows sessions for the current directory — this tool shows all of them, sorted by recency, with titles, message counts, and models, and resumes any of them in the right working directory.

## Install

```sh
go install github.com/bdols/claude-session-browser@latest
```

or build from source:

```sh
go build -o csb .
```

## Usage

```sh
csb                 # launch the TUI
csb --list          # plain-text session table to stdout
csb --root /path    # scan an alternate projects dir (see Remote below)
```

On terminals ≥95 columns wide the TUI shows a split view: the session list on the left and a live preview of the selected session's transcript tail on the right. Narrower terminals fall back to a full-width list.

### Keys

| Key | Action |
|-----|--------|
| `↑/↓` `j/k` | move selection |
| `enter` | resume selected session (`claude --resume <id>` in its original cwd; exiting Claude returns to the browser) |
| `v` `l` `→` | view transcript |
| `i` | inspect: session id, start/end dates, duration, message/tool/token counts, file size |
| `/` | filter (matches project, title, last prompt, branch, id) |
| `s` | full-content search across all sessions (uses `rg` when installed, Go fallback otherwise); shows per-session match counts, `esc` clears |
| `b` | back up sessions now |
| `d` | delete session file (with confirmation) |
| `r` | rescan |
| `q` | quit |

Set `CSB_CLAUDE_BIN` to override the `claude` binary used for resuming.

## Backups

Claude Code deletes session transcripts older than `cleanupPeriodDays` (default: **30 days**). The browser mirrors every session file to `~/.local/share/claude-session-browser/backups/` (or `$XDG_DATA_HOME/...`) automatically on every launch, so any session you've browsed at least once in 30 days survives the purge. Backups keep the projects-dir layout, preserve mtimes, are synced incrementally, and are never deleted by this tool.

```sh
csb --backup                    # one-shot sync (for cron/systemd timers)
csb --backup-dir /path          # alternate destination
csb --no-auto-backup            # don't sync on TUI launch
csb --root ~/.local/share/claude-session-browser/backups   # browse the backups themselves
csb --restore <session-id>      # copy a pruned session back so --resume works again
```

`--restore` refuses to overwrite a session that still exists live. You can also raise `cleanupPeriodDays` in `~/.claude/settings.json` to keep the originals longer.

## Remote sessions

There is no server component (yet). To browse sessions on a remote machine, mount or sync its projects dir and point `--root` at it:

```sh
sshfs server:.claude/projects /tmp/remote-sessions
csb --root /tmp/remote-sessions
```

Resuming a remote session locally only makes sense if the project cwd exists locally too; otherwise use the browser for reading transcripts.

### Roadmap

- atuin-style sync: a small server storing session summaries from many machines, with the TUI browsing all of them and `ssh <host> claude --resume <id>` to jump in
- token/cost stats per session

## Testing

```sh
go test ./...        # unit tests: parsing, backup sync/restore, search (rg + Go fallback)
test/tui_test.sh     # end-to-end TUI tests driven through tmux (requires tmux)
```

The Go tests build their own fixtures in temp dirs; `test/tui_test.sh` builds the binary, runs it against a throwaway fixture projects dir with `claude` stubbed via `CSB_CLAUDE_BIN`, and drives real keystrokes to assert on rendered output (split-pane preview, filter, inspect, content search, resume hand-off). Neither suite touches your real `~/.claude/projects`.

## How it works

Each `.jsonl` line is a typed record (`user`, `assistant`, `ai-title`, `last-prompt`, …). The scanner does a partial JSON decode of every line to extract the title, cwd, git branch, model, timestamps, and human/assistant message counts, skipping meta records, sidechains, and tool results. The transcript view re-parses the file into user prompts, assistant text, and one-line tool-call summaries.
