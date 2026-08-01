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
	"$TMUX_BIN" new-session -d -s "$SES" -x 150 -y 40 \
		"CSB_CLAUDE_BIN=$STUB $CSB --root $FIX --no-auto-backup"
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
