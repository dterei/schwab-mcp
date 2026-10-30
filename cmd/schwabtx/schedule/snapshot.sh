#!/bin/sh
# snapshot.sh runs `schwabtx snapshot` for a scheduler: launchd on macOS,
# through the agent that install-agent.sh sets up, or cron. README.md next to
# this file says how to set it up.
#
#   snapshot.sh [ENV_FILE]
#
# It loads ENV_FILE, by default the .env at the root of this repository, and
# runs schwabtx in that file's folder, so that relative paths in it work as
# they do from there. It runs $SCHWABTX, which the .env can set, else
# schwabtx from PATH, else ~/go/bin/schwabtx. A run that fails is tried twice
# more, a minute apart, since a run that launchd starts as the Mac wakes can
# come before the network is back.
#
# Each line of output gets the time in front. The summary, and the errors of
# a try that is tried again, go to stdout. Warnings and errors from the last
# try go to stderr, which cron mails to you if the machine can send mail.
# launchd mails nothing, so on macOS they also show as a notification: the
# warning in the two days before the weekly Schwab login lapses, and the error
# once it has or when every try failed. The exit status is that of the last
# try.

set -u
unset CDPATH

# notify shows its argument, on one line, as a macOS notification, and does
# nothing elsewhere. The message is passed as an argument, so quotes in it
# are safe.
notify() {
	[ "$(uname -s)" = Darwin ] || return 0
	osascript -e 'on run argv' \
		-e 'display notification (item 1 of argv) with title "schwabtx snapshot"' \
		-e 'end run' "$(printf '%s' "$1" | tr '\n' ' ')" >/dev/null 2>&1 || true
}

# stamp copies its input to its output with the time in front of each line.
stamp() {
	now=$(date '+%Y-%m-%d %H:%M:%S %Z')
	while IFS= read -r line || [ -n "$line" ]; do
		printf '%s %s\n' "$now" "$line"
	done
}

fail() {
	printf 'error: %s\n' "$1" | stamp >&2
	notify "error: $1"
	exit 1
}

# The default .env is the one of the repository this script is in, found
# through any link to the script and, with cd -P, to its folders.
script=$0
links=0
while [ -L "$script" ] && [ "$links" -lt 40 ]; do
	link=$(readlink "$script")
	case $link in
	/*) script=$link ;;
	*) script=$(dirname "$script")/$link ;;
	esac
	links=$((links + 1))
done
env_file=${1:-$(dirname "$script")/../../../.env}
cd -P "$(dirname "$env_file")" 2>/dev/null || fail "cannot read $env_file"
env_file=$(pwd -P)/$(basename "$env_file")
if [ -d "$env_file" ] || [ ! -r "$env_file" ]; then
	fail "cannot read $env_file"
fi
if grep -q "$(printf '\r')" "$env_file"; then
	fail "$env_file has Windows (CRLF) line endings; save it with Unix (LF) ones"
fi
# A mistake in the file, such as a value with a space and no quotes, must be
# reported, not end the script or be skipped without a word, so the file is
# first loaded in a subshell, where any complaint shows. Tracing (sh -x) is
# off meanwhile: it would show as a complaint, and show the secrets in it.
flags=$-
set +xv
if ! complaint=$( (set +u -a && command . "$env_file") 2>&1 >/dev/null) || [ -n "$complaint" ]; then
	fail "cannot load $env_file: $complaint"
fi
set +u -a
command . "$env_file" || fail "cannot load $env_file"
set -u +a
case $flags in *x*) set -x ;; esac
case $flags in *v*) set -v ;; esac

schwabtx=${SCHWABTX:-$(command -v schwabtx || printf '%s' "$HOME/go/bin/schwabtx")}
[ -x "$schwabtx" ] || fail "cannot run $schwabtx; install it with \`go install ./cmd/schwabtx\`, or set SCHWABTX in $env_file to where it is"

tmp=$(mktemp -d) || fail "cannot create a temporary directory"
trap 'rm -rf "$tmp"' EXIT
tries=3
try=1
while :; do
	"$schwabtx" snapshot >"$tmp/out" 2>"$tmp/err"
	status=$?
	stamp <"$tmp/out"
	if [ "$status" -eq 0 ] || [ "$try" -eq "$tries" ]; then
		break
	fi
	{
		cat "$tmp/err"
		echo "try $try of $tries failed; trying again in a minute"
	} | stamp
	try=$((try + 1))
	sleep 60
done

if [ "$status" -ne 0 ] && [ ! -s "$tmp/err" ]; then
	echo "error: schwabtx exited with status $status" >"$tmp/err"
fi
stamp <"$tmp/err" >&2
if [ -s "$tmp/err" ]; then
	notify "$(cat "$tmp/err")"
fi
exit "$status"
