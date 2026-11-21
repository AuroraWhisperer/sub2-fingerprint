param(
    [Parameter(Mandatory=$true, Position=0)]
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._@:-]*$')][string]$SshTarget,
    [ValidateRange(1,65535)][int]$SshPort
)
$arguments = @('-N', '-T', '-a', '-x', '-o', 'ExitOnForwardFailure=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ControlMaster=no', '-o', 'ControlPath=none', '-o', 'ServerAliveInterval=30', '-o', 'ServerAliveCountMax=2', '-L', '127.0.0.1:17861:127.0.0.1:17861')
if ($SshPort) { $arguments += @('-p', $SshPort.ToString()) }
$ssh = Get-Command ssh -CommandType Application -ErrorAction Stop | Select-Object -First 1
Write-Host 'Open http://127.0.0.1:17861/ after SSH connects. Ctrl+C closes the tunnel.'
if (-not (Get-Command Get-NetTCPConnection -ErrorAction SilentlyContinue)) {
    & $ssh.Source @arguments $SshTarget
    exit $LASTEXITCODE
}
if (Get-NetTCPConnection -LocalAddress 127.0.0.1 -LocalPort 17861 -State Listen -ErrorAction SilentlyContinue) {
    throw 'Local port 17861 is already in use. Reuse the existing tunnel if it belongs to you.'
}
# Keep ssh in the foreground for normal interactive authentication. The runspace only inspects local TCP state.
$watch = [PowerShell]::Create()
$result = [hashtable]::Synchronized(@{ IdleClose = $false })
$watchScript = {
    param($parentId, $started, $exePath, $outcome)
    $owned = $null
    while (-not $owned) {
        Start-Sleep -Seconds 2
        $listener = Get-NetTCPConnection -LocalAddress 127.0.0.1 -LocalPort 17861 -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
        if (-not $listener) { continue }
        $procInfo = Get-CimInstance Win32_Process -Filter ('ProcessId = ' + [int]$listener.OwningProcess) -ErrorAction SilentlyContinue
        if (-not $procInfo -or $procInfo.ParentProcessId -ne $parentId) { return }
        try {
            $child = [Diagnostics.Process]::GetProcessById($listener.OwningProcess)
            if ($child.MainModule.FileName -ne $exePath -or $child.StartTime.ToUniversalTime() -lt $started.AddSeconds(-1)) { return }
            $owned = $child
        } catch { return }
    }
    $unusedSince = [DateTime]::UtcNow
    $net = [Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties()
    try {
        while (-not $owned.HasExited) {
            Start-Sleep -Seconds 4
            try {
                $connected = $net.GetActiveTcpConnections() | Where-Object {
                    $_.LocalEndPoint.Port -eq 17861 -and $_.LocalEndPoint.Address.Equals([Net.IPAddress]::Loopback) -and
                    $_.State -eq [Net.NetworkInformation.TcpState]::Established
                } | Select-Object -First 1
            } catch { return }
            if ($connected) { $unusedSince = [DateTime]::UtcNow }
            if ([DateTime]::UtcNow.Subtract($unusedSince).TotalMinutes -ge 5 -and -not $owned.HasExited) {
                $outcome.IdleClose = $true
                try { $owned.Kill() } catch { $outcome.IdleClose = $false }
                return
            }
        }
    } finally { $owned.Dispose() }
}
$null = $watch.AddScript($watchScript.ToString()).AddArgument($PID).AddArgument([DateTime]::UtcNow).AddArgument($ssh.Source).AddArgument($result)
try {
    $null = $watch.BeginInvoke()
    & $ssh.Source @arguments $SshTarget
    $exit = $LASTEXITCODE
} finally {
    $watch.Stop()
    $watch.Dispose()
}
if ($result.IdleClose) { Write-Host 'Tunnel closed after five minutes without local connections.'; exit 0 }
exit $exit
