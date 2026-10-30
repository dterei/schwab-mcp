<#
.SYNOPSIS
Runs `schwabtx snapshot` for Windows Task Scheduler.

.DESCRIPTION
Loads -EnvFile, by default the .env at the root of this repository, and runs
$env:SCHWABTX, which the .env can set, else schwabtx.exe in your Go bin
folder. A run that fails is tried twice more, -RetryDelay seconds apart, since
a run that starts as the PC wakes can come before the network is back.

Every line of output is appended to -Log with the time in front. Task
Scheduler reports neither warnings nor errors, so those from the last try also
show as a notification: the warning a day before the weekly Schwab login
lapses, and the error once it has or when every try failed. The exit code is
that of the last try, which Task Scheduler shows as the Last Run Result.

README.md next to this file says how to schedule it.
#>
param(
    [string]$EnvFile = (Join-Path $PSScriptRoot '..\..\..\.env'),
    [string]$Log = (Join-Path $env:LOCALAPPDATA 'schwabtx\snapshot.log'),
    [int]$RetryDelay = 60
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
    $Text = $Text -replace '\s*\r?\n\s*', ' '
    if ($PSVersionTable.PSEdition -ne 'Desktop') {
        Write-Log 'no notification: notifications need Windows PowerShell (powershell.exe)'
        return
    }
    try {
        $null = [Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
        $null = [Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime]
        $xml = New-Object Windows.Data.Xml.Dom.XmlDocument
        $xml.LoadXml('<toast><visual><binding template="ToastGeneric"><text>schwabtx snapshot</text><text>' +
            [Security.SecurityElement]::Escape($Text) + '</text></binding></visual></toast>')
        $toast = New-Object Windows.UI.Notifications.ToastNotification $xml
        # The app ID of Windows PowerShell, which Windows lets show notifications.
        $app = '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe'
        [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($app).Show($toast)
    } catch {
        Write-Log "could not show a notification: $($_.Exception.Message)"
    }
}

# .NET resolves a relative path against the process's directory, not
# PowerShell's.
$Log = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Log)
$code = 1
try {
    $null = New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Log)
    if (-not (Test-Path -LiteralPath $EnvFile -PathType Leaf)) {
        throw "cannot read $EnvFile"
    }
    foreach ($line in Get-Content -LiteralPath $EnvFile) {
        if ($line -match '^\s*([A-Za-z_]\w*)=(.*)$') {
            [Environment]::SetEnvironmentVariable($Matches[1], $Matches[2])
        }
    }
    $exe = $env:SCHWABTX
    if (-not $exe) {
        $exe = Join-Path $env:USERPROFILE 'go\bin\schwabtx.exe'
    }
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) {
        throw "cannot run $exe; install it with ``go install`` or set SCHWABTX in $EnvFile"
    }

    # Read stdout and stderr as they are, whichever PowerShell runs this.
    $start = New-Object Diagnostics.ProcessStartInfo $exe, 'snapshot'
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

    Write-Log $problems
    if ($problems) {
        Show-Notification $problems
    } elseif ($code -ne 0) {
        Show-Notification "error: schwabtx exited with code $code"
    }
} catch {
    Write-Log "error: $($_.Exception.Message)"
    Show-Notification "error: $($_.Exception.Message)"
    $code = 1
}
exit $code
