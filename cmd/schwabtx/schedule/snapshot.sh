#!/bin/sh
# snapshot.sh runs `schwabtx snapshot` for a scheduler: launchd on macOS, with
# the plist next to this file, or cron. README.md next to this file says how
# to set it up.
#
#   snapshot.sh [ENV_FILE]
#
# It loads ENV_FILE, by default the .env at the root of this repository, and
# runs $SCHWABTX, which the .env can set, else schwabtx from PATH, else
# ~/go/bin/schwabtx. A run that fails is tried twice more, a minute apart,
# since a run that launchd starts as the Mac wakes can come before the network
# is back.
#
# Each line of output gets the time in front. The summary, and any failed
# try that is tried again, go to stdout. Warnings and errors from the last
# try go to stderr, which cron mails to you if the machine can send mail.
# launchd mails nothing, so on macOS they also show as a notification: the
# warning a day before the weekly Schwab login lapses, and the error once it
# has or when every try failed. The exit status is that of the last try.

set -u

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

env_file=${1:-$(dirname "$0")/../../../.env}
case $env_file in
*/*) ;;
*) env_file=./$env_file ;; # "." searches PATH for a bare file name
esac
[ -r "$env_file" ] || fail "cannot read $env_file"
set -a
# shellcheck source=/dev/null
. "$env_file"
set +a

schwabtx=${SCHWABTX:-$(command -v schwabtx || printf '%s' "$HOME/go/bin/schwabtx")}
[ -x "$schwabtx" ] || fail "cannot run $schwabtx; install it with \`go install\` or set SCHWABTX in $env_file"

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

stamp <"$tmp/err" >&2
if [ -s "$tmp/err" ]; then
	notify "$(cat "$tmp/err")"
elif [ "$status" -ne 0 ]; then
	notify "error: schwabtx exited with status $status"
fi
exit "$status"
