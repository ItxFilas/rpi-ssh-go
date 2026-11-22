param(
    [Parameter(Mandatory=$true)][int]$ProcessId,
    [switch]$AssertOutboundOnly
)
$ErrorActionPreference = 'Stop'
$process = Get-Process -Id $ProcessId
# Enumerate before filtering: absence of sockets is valid; enumeration errors are not.
$udp = @(Get-NetUDPEndpoint | Where-Object OwningProcess -eq $ProcessId)
$tcp = @(Get-NetTCPConnection | Where-Object OwningProcess -eq $ProcessId)
$listeners = @($tcp | Where-Object State -eq Listen)
[pscustomobject]@{
    ProcessId = $ProcessId
    Executable = $process.Path
    UDPListeners = $udp.Count
    TCPListeners = $listeners.Count
    EstablishedTCP = @($tcp | Where-Object State -eq Established).Count
} | ConvertTo-Json -Compress
if ($AssertOutboundOnly -and ($udp.Count -gt 0 -or $listeners.Count -gt 0)) {
    throw 'Server has listening sockets; verify the binary and RPI_NETWORK_MODE.'
}
