#!/usr/bin/env bash
#
# End-to-end tests for the claude-session-browser TUI, driven through
# tmux. These complement the Go unit tests (which cover parsing, backup,
# and search logic in isolation) by exercising the actually-rendered
# interface: the split-pane transcript preview, the metadata filter,
# the inspect card, full-content search, and the resume hand-off to
# `claude`.
#
# The tests build the binary and run it against a throwaway fixture
# projects dir, so they never touch your real ~/.claude/projects and are
# deterministic. `claude` itself is stubbed via CSB_CLAUDE_BIN so the
# resume test can assert what would have been launched without starting
# a real session.
#
# Requires: tmux, go, and a git checkout. Run from anywhere:
#     test/tui_test.sh
#
# Exit status is non-zero if any assertion fails (CI-friendly).

set -u

TMUX_BIN=${TMUX_BIN:-$(command -v tmux || echo /usr/bin/tmux)}
if [ ! -x "$TMUX_BIN" ]; then
	echo "SKIP: tmux not found (set TMUX_BIN to override)"
	exit 0
fi

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
FIX="$WORK/projects"
CWD_ALPHA="$WORK/cwd-alpha"
CWD_BETA="$WORK/cwd-beta"
CSB="$WORK/csb"
STUB="$WORK/fake-claude.sh"
RESUME_LOG="$WORK/resume.log"
SES="csb-tui-test-$$"

FAILS=0
ok() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad() {
	printf '  \033[31mFAIL\033[0m %s\n' "$1"
	FAILS=$((FAILS + 1))
}

cleanup() {
	"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

# ---- fixtures ---------------------------------------------------------

mkfixtures() {
	mkdir -p "$FIX/proj-alpha" "$FIX/proj-beta" "$CWD_ALPHA" "$CWD_BETA"

	local ts_old ts_old2 ts_new ts_new2
	ts_old=$(date -u -d '-2 hours' +%Y-%m-%dT%H:%M:%SZ)
	ts_old2=$(date -u -d '-2 hours +1 min' +%Y-%m-%dT%H:%M:%SZ)
	ts_new=$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ)
	ts_new2=$(date -u -d '-1 hour +1 min' +%Y-%m-%dT%H:%M:%SZ)

	# Alpha is newer, so it sorts to the top and is selected by default.
	# Its content mentions "needle" three times (once in the user prompt,
	# twice in the reply) for the content-search assertion.
	cat >"$FIX/proj-alpha/sess-alpha.jsonl" <<EOF
{"type":"ai-title","aiTitle":"Alpha Session","sessionId":"sess-alpha"}
{"type":"user","message":{"role":"user","content":"find the needle please"},"timestamp":"$ts_new","cwd":"$CWD_ALPHA","gitBranch":"main","version":"9.9.9"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"searching for needle and Needle now"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100}},"timestamp":"$ts_new2"}
{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","name":"Bash","input":{"command":"echo audit-marker-xyz"}}]},"timestamp":"$ts_new2"}
EOF

	cat >"$FIX/proj-beta/sess-beta.jsonl" <<EOF
{"type":"ai-title","aiTitle":"Beta Session","sessionId":"sess-beta"}
{"type":"user","message":{"role":"user","content":"just a beta chat"},"timestamp":"$ts_old","cwd":"$CWD_BETA"}
{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"okay all done"}],"usage":{"input_tokens":3,"output_tokens":2}},"timestamp":"$ts_old2"}
EOF

	# Stubbed `claude`: record what a resume would have run, then exit so
	# the TUI resumes control.
	cat >"$STUB" <<EOF
#!/bin/sh
echo "args: \$*" > "$RESUME_LOG"
echo "cwd: \$PWD" >> "$RESUME_LOG"
EOF
	chmod +x "$STUB"
}

# ---- tmux helpers -----------------------------------------------------

pane() { "$TMUX_BIN" capture-pane -t "$SES" -p; }
send() { "$TMUX_BIN" send-keys -t "$SES" "$@"; }
type_text() { "$TMUX_BIN" send-keys -t "$SES" -l "$1"; }

# wait_for PATTERN [TIMEOUT_SECONDS]: poll the pane until PATTERN appears.
wait_for() {
	local pat=$1 end=$((SECONDS + ${2:-5}))
	while [ $SECONDS -lt $end ]; do
		pane | grep -qF "$pat" && return 0
		sleep 0.2
	done
	return 1
}

# wait_absent PATTERN [TIMEOUT_SECONDS]: poll until PATTERN is gone. Use
# this as the barrier when the interesting effect is something leaving
# the screen (e.g. a filter dropping a row) — waiting for what remains
# can pass before the re-render lands, since it was there all along.
wait_absent() {
	local pat=$1 end=$((SECONDS + ${2:-5}))
	while [ $SECONDS -lt $end ]; do
		pane | grep -qF "$pat" || return 0
		sleep 0.2
	done
	return 1
}

launch() {
	"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
	# Point --backup-dir and CSB_SESSIONS_DIR at empty dirs so the suite is
	# hermetic: otherwise restore-on-resume would consult the developer's
	# real backups and live detection would pick up their real sessions.
	mkdir -p "$WORK/empty-sessions"
	"$TMUX_BIN" new-session -d -s "$SES" -x 150 -y 40 \
		"CSB_SESSIONS_DIR=$WORK/empty-sessions CSB_CLAUDE_BIN=$STUB $CSB --root $FIX --backup-dir $WORK/empty-backups --no-auto-backup"
	wait_for "sessions" 8 || echo "  (warning: TUI status line never appeared)"
}

assert_present() {
	if pane | grep -qF "$2"; then ok "$1"; else bad "$1 — missing: $2"; fi
}
assert_absent() {
	if pane | grep -qF "$2"; then bad "$1 — unexpected: $2"; else ok "$1"; fi
}

# ---- build ------------------------------------------------------------

echo "building…"
(cd "$ROOT" && go build -o "$CSB" .) || {
	echo "build failed"
	exit 1
}
mkfixtures

# ---- scenarios --------------------------------------------------------

echo "list + split-pane preview"
launch
assert_present "list shows both session titles" "Alpha Session"
assert_present "list shows beta title" "Beta Session"
assert_present "list shows MODIFIED column" "MODIFIED"
assert_present "list shows CREATED column" "CREATED"
# Alpha is selected by default; its transcript renders in the preview pane.
assert_present "preview shows selected session transcript" "searching for needle"

echo "metadata filter (/)"
send "/"
type_text "beta"
wait_absent "Alpha Session" 3 # barrier: wait for the filter to drop alpha
assert_present "filter narrows to beta" "Beta Session"
assert_absent "filter hides alpha" "Alpha Session"
send Enter
send Escape # clear filter
wait_for "Alpha Session" 3
assert_present "esc restores full list" "Alpha Session"

echo "inspect card (i)"
send Escape # ensure list mode
send "i"
wait_for "session id" 3
assert_present "inspect shows session id" "sess-alpha"
assert_present "inspect shows message breakdown" "1 user"
assert_present "inspect shows cli version" "9.9.9"
send q # close card (any key)

echo "full-content search (s)"
launch # fresh, unfiltered
send "s"
type_text "needle"
send Enter
wait_for "matches" 5
assert_present "search reports match count" "3 matches"
assert_present "search keeps matching session" "Alpha Session"
assert_absent "search drops non-matching session" "Beta Session"
# The key bar must stay visible while a search is active — the search
# readout on its own line must not hide the remaining actions.
assert_present "key bar visible during search" "i inspect"
# …and those keys must still work: inspect opens on the matched session.
send "i"
wait_for "session id" 3
assert_present "inspect works after search" "sess-alpha"
send Escape

echo "version browser (V)"
BK="$WORK/backups"
rm -rf "$BK"
"$CSB" --root "$FIX" --backup-dir "$BK" --backup >/dev/null
"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
"$TMUX_BIN" new-session -d -s "$SES" -x 150 -y 40 \
	"CSB_CLAUDE_BIN=$STUB $CSB --root $FIX --backup-dir $BK --no-auto-backup"
wait_for "sessions" 8 || echo "  (warning: TUI never appeared)"
send V
wait_for "RECORDS" 5
assert_present "version browser opens" "RECORDS"
assert_present "version browser flags most complete" "most complete"
send v # view the version transcript
wait_for "searching for needle" 5
assert_present "version transcript viewer renders" "searching for needle"
send Escape # back to version list
send Escape # back to session list
wait_for "MODIFIED" 5
assert_present "escape returns to list" "MODIFIED"

echo "running-session guard (V + enter)"
# Fake a running claude process (cmdline must contain "claude") and a
# registry entry so sess-alpha is detected as live.
mkdir -p "$WORK/sessions"
cp "$(command -v sleep)" "$WORK/claude-keeper"
"$WORK/claude-keeper" 300 &
KEEPER=$!
printf '{"pid":%d,"sessionId":"sess-alpha","cwd":"%s","status":"busy","name":"a","updatedAt":1}\n' \
	"$KEEPER" "$FIX" >"$WORK/sessions/$KEEPER.json"
"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
"$TMUX_BIN" new-session -d -s "$SES" -x 150 -y 40 \
	"CSB_SESSIONS_DIR=$WORK/sessions CSB_CLAUDE_BIN=$STUB $CSB --root $FIX --backup-dir $BK --no-auto-backup"
wait_for "sessions" 8 || echo "  (warning: TUI never appeared)"
send V
wait_for "RECORDS" 5
assert_present "running session shows restore-disabled warning" "restoring a version is disabled"
send Enter # attempt a version restore
sleep 0.5
assert_absent "version restore blocked while running" "Restore this version"
send q
kill "$KEEPER" 2>/dev/null
rm -f "$WORK"/sessions/*.json

echo "tool-call audit (--commands and c)"
# CLI: the Bash tool call from the alpha fixture must appear.
if "$CSB" --root "$FIX" --backup-dir "$WORK/empty-backups" --commands 2>/dev/null | grep -q "echo audit-marker-xyz"; then
	ok "--commands lists the Bash tool call"
else
	bad "--commands missing the Bash tool call"
fi
if "$CSB" --root "$FIX" --backup-dir "$WORK/empty-backups" --commands audit-marker 2>/dev/null | grep -q "echo audit-marker-xyz"; then
	ok "--commands filter matches"
else
	bad "--commands filter found nothing"
fi
# Selectors must be structured, not substrings: an unknown value matches
# nothing, and the sandbox state is not reachable through command text.
if [ "$("$CSB" --root "$FIX" --backup-dir "$WORK/empty-backups" --commands sandbox:default 2>/dev/null | grep -c audit-marker-xyz)" = "1" ]; then
	ok "--commands sandbox:default selects the Bash call"
else
	bad "sandbox:default did not select the Bash call"
fi
if [ "$("$CSB" --root "$FIX" --backup-dir "$WORK/empty-backups" --commands sandbox:bypass 2>/dev/null | tail -n +2 | wc -l)" = "0" ]; then
	ok "--commands sandbox:bypass excludes non-bypassed calls"
else
	bad "sandbox:bypass matched a call that did not bypass"
fi
if [ "$("$CSB" --root "$FIX" --backup-dir "$WORK/empty-backups" --commands sandbox:typo 2>/dev/null | tail -n +2 | wc -l)" = "0" ]; then
	ok "an unrecognised selector value matches nothing"
else
	bad "a mistyped selector returned rows"
fi

# TUI: c opens the audit view showing the call.
launch
send c
wait_for "Tool-call audit" 5
assert_present "c opens the audit view" "Tool-call audit"
assert_present "audit view shows the Bash call" "audit-marker-xyz"

echo "markdown export (e)"
mkdir -p "$WORK/empty-sessions"
"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
# -c sets the working dir so the exported .md lands in the throwaway WORK.
"$TMUX_BIN" new-session -d -s "$SES" -c "$WORK" -x 150 -y 40 \
	"CSB_SESSIONS_DIR=$WORK/empty-sessions CSB_CLAUDE_BIN=$STUB $CSB --root $FIX --backup-dir $WORK/empty-backups --no-auto-backup"
wait_for "sessions" 8 || echo "  (warning: TUI never appeared)"
send e
wait_for "exported to" 5
assert_present "export reports the written file" "exported to"
if ls "$WORK"/*.md >/dev/null 2>&1; then ok "markdown file created"; else bad "no .md file created"; fi
if grep -q "## 👤 User" "$WORK"/*.md 2>/dev/null; then ok "markdown has turn headings"; else bad "markdown missing headings"; fi
rm -f "$WORK"/*.md

echo "running / rescue view (R)"
# A hermetic home with one saved transcript and a registry naming two
# running sessions: one whose file exists (saved) and one whose file is
# gone (reaped). Fake processes keep both "alive".
RHOME="$WORK/rhome"
mkdir -p "$RHOME/.claude/projects/-proj" "$WORK/rsessions"
printf '{"sessionId":"savedsess","type":"ai-title","aiTitle":"Saved One","cwd":"%s"}\n' "$RHOME" \
	>"$RHOME/.claude/projects/-proj/savedsess.jsonl"
cp "$(command -v sleep)" "$WORK/claude-k1"
cp "$(command -v sleep)" "$WORK/claude-k2"
"$WORK/claude-k1" 300 &
K1=$!
"$WORK/claude-k2" 300 &
K2=$!
printf '{"pid":%d,"sessionId":"savedsess","cwd":"%s","status":"idle","version":"9.9.9","updatedAt":2}\n' "$K1" "$RHOME" \
	>"$WORK/rsessions/$K1.json"
printf '{"pid":%d,"sessionId":"reapedsess","cwd":"%s","status":"idle","version":"9.9.9","updatedAt":1}\n' "$K2" "$RHOME" \
	>"$WORK/rsessions/$K2.json"
"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
"$TMUX_BIN" new-session -d -s "$SES" -x 150 -y 40 \
	"HOME=$RHOME CSB_SESSIONS_DIR=$WORK/rsessions CSB_CLAUDE_BIN=$STUB $CSB --root $RHOME/.claude/projects --backup-dir $BK --no-auto-backup"
wait_for "no saved transcript" 8 || echo "  (warning: reaped alert never appeared)"
assert_present "main list alerts about reaped running sessions" "no saved transcript"
# savedsess has a transcript and is running, so the main list shows its
# Claude Code version in the CC VER column.
assert_present "main list shows CC VER for a running session" "9.9.9"
send R
wait_for "TRANSCRIPT" 5
assert_present "running view flags the reaped session" "REAPED"
assert_present "running view lists the saved session" "saved"
assert_present "running view shows the CC VER column" "CC VER"
send q
kill "$K1" "$K2" 2>/dev/null
rm -rf "$RHOME" "$WORK/rsessions"

echo "re-home a moved project (--rehome)"
# A session recorded under an old path, whose project has since moved.
RH="$WORK/rh"
OLDP="$WORK/old-project"
NEWP="$WORK/new-project"
mkdir -p "$RH/-old-project" "$NEWP" "$RH/-old-project/memory"
printf '{"type":"ai-title","aiTitle":"Moved","sessionId":"movedsess"}\n' >"$RH/-old-project/movedsess.jsonl"
printf '{"type":"user","message":{"role":"user","content":"hi"},"cwd":"%s","timestamp":"2026-05-01T10:00:00Z"}\n' \
	"$OLDP" >>"$RH/-old-project/movedsess.jsonl"
echo "project note" >"$RH/-old-project/memory/note.md"
MUNGED=$(printf '%s' "$NEWP" | sed 's/[^A-Za-z0-9]/-/g')
if "$CSB" --root "$RH" --backup-dir "$WORK/empty-backups" --rehome movedsess --to "$NEWP" >"$WORK/rehome.out" 2>&1; then
	ok "--rehome exits cleanly"
else
	bad "--rehome failed"
	sed 's/^/    /' "$WORK/rehome.out"
fi
if [ -f "$RH/$MUNGED/movedsess.jsonl" ]; then
	ok "transcript landed in the new project dir"
else
	bad "no transcript at $RH/$MUNGED/movedsess.jsonl"
fi
if grep -qF "\"cwd\":\"$NEWP\"" "$RH/$MUNGED/movedsess.jsonl" 2>/dev/null &&
	! grep -qF "\"cwd\":\"$OLDP\"" "$RH/$MUNGED/movedsess.jsonl" 2>/dev/null; then
	ok "recorded cwd was rewritten to the new path"
else
	bad "cwd not rewritten"
fi
# Re-homing copies: the original must still be there.
if [ -f "$RH/-old-project/movedsess.jsonl" ]; then
	ok "original transcript left in place"
else
	bad "original transcript was removed"
fi
if [ -f "$RH/$MUNGED/memory/note.md" ]; then
	ok "project memory carried across"
else
	bad "project memory not copied"
fi
# A second run must refuse rather than clobber.
if "$CSB" --root "$RH" --backup-dir "$WORK/empty-backups" --rehome movedsess --to "$NEWP" >/dev/null 2>&1; then
	bad "--rehome overwrote an existing destination"
else
	ok "--rehome refuses to clobber without --force"
fi

echo "redact text in a session (--replace)"
RP="$WORK/rp"
mkdir -p "$RP/-proj"
printf '{"type":"ai-title","aiTitle":"Leaky","sessionId":"leaky"}\n' >"$RP/-proj/leaky.jsonl"
printf '{"type":"user","message":{"role":"user","content":"key sk-live-9999 here"},"cwd":"/proj","timestamp":"2026-05-01T10:00:00Z"}\n' \
	>>"$RP/-proj/leaky.jsonl"
"$CSB" --root "$RP" --backup-dir "$WORK/rpbk" --backup >/dev/null 2>&1
if "$CSB" --root "$RP" --backup-dir "$WORK/rpbk" --replace leaky --from "sk-live-9999" --to "[REDACTED]" --all-versions >"$WORK/rp.out" 2>&1; then
	ok "--replace exits cleanly"
else
	bad "--replace failed"
	sed 's/^/    /' "$WORK/rp.out"
fi
if grep -q "REDACTED" "$RP/-proj/leaky.jsonl" && ! grep -q "sk-live-9999" "$RP/-proj/leaky.jsonl"; then
	ok "secret scrubbed from the live transcript"
else
	bad "secret still in the live transcript"
fi
# --all-versions must reach into the backup history too, or the secret
# simply lives on there.
if grep -rq "sk-live-9999" "$WORK/rpbk" 2>/dev/null; then
	bad "secret survived in the backup versions"
else
	ok "secret scrubbed from backup versions too"
fi
# The transcript must still parse: every line valid JSON.
if "$CSB" --root "$RP" --backup-dir "$WORK/rpbk" --list 2>/dev/null | grep -q "leaky"; then
	ok "transcript still parses after redaction"
else
	bad "transcript broken after redaction"
fi

echo "re-home and redact prompts (m and x)"
launch
send m
wait_for "Re-home this session" 5
assert_present "m opens the re-home prompt" "Re-home this session"
assert_present "re-home prompt shows the destination preview" "new dir:"
send Escape
wait_for "re-home cancelled" 5
assert_present "esc cancels re-home" "re-home cancelled"
send x
wait_for "Find & replace" 5
assert_present "x opens the redact prompt" "Find & replace"
assert_present "redact prompt has both fields" "replace:"
send Escape

echo "tmux window naming (--name-windows)"
# A private tmux server whose pane IS the fake claude process (exec
# replaces the shell), so the pid in the registry is the pane pid and
# window naming has a real window to act on.
NS="csbname$$"
NHOME="$WORK/nhome"
mkdir -p "$NHOME" "$WORK/nsessions" "$WORK/proj-widgets"
cp "$(command -v sleep)" "$WORK/claude-win"
"$TMUX_BIN" -L "$NS" kill-server 2>/dev/null
"$TMUX_BIN" -L "$NS" new-session -d -s w -x 80 -y 24 "$WORK/claude-win 120"
sleep 0.5
NPID=$("$TMUX_BIN" -L "$NS" list-panes -a -F '#{pane_pid}' | head -1)
printf '{"pid":%d,"sessionId":"winsess","cwd":"%s","status":"idle","version":"9.9.9","updatedAt":1}\n' \
	"$NPID" "$WORK/proj-widgets" >"$WORK/nsessions/$NPID.json"
# Dry run must report the change without making it.
if CSB_TMUX_SOCKET="$NS" CSB_SESSIONS_DIR="$WORK/nsessions" HOME="$NHOME" \
	"$CSB" --name-windows --dry-run 2>/dev/null | grep -q "proj-widgets"; then
	ok "--name-windows --dry-run previews the rename"
else
	bad "--name-windows --dry-run showed nothing"
fi
if [ "$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)" != "proj-widgets" ]; then
	ok "dry run left the window name alone"
else
	bad "dry run renamed the window"
fi
CSB_TMUX_SOCKET="$NS" CSB_SESSIONS_DIR="$WORK/nsessions" HOME="$NHOME" \
	"$CSB" --name-windows >/dev/null 2>&1
if [ "$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)" = "proj-widgets" ]; then
	ok "--name-windows renamed the window after the project"
else
	bad "window not renamed (got '$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)')"
fi
# tmux must stop auto-renaming, or the name reverts on the next command.
if [ "$("$TMUX_BIN" -L "$NS" list-windows -F '#{?automatic-rename,on,off}' | head -1)" = "off" ]; then
	ok "automatic-rename disabled so the name sticks"
else
	bad "automatic-rename still on; name would revert"
fi
# Running it again is a no-op, not churn.
if CSB_TMUX_SOCKET="$NS" CSB_SESSIONS_DIR="$WORK/nsessions" HOME="$NHOME" \
	"$CSB" --name-windows 2>/dev/null | grep -q "nothing to rename"; then
	ok "re-running --name-windows is a no-op"
else
	bad "--name-windows renamed an already-correct window"
fi
# A name the user chose by hand must survive: tmux records that as
# automatic-rename off, and clobbering it would be a downgrade.
"$TMUX_BIN" -L "$NS" rename-window -t w:0 "my-own-name"
CSB_TMUX_SOCKET="$NS" CSB_SESSIONS_DIR="$WORK/nsessions" HOME="$NHOME" \
	"$CSB" --name-windows >/dev/null 2>&1
if [ "$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)" = "my-own-name" ]; then
	ok "hand-named window left alone"
else
	bad "hand-named window was clobbered"
fi
# ...unless explicitly forced.
CSB_TMUX_SOCKET="$NS" CSB_SESSIONS_DIR="$WORK/nsessions" HOME="$NHOME" \
	"$CSB" --name-windows --force >/dev/null 2>&1
if [ "$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)" = "proj-widgets" ]; then
	ok "--force renames a hand-named window"
else
	bad "--force did not rename the hand-named window"
fi

# csb must not mistake its own past renames for the user's. rename-window
# turns automatic-rename off, so without the @csb_named marker every
# window csb ever named would be permanently frozen.
MARK=$("$TMUX_BIN" -L "$NS" list-panes -a -F '#{@csb_named}' | head -1)
if [ "$MARK" = "proj-widgets" ]; then
	ok "rename records the @csb_named marker"
else
	bad "marker is '$MARK', want 'proj-widgets'"
fi
# Give the session a name; csb should update the window it named before,
# with no --force, even though automatic-rename is off.
printf '{"pid":%d,"sessionId":"winsess","cwd":"%s","status":"idle","name":"widgets","nameSource":"user","version":"9.9.9","updatedAt":2}\n' \
	"$NPID" "$WORK/proj-widgets" >"$WORK/nsessions/$NPID.json"
CSB_TMUX_SOCKET="$NS" CSB_SESSIONS_DIR="$WORK/nsessions" HOME="$NHOME" \
	"$CSB" --name-windows >/dev/null 2>&1
if [ "$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)" = "widgets" ]; then
	ok "csb updates a window it named itself, without --force"
else
	bad "csb refused to update its own window (got '$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)')"
fi
# But once the user renames it, csb must yield again.
"$TMUX_BIN" -L "$NS" rename-window -t w:0 "user-owns-this"
printf '{"pid":%d,"sessionId":"winsess","cwd":"%s","status":"idle","name":"changed-again","nameSource":"user","version":"9.9.9","updatedAt":3}\n' \
	"$NPID" "$WORK/proj-widgets" >"$WORK/nsessions/$NPID.json"
CSB_TMUX_SOCKET="$NS" CSB_SESSIONS_DIR="$WORK/nsessions" HOME="$NHOME" \
	"$CSB" --name-windows >/dev/null 2>&1
if [ "$("$TMUX_BIN" -L "$NS" list-windows -F '#{window_name}' | head -1)" = "user-owns-this" ]; then
	ok "csb yields after the user renames its window"
else
	bad "csb clobbered a window the user renamed after it"
fi
"$TMUX_BIN" -L "$NS" kill-server 2>/dev/null
rm -rf "$NHOME" "$WORK/nsessions"

echo "session-name window naming (--resume name)"
# A session named with --resume must name its window after that name, not
# the cwd basename — this is what separates several windows in one project.
NS2="csbname2$$"
mkdir -p "$WORK/n2sessions" "$WORK/proj-shared"
cp "$(command -v sleep)" "$WORK/claude-win2"
"$TMUX_BIN" -L "$NS2" kill-server 2>/dev/null
"$TMUX_BIN" -L "$NS2" new-session -d -s w -x 80 -y 24 "$WORK/claude-win2 120"
sleep 0.5
N2PID=$("$TMUX_BIN" -L "$NS2" list-panes -a -F '#{pane_pid}' | head -1)
printf '{"pid":%d,"sessionId":"named1","cwd":"%s","status":"idle","name":"ebooks","nameSource":"user","version":"9.9.9","updatedAt":1}\n' \
	"$N2PID" "$WORK/proj-shared" >"$WORK/n2sessions/$N2PID.json"
CSB_TMUX_SOCKET="$NS2" CSB_SESSIONS_DIR="$WORK/n2sessions" HOME="$NHOME" \
	"$CSB" --name-windows >/dev/null 2>&1
W2=$("$TMUX_BIN" -L "$NS2" list-windows -F '#{window_name}' | head -1)
if [ "$W2" = "ebooks" ]; then
	ok "window named after the session name, not the cwd"
else
	bad "window named '$W2', want 'ebooks' (cwd basename is proj-shared)"
fi

echo "live window-id column in the list"
# The list's live column shows the tmux window id for a running session.
LHOME="$WORK/lhome"
mkdir -p "$LHOME/.claude/projects/-proj"
printf '{"sessionId":"named1","type":"ai-title","aiTitle":"Named Session","cwd":"%s"}\n' "$WORK/proj-shared" \
	>"$LHOME/.claude/projects/-proj/named1.jsonl"
"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
"$TMUX_BIN" new-session -d -s "$SES" -x 150 -y 40 \
	"HOME=$LHOME CSB_TMUX_SOCKET=$NS2 CSB_SESSIONS_DIR=$WORK/n2sessions CSB_CLAUDE_BIN=$STUB $CSB --root $LHOME/.claude/projects --backup-dir $WORK/empty-backups --no-auto-backup"
wait_for "Named Session" 8 || echo "  (warning: list never appeared)"
assert_present "list shows the tmux window id for a live session" "w:0"
send q
# Negative control: with no live sessions the column must be blank, so the
# assertion above can't pass on some incidental "w:0" elsewhere on screen.
"$TMUX_BIN" kill-session -t "$SES" 2>/dev/null
"$TMUX_BIN" new-session -d -s "$SES" -x 150 -y 40 \
	"HOME=$LHOME CSB_TMUX_SOCKET=$NS2 CSB_SESSIONS_DIR=$WORK/empty-sessions CSB_CLAUDE_BIN=$STUB $CSB --root $LHOME/.claude/projects --backup-dir $WORK/empty-backups --no-auto-backup"
wait_for "Named Session" 8 || echo "  (warning: list never appeared)"
assert_absent "no window id when nothing is live" "w:0"
send q
"$TMUX_BIN" -L "$NS2" kill-server 2>/dev/null
rm -rf "$LHOME" "$WORK/n2sessions"

echo "resume hand-off (enter)"
launch # alpha selected at top
send Enter
# The stub writes the log then exits; the TUI returns and rescans.
if wait_for "returned from session" 5; then :; fi
if [ -f "$RESUME_LOG" ] && grep -q "sess-alpha" "$RESUME_LOG" &&
	grep -qF "$CWD_ALPHA" "$RESUME_LOG"; then
	ok "resume invoked claude --resume sess-alpha in its cwd"
else
	bad "resume did not launch claude with the right id/cwd"
	[ -f "$RESUME_LOG" ] && sed 's/^/    log: /' "$RESUME_LOG"
fi

# ---- summary ----------------------------------------------------------

echo
if [ "$FAILS" -eq 0 ]; then
	echo "all TUI tests passed"
	exit 0
fi
echo "$FAILS TUI assertion(s) failed"
exit 1
