# Scheduled snapshots

`schwabtx snapshot` is meant to run every evening (see
[Every day](../README.md#every-day)). This folder has what a scheduler needs:

- [`snapshot.sh`](snapshot.sh), which launchd on macOS or cron runs, and
  [`install-agent.sh`](install-agent.sh), which sets up the launchd agent
- [`snapshot.ps1`](snapshot.ps1), which Task Scheduler on Windows runs, and
  [`register-task.ps1`](register-task.ps1), which sets up the task

Each script loads `.env` from the root of this repository and runs
`schwabtx snapshot` in that folder, so relative paths in `.env` work as they
do there. It puts the time in front of each line of output. A run that fails
is tried twice more, a minute apart, since a run that starts as the computer
wakes can come before the network is back. Warnings and errors from the last
try reach you as a notification on macOS and Windows, and through cron's mail
elsewhere: the warning in the two days before the weekly Schwab login lapses,
the error once it has, and any run that failed. After you log in again, run
the snapshot once more, as under "Run it once now" below, so that the day is
not missed.

The scripts look for `schwabtx` in `~/go/bin` (`%USERPROFILE%\go\bin` on
Windows), where `go install` puts it unless you set `GOBIN` or `GOPATH`.
Install it from the root of this repository, and again whenever you pull
changes:

```sh
go install ./cmd/schwabtx
```

If it is somewhere else, set `SCHWABTX` in `.env` to its full path.

The examples run at 9:05 p.m. local time, which suits US time zones.
Elsewhere, pick the local time that falls in the US evening, a few hours
after the 4 p.m. Eastern close.

## macOS

Keep this repository, and any file that `.env` names, out of Documents,
Desktop, Downloads and iCloud Drive: macOS does not let background jobs read
them. Then, from the root of this repository:

```sh
sh cmd/schwabtx/schedule/install-agent.sh
```

It installs a launchd agent that runs `snapshot.sh` at 9:05 p.m. while you
are logged in, or when the Mac wakes if it was asleep then, and appends its
output to `~/Library/Logs/schwabtx-snapshot.log`. A run missed while the Mac
was shut down or you were logged out is skipped. For another time, give it
in 24-hour form: `sh cmd/schwabtx/schedule/install-agent.sh 22:30`. Run it
again after moving this repository, too: the agent runs the script where it
was, and fails without a word once it is gone.

macOS then tells you that background items were added. The agent shows up
under System Settings > General > Login Items & Extensions (Login Items
before macOS 15) > Allow in the Background, as `sh` from an unidentified
developer. Leave it on: switched off, it stops running without a word.

### Run it once now

Start the agent now, and after a few seconds check the log:

```sh
launchctl kickstart gui/$(id -u)/com.github.dterei.schwabtx.snapshot
cat ~/Library/Logs/schwabtx-snapshot.log
```

If the log stays empty, this shows the agent's last exit code, where 78
means that the folder for the log is missing:

```sh
launchctl print gui/$(id -u)/com.github.dterei.schwabtx.snapshot | grep 'last exit code'
```

If `launchctl` cannot find the agent, check that it is allowed in Login
Items.

### Notifications

The notifications come from Script Editor, and macOS may drop them until you
allow it. Check that this test notification shows:

```sh
osascript -e 'display notification "test" with title "schwabtx snapshot"'
```

If it does not, open Script Editor (in Applications > Utilities), run
`display notification "test"` there and allow notifications when asked, or
turn them on under System Settings > Notifications > Script Editor. There,
set the style to Alerts (Persistent on macOS 26) so that a notification
stays on screen until you close it, and leave Notification Center on so that
one you miss is still listed there. Allow Script Editor in any Focus you use
in the evening. Whether or not a notification shows, the log has every
warning and error.

### Remove it

```sh
launchctl bootout gui/$(id -u)/com.github.dterei.schwabtx.snapshot
rm ~/Library/LaunchAgents/com.github.dterei.schwabtx.snapshot.plist
```

## Linux and other Unix systems

From cron at 9:05 p.m.:

```
5 21 * * *  sh "$HOME/schwab-mcp/cmd/schwabtx/schedule/snapshot.sh" 2>&1 >> "$HOME/schwabtx-snapshot.log" | tee -a "$HOME/schwabtx-snapshot.log"
```

Adjust the path to where this repository is. Cron uses the machine's time
zone, so on a machine set to UTC, use `5 2 * * *`. It runs nothing while the
machine is off, and after a sleep of more than a few hours it skips the runs
it missed.

The line appends the output to `~/schwabtx-snapshot.log`. Warnings and errors
from the last try also go to cron, which mails them to you only if the
machine can send mail and `MAILTO` names an address you read. Many machines,
stock Ubuntu among them, have no mail server, and cron then throws them away.

Cron runs the line with little more than `HOME` and a short `PATH`. To run
the script once now the same way:

```sh
env -i HOME="$HOME" PATH=/usr/bin:/bin sh "$HOME/schwab-mcp/cmd/schwabtx/schedule/snapshot.sh"
```

## Windows

Keep this repository inside your user profile, like the token file: other
users of the PC may be able to change files elsewhere, and so the script that
the task runs as you. Then, from the root of this repository, in PowerShell
or a Command Prompt, without admin rights:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File cmd\schwabtx\schedule\register-task.ps1
```

It registers a task that runs `snapshot.ps1` at 9:05 p.m. while you are
signed in, also on battery; a locked screen is fine. For another time, add
`-At` with a time in 24-hour form, such as `-At 22:30`. Run it again after
moving this repository, too: it replaces the task. A run missed while the PC
was off starts about 10 minutes after it boots, and one missed while it slept
usually soon after it wakes. One missed while you were signed out is skipped.

The script appends its output to `%LOCALAPPDATA%\schwabtx\snapshot.log`, and
warnings and errors show as a notification from Windows PowerShell, so keep
that app's notifications on in Settings > System > Notifications. Do not
disturb holds a notification back, unless Windows PowerShell is among the
priority notifications there, but it still lands in the notification center.
The task has to run `powershell.exe`: PowerShell 7 cannot show
notifications. If Group Policy on your PC requires signed scripts or turns
scripts off, the task cannot run the script; `Get-ExecutionPolicy -List`
shows the policies.

A console window flashes as the task starts. On Windows 11, where Windows
Terminal is the default terminal, a Terminal window opens instead, often
minimized, and stays until the run ends; closing it stops the run. For just
the flash, set Windows Terminal's Settings > Startup > Default terminal
application to Windows Console Host.

### Run it once now

```powershell
Start-ScheduledTask 'schwabtx snapshot'
```

After a few seconds, check the log and the result, where 0 means it worked,
267009 that it is still running, 267011 that it has not run yet, and any
other number that it failed:

```powershell
Get-Content -Encoding UTF8 "$env:LOCALAPPDATA\schwabtx\snapshot.log"
(Get-ScheduledTaskInfo 'schwabtx snapshot').LastTaskResult
```

### Remove it

```powershell
Unregister-ScheduledTask 'schwabtx snapshot' -Confirm:$false
```
