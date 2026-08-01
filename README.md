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

The list shows two time columns: **MODIFIED** (time since the last message — the sort key) and **CREATED** (time since the first message). Both are derived from record timestamps, not file mtime. The inspect card (`i`) shows the exact start/end timestamps and span.

### Keys

| Key | Action |
|-----|--------|
| `↑/↓` `j/k` | move selection |
| `enter` | resume selected session (`claude --resume <id>` in its original cwd; exiting Claude returns to the browser). If the session isn't in Claude's live dir but a backup exists (e.g. it was reaped, or you're browsing backups), a dialog offers to restore it first, then resume |
| `v` `l` `→` | view transcript |
| `i` | inspect: session id, start/end dates, duration, message/tool/token counts, running status, file size |
| `w` | switch the tmux window to the running session (if it's live and inside tmux) |
| `/` | filter (matches project, title, last prompt, branch, id) |
| `s` | full-content search across all sessions (uses `rg` when installed, Go fallback otherwise); shows per-session match counts, `esc` clears |
| `b` | back up sessions now |
| `d` | delete session file (with confirmation) |
| `r` | rescan |
| `q` | quit |

The key bar sits on its own line at the bottom, so a search or filter readout never hides it — `i`, `w`, and the other actions stay listed and usable while a content search or filter is active.

### Running sessions

A marker in the left gutter shows sessions that are **currently running**, encoded by both shape and color so it stays legible under red-green color blindness (the palette avoids the green/red axis):

- `●` blue — idle but alive
- `◆` orange — busy (working)
- `▲` magenta — waiting on your input

The status bar shows a per-status breakdown (e.g. `●11 ◆2 ▲2 live`), which also serves as a legend, and the inspect card spells the status out in words.

Detection reads Claude Code's per-process registry at `~/.claude/sessions/<pid>.json` (which records the resolved session id, cwd, and status), drops stale entries whose process has exited, and walks the process's parents through `/proc` to find its tmux pane. Pressing `w` on a live session runs `tmux switch-client`/`select-window` to bring that pane to the foreground — the browser keeps running in its own pane. This works when the browser itself is running inside tmux.

### Environment

| Variable | Effect |
|----------|--------|
| `CSB_CLAUDE_BIN` | `claude` binary used for resuming (default `claude`) |
| `CSB_TMUX_SOCKET` | tmux server socket (`tmux -L <name>`) for detection and switching; default server otherwise |

## Backups

Claude Code deletes session transcripts older than `cleanupPeriodDays` (default: **30 days**). The browser keeps **versioned, append-only** copies under `~/.local/share/claude-session-browser/backups/` (or `$XDG_DATA_HOME/...`) automatically on every launch, so any session you've browsed at least once in 30 days survives the purge.

Backups are **append-only**: each launch captures the session's current content as a new immutable version *only when it changed*, and versions are never overwritten or deleted. This is deliberate — a mirror that overwrites can propagate a truncated or corrupted source over a good backup and destroy it; a versioned store cannot. A truncation just lands as a smaller *new* version beside the good one.

Layout, per session file:

```
<project>/<id>.jsonl            # "current" file (hard link to the newest version) — browsable via --root
<project>/<id>.versions/        # append-only history, one file per distinct state
    20260801T120000-ab12cd.jsonl
    20260801T153000-9f0e11.jsonl
```

```sh
csb --backup                    # one-shot sync (for cron/systemd timers)
csb --backup-dir /path          # alternate destination
csb --no-auto-backup            # don't sync on TUI launch
csb --root ~/.local/share/claude-session-browser/backups   # browse the backups themselves
csb --restore <session-id>      # restore the most complete version so --resume works again
```

`--restore` brings back the **most complete (largest) version**, so a later truncation can never mask the good copy, and it refuses to overwrite a session that still exists live. The inspect card (`i`) shows how many versions are stored and flags (`⚠`) when an older version holds more data than the newest — the sign of a truncated live file. Existing pre-versioning (flat) backups are migrated in place on the next sync: the old copy is seeded as version 1 before anything else is captured. You can also raise `cleanupPeriodDays` in `~/.claude/settings.json` to keep the originals longer.

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

The Go tests build their own fixtures in temp dirs — including the running-session detection (fake registry via `CSB_SESSIONS_DIR`, injected process/pane data) and the tmux switch, which runs against a private throwaway tmux server via `CSB_TMUX_SOCKET` so it can never disturb your own session. `test/tui_test.sh` builds the binary, runs it against a throwaway fixture projects dir with `claude` stubbed via `CSB_CLAUDE_BIN`, and drives real keystrokes to assert on rendered output (split-pane preview, filter, inspect, content search, resume hand-off). Neither suite touches your real `~/.claude/projects` or tmux server.

## How it works

Each `.jsonl` line is a typed record (`user`, `assistant`, `ai-title`, `last-prompt`, …). The scanner does a partial JSON decode of every line to extract the title, cwd, git branch, model, timestamps, and human/assistant message counts, skipping meta records, sidechains, and tool results. The transcript view re-parses the file into user prompts, assistant text, and one-line tool-call summaries.

Running-session detection is separate: Claude Code maintains `~/.claude/sessions/<pid>.json` for each live process, giving an exact session id regardless of how it was launched. The browser reads that registry, validates each pid against `/proc` (dropping stale files and guarding pid reuse), and walks parent pids until it reaches a tmux pane to locate the window.
