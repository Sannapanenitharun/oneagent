<#
.SYNOPSIS
Forwards a remote agent's dashboard port to this machine, and keeps it up.

.DESCRIPTION
The Windows counterpart to scripts/dev-tunnel.sh. It exists because the frontend
is developed on Windows while the agent only runs on Linux, and the obvious
`bash scripts/dev-tunnel.sh` does not work there: on a default Windows install
`bash` resolves to WSL's bash, which cannot see a Windows-style identity path.

The agent's dashboard binds loopback on purpose: it has no authentication and
serves that host's metrics, logs and trace contents. An SSH forward is how you
reach it from a browser without publishing any of that. Do NOT move
dashboard.listen_addr off 127.0.0.1 to avoid needing this.

Why the reconnect loop: a plain `ssh -N` forward has no keepalive, so an idle
connection gets dropped by the network or by sshd and simply stays dead. The
dashboard then shows "agent not reachable" until a human notices.

This file is deliberately ASCII-only. Windows PowerShell 5.1 decodes a .ps1
without a byte-order mark using the ANSI codepage, where the third byte of a
UTF-8 em dash lands on U+201D, a closing smart quote. Inside a double-quoted
string that terminates it early and the whole script fails to parse. Keep it
ASCII and the encoding stops being able to matter.

.EXAMPLE
scripts\dev-tunnel.ps1 ubuntu@203.0.113.10

.EXAMPLE
scripts\dev-tunnel.ps1 -Identity C:\keys\host.pem ubuntu@203.0.113.10

.EXAMPLE
scripts\dev-tunnel.ps1 -LocalPort 9000 ubuntu@203.0.113.10
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string] $Target,

    [string] $Identity,
    [int]    $LocalPort  = 8089,
    [int]    $RemotePort = 8088
)

$sshArgs = @(
    '-o', 'BatchMode=yes'
    '-o', 'ExitOnForwardFailure=yes'
    '-o', 'ServerAliveInterval=15'
    '-o', 'ServerAliveCountMax=3'
    '-o', 'TCPKeepAlive=yes'
    '-N', '-L', "${LocalPort}:127.0.0.1:${RemotePort}"
)
if ($Identity) { $sshArgs = @('-i', $Identity) + $sshArgs }

# Fail early and legibly rather than letting ssh exit 255 into the retry loop,
# where "port already in use" looks identical to an auth failure.
$inUse = Get-NetTCPConnection -LocalPort $LocalPort -State Listen -ErrorAction SilentlyContinue
if ($inUse) {
    # Not $pid: that is a read-only automatic variable and assigning to it throws.
    $owner = ($inUse | Select-Object -First 1).OwningProcess
    Write-Error "127.0.0.1:$LocalPort is already listening (PID $owner). Stop that process first, or pass -LocalPort."
    exit 1
}

Write-Host "forwarding 127.0.0.1:$LocalPort -> $Target 127.0.0.1:$RemotePort"
Write-Host "point the UI at it with: `$env:AGENT_I_URL = 'http://127.0.0.1:$LocalPort'; npm run dev"

$fails = 0
while ($true) {
    & ssh @sshArgs $Target
    $code = $LASTEXITCODE

    # 255 is ssh's own error: auth, DNS, a port already bound. Those do not fix
    # themselves, so back off hard rather than hammering the host. Three fast
    # retries covers a genuine transient; anything past that is a real fault.
    if ($code -eq 255) {
        $fails++
        if ($fails -ge 3) {
            Write-Warning "ssh failed $fails times (exit 255). Check the identity file, the host, and whether $LocalPort is already in use."
            Start-Sleep -Seconds 30
        } else {
            Start-Sleep -Seconds 5
        }
    } else {
        $fails = 0
        Write-Host "connection dropped (exit $code), reconnecting"
        Start-Sleep -Seconds 3
    }
}
