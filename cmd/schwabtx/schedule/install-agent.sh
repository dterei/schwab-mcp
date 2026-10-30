#!/bin/sh
# install-agent.sh installs, for you, the launchd agent that runs snapshot.sh
# every day at 9:05 p.m., or at the time given as HH:MM in 24-hour form. Run
# it again to change the time, or after moving this repository: it replaces
# the agent. README.md next to this file says more.
#
#   install-agent.sh [HH:MM]

set -eu
unset CDPATH

fail() {
	printf 'install-agent.sh: %s\n' "$1" >&2
	exit 1
}

# lower prints its argument, with a slash after it, in lower case.
lower() {
	printf '%s/\n' "$1" | tr '[:upper:]' '[:lower:]'
}

label=com.github.dterei.schwabtx.snapshot
here=$(cd -P "$(dirname "$0")" && pwd -P)
plist=$HOME/Library/LaunchAgents/$label.plist
log=$HOME/Library/Logs/schwabtx-snapshot.log
at=${1:-21:05}

[ "$(uname -s)" = Darwin ] || fail "launchd agents are for macOS; README.md has a cron line for other systems"
[ "$(id -u)" -ne 0 ] || fail "run this as yourself, without sudo: the agent runs as whoever installs it"
case $at in
[01][0-9]:[0-5][0-9] | 2[0-3]:[0-5][0-9]) ;;
*) fail "the time must be HH:MM in 24-hour form, such as 21:05" ;;
esac
# macOS does not let background jobs read these folders. The paths are
# compared with links followed and case ignored, as the file system does.
home=$(lower "$(cd -P "$HOME" && pwd -P)")
case $(lower "$here") in
"$home"desktop/* | "$home"documents/* | "$home"downloads/* | "$home""library/mobile documents"/* | "$home""library/cloudstorage"/*)
	fail "macOS does not let background jobs read $here; move this repository elsewhere, such as ~/schwab-mcp, and run this again"
	;;
/volumes/*)
	echo "install-agent.sh: if $here is on an external drive or a network volume, macOS will not let the agent read it" >&2
	;;
esac
hour=${at%:*}
minute=${at#*:}

# launchd does not expand ~ or $HOME and needs the log's folder to exist.
# plutil writes each path as a proper plist string, whatever is in it. On an
# array element, -replace inserts rather than replaces, so the script's path
# is removed and inserted.
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
cp "$here/$label.plist" "$plist"
plutil -remove ProgramArguments.1 "$plist"
plutil -insert ProgramArguments.1 -string "$here/snapshot.sh" "$plist"
plutil -replace StartCalendarInterval.Hour -integer "${hour#0}" "$plist"
plutil -replace StartCalendarInterval.Minute -integer "${minute#0}" "$plist"
plutil -replace StandardOutPath -string "$log" "$plist"
plutil -replace StandardErrorPath -string "$log" "$plist"
plutil -lint "$plist" >/dev/null
chmod 644 "$plist"

# Replace the agent if it is loaded, and undo a `launchctl disable`. Right
# after bootout, bootstrap can fail for a moment, so it gets a second try.
domain=gui/$(id -u)
launchctl enable "$domain/$label"
launchctl bootout "$domain/$label" 2>/dev/null || true
if ! launchctl bootstrap "$domain" "$plist" 2>/dev/null; then
	sleep 2
	launchctl bootstrap "$domain" "$plist" ||
		fail "launchctl could not load $plist; if the agent, sh, is switched off under System Settings > General > Login Items & Extensions > Allow in the Background, switch it on and run this again"
fi
echo "Installed $plist: snapshot.sh runs every day at $at, and logs to $log"
