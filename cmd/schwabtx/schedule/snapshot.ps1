<#
.SYNOPSIS
Runs `schwabtx snapshot` for Windows Task Scheduler.

.DESCRIPTION
Loads -EnvFile, by default the .env at the root of this repository, and runs
schwabtx in that file's folder, so that relative paths in it work as they do
from there. It runs $env:SCHWABTX, which the .env can set, else schwabtx.exe
from PATH, else the one in %USERPROFILE%\go\bin. A run that fails is tried
twice more, -RetryDelay seconds apart, since a run that starts as the PC
wakes can come before the network is back.

Every line of output is appended to -Log with the time in front. Task
Scheduler reports neither warnings nor errors, so those from the last try
also show as a notification, unless -NoNotification is given: the warning in
the two days before the weekly Schwab login lapses, and the error once it
has or when every try failed. The exit code is that of the last try, which
Task Scheduler shows as the Last Run Result.

register-task.ps1, next to this file, sets up the task that runs it.
#>
param(
    [string]$EnvFile = (Join-Path $PSScriptRoot '..\..\..\.env'),
    [string]$Log = (Join-Path $env:LOCALAPPDATA 'schwabtx\snapshot.log'),
    [int]$RetryDelay = 60,
    [switch]$NoNotification
)

# Write-Log appends each line of $Text to the log and prints it, with the time
# in front. The log is UTF-8 without a byte order mark, whichever PowerShell
# writes it.
function Write-Log([string]$Text) {
    $now = Get-Date -Format 'yyyy-MM-dd HH:mm:ss zzz'
    $lines = @($Text -split '\r?\n' | Where-Object { $_ -ne '' } | ForEach-Object { "$now $_" })
    if ($lines.Count -gt 0) {
        [IO.File]::AppendAllText($Log, ($lines -join "`r`n") + "`r`n", (New-Object Text.UTF8Encoding $false))
        $lines
    }
}

# Show-Notification shows $Text, on one line, as a Windows notification. Only
# Windows PowerShell (powershell.exe) can reach the Windows Runtime for that;
# PowerShell 7 notes in the log that it cannot.
function Show-Notification([string]$Text) {
    if ($NoNotification) {
        return
    }
    if ($PSVersionTable.PSEdition -ne 'Desktop') {
        Write-Log 'no notification: notifications need Windows PowerShell (powershell.exe)'
        return
    }
    $Text = $Text -replace '\s*\r?\n\s*', ' '
    try {
        $null = [Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
        $null = [Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime]
        $xml = New-Object Windows.Data.Xml.Dom.XmlDocument
        $xml.LoadXml('<toast><visual><binding template="ToastGeneric"><text>schwabtx snapshot</text><text>' +
            [Security.SecurityElement]::Escape($Text) + '</text></binding></visual></toast>')
        $toast = New-Object Windows.UI.Notifications.ToastNotification $xml
        # The app ID of Windows PowerShell, which Windows lets show notifications.
        $app = '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe'
        $notifier = [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($app)
        $notifier.Show($toast)
    } catch {
        Write-Log "could not show a notification: $($_.Exception.Message)"
        return
    }
    # Windows drops a notification it does not allow without an error. Until
    # an app has shown one, reading its setting can fail, so this comes after.
    try {
        if ($notifier.Setting -ne 'Enabled') {
            Write-Log "notifications are off for Windows PowerShell ($($notifier.Setting))"
        }
    } catch {
        Write-Log "could not check that notifications are on: $($_.Exception.Message)"
    }
}

# .NET resolves a relative path against the process's folder, not
# PowerShell's, so the paths are made absolute first.
$Log = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Log)
$EnvFile = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($EnvFile)
$code = 1
try {
    $null = New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Log)
    if (-not (Test-Path -LiteralPath $EnvFile -PathType Leaf)) {
        throw "cannot read $EnvFile"
    }
    # Windows PowerShell reads a file without a byte order mark as ANSI unless
    # told otherwise. As in sh, a value may be quoted, and a # after a space
    # starts a comment.
    foreach ($line in Get-Content -LiteralPath $EnvFile -Encoding UTF8) {
        if ($line -match '^\s*([A-Za-z_]\w*)=(.*)$') {
            $name, $raw = $Matches[1], $Matches[2]
            $value = $raw.Trim()
            if ($value -match '^(["''])(.*?)\1(\s+#.*)?$') {
                $value = $Matches[2]
            } else {
                $value = ($raw -replace '\s+#.*$', '').Trim()
            }
            [Environment]::SetEnvironmentVariable($name, $value)
        }
    }
    $dir = Split-Path -Parent $EnvFile
    Set-Location -LiteralPath $dir
    $exe = $env:SCHWABTX
    if (-not $exe) {
        $onPath = Get-Command schwabtx.exe -CommandType Application -ErrorAction Ignore | Select-Object -First 1
        $exe = if ($onPath) { $onPath.Path } else { Join-Path $env:USERPROFILE 'go\bin\schwabtx.exe' }
    }
    # A path PowerShell cannot make sense of, say with a stray quote, gets the
    # same message as one that does not exist.
    $found = $false
    try {
        $exe = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($exe)
        $found = Test-Path -LiteralPath $exe -PathType Leaf
    } catch {
    }
    if (-not $found) {
        throw "cannot run $exe; install it with ``go install ./cmd/schwabtx``, or set SCHWABTX in $EnvFile to where it is"
    }

    # Read stdout and stderr as they are, whichever PowerShell runs this.
    $start = New-Object Diagnostics.ProcessStartInfo $exe, 'snapshot'
    $start.WorkingDirectory = $dir
    $start.UseShellExecute = $false
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $start.StandardOutputEncoding = [Text.Encoding]::UTF8
    $start.StandardErrorEncoding = [Text.Encoding]::UTF8
    $tries = 3
    for ($try = 1; ; $try++) {
        $process = [Diagnostics.Process]::Start($start)
        $stderr = $process.StandardError.ReadToEndAsync()
        $stdout = $process.StandardOutput.ReadToEnd()
        $process.WaitForExit()
        $code = $process.ExitCode
        $problems = $stderr.Result.Trim()
        Write-Log $stdout
        if ($code -eq 0 -or $try -eq $tries) {
            break
        }
        Write-Log $problems
        Write-Log "try $try of $tries failed; trying again in $RetryDelay seconds"
        Start-Sleep -Seconds $RetryDelay
    }

    if ($code -ne 0 -and -not $problems) {
        $problems = "error: schwabtx exited with code $code"
    }
    if ($problems) {
        Write-Log $problems
        Show-Notification $problems
    }
} catch {
    Write-Log "error: $($_.Exception.Message)"
    Show-Notification "error: $($_.Exception.Message)"
    $code = 1
}
exit $code
