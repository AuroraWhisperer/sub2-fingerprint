param(
    [Parameter(Mandatory=$true, Position=0)]
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._@:-]*$')][string]$SshTarget,
    [ValidateRange(1,65535)][int]$SshPort
)
$arguments = @('-N', '-T', '-a', '-x', '-o', 'ExitOnForwardFailure=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ControlMaster=no', '-o', 'ControlPath=none', '-o', 'ServerAliveInterval=30', '-o', 'ServerAliveCountMax=2', '-L', '127.0.0.1:17861:127.0.0.1:17861')
if ($SshPort) { $arguments += @('-p', $SshPort.ToString()) }
Write-Host 'Connecting. Open http://127.0.0.1:17861/ in your browser. Press Ctrl+C here to close this tunnel.'
& ssh @arguments $SshTarget
exit $LASTEXITCODE
