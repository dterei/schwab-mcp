# Scheduled snapshots

`schwabtx snapshot` is meant to run every evening (see
[Every day](../README.md#every-day)). This folder has what a scheduler needs:

- [`snapshot.sh`](snapshot.sh) for launchd on macOS, with the agent
  [`com.github.dterei.schwabtx.snapshot.plist`](com.github.dterei.schwabtx.snapshot.plist),
  and for cron on Linux and other Unix systems
- [`snapshot.ps1`](snapshot.ps1) for Task Scheduler on Windows

Each script loads `.env` from the root of this repository, runs
`schwabtx snapshot` and puts the time in front of each line of output. A run
that fails is tried twice more, a minute apart, since a run that starts as
the computer wakes can come before the network is back. Warnings and errors
from the last try reach you as a notification on macOS and Windows, and
through cron's mail elsewhere: the warning a day before the weekly Schwab
login lapses, the error once it has, and any run that failed.

The scripts run the `schwabtx` that `go install` puts in your Go bin folder,
`~/go/bin` unless you set `GOBIN` or `GOPATH`. Install it from the root of
this repository, and again whenever you pull changes:

```sh
go install ./cmd/schwabtx
```

To run another binary, set `SCHWABTX` to its path in `.env`.

The examples run at 9:05 p.m. local time, which suits US time zones.
Elsewhere, pick the local time that falls in the US evening, a few hours
after the 4 p.m. Eastern close.

## macOS

The launchd agent runs `snapshot.sh` at 9:05 p.m., or when the Mac wakes if
it was asleep then, and appends its output to
`~/Library/Logs/schwabtx-snapshot.log`. Install it from the root of this
repository:

```sh
mkdir -p ~/Library/LaunchAgents
sed -e "s|/Users/YOU/schwab-mcp|$PWD|g" -e "s|/Users/YOU|$HOME|g" \
  cmd/schwabtx/schedule/com.github.dterei.schwabtx.snapshot.plist \
  > ~/Library/LaunchAgents/com.github.dterei.schwabtx.snapshot.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.dterei.schwabtx.snapshot.plist
```

macOS then tells you that a background item was added. It is listed as `sh`
under System Settings > General > Login Items & Extensions, and has to stay
allowed there. Keep this repository out of Documents, Desktop, Downloads and
iCloud Drive, which macOS does not let background jobs read.

Run it once now, and after a few seconds check the log:

```sh
launchctl kickstart gui/$(id -u)/com.github.dterei.schwabtx.snapshot
cat ~/Library/Logs/schwabtx-snapshot.log
```

The notifications come from Script Editor, and macOS may drop them until you
allow it. Check that this test notification shows:

```sh
osascript -e 'display notification "test" with title "schwabtx snapshot"'
```

If it does not, open Script Editor (in Applications > Utilities), run
`display notification "test"` there and allow notifications when asked, or
turn them on under System Settings > Notifications > Script Editor. Set
their style to Alerts (Persistent on newer macOS) so that one stays on
screen until you close it, and allow Script Editor in any Focus you use in
the evening. A notification you miss stays in Notification Center. Whether
or not one shows, the log has every warning and error.

To run it at another time, change `Hour` and `Minute` in the copy in
`~/Library/LaunchAgents`, then run the `bootout` command below and the
`bootstrap` command above. To remove it:

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
zone, so on a machine set to UTC, use `5 2 * * *`. It skips a run while the
machine is off or asleep.

The line appends the output to `~/schwabtx-snapshot.log`. Warnings and errors
from the last try also go to cron, which mails them to you only if the
machine can send mail and `MAILTO` names an address you read. Many machines,
stock Ubuntu among them, have no mail server, and cron then throws them away.

## Windows

From PowerShell at the root of this repository, without admin rights:

```powershell
$script = Join-Path $PWD 'cmd\schwabtx\schedule\snapshot.ps1'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$script`""
$trigger = New-ScheduledTaskTrigger -Daily -At '9:05 PM'
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Minutes 15)
Register-ScheduledTask -TaskName 'schwabtx snapshot' -Action $action -Trigger $trigger -Settings $settings
```

The task runs at 9:05 p.m. while you are signed in, also on battery, or as
soon as it can after a run missed while the PC was off or asleep. A console
window flashes as it starts; on Windows 11, where Windows Terminal is the
default terminal, a Terminal window may stay open while it runs. The script
appends its output to `%LOCALAPPDATA%\schwabtx\snapshot.log`, and warnings
and errors show as a notification from Windows PowerShell, so keep that
app's notifications on in Settings > System > Notifications. Do not disturb
holds a notification back, but it still lands in the notification center.
The task has to use `powershell.exe`: PowerShell 7 cannot show notifications.
If Group Policy sets an execution policy on your PC, it overrides
`-ExecutionPolicy Bypass`; `Get-ExecutionPolicy -List` shows it.

Run it once now, and after a few seconds check the log and the result, where
0 means it worked:

```powershell
Start-ScheduledTask 'schwabtx snapshot'
Get-Content "$env:LOCALAPPDATA\schwabtx\snapshot.log"
(Get-ScheduledTaskInfo 'schwabtx snapshot').LastTaskResult
```

To remove it: `Unregister-ScheduledTask 'schwabtx snapshot' -Confirm:$false`.
