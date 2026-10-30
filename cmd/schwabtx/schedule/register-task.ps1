<#
.SYNOPSIS
Sets up the Task Scheduler task that runs snapshot.ps1 every day.

.DESCRIPTION
Registers, or replaces, the task -TaskName, which runs snapshot.ps1 (next to
this file) as you every day at -At, a 24-hour local time, while you are
signed in, and needs no admin rights. Run it again to change the time, or
after moving this repository. README.md next to this file says more.
#>
param(
    [string]$At = '21:05',
    [string]$TaskName = 'schwabtx snapshot'
)
$ErrorActionPreference = 'Stop'

$time = [datetime]::MinValue
if (-not [datetime]::TryParseExact($At, 'HH:mm', [Globalization.CultureInfo]::InvariantCulture, 'None', [ref]$time)) {
    throw "-At must be a time in 24-hour form, such as 21:05, not '$At'"
}
$script = Join-Path $PSScriptRoot 'snapshot.ps1'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$script`""
$trigger = New-ScheduledTaskTrigger -Daily -At $time
# New-ScheduledTaskTrigger writes the time with a UTC offset, which would move
# the run by an hour when daylight saving time starts or ends. Without one, it
# is local time.
$trigger.StartBoundary = (Get-Date).Date.Add($time.TimeOfDay).ToString('s')
$principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Minutes 30)
$null = Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force
"Registered the task '$TaskName': $script runs every day at $At while you are signed in."
