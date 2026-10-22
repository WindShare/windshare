# #region DEBUG
param([int]$Rounds=1, [switch]$Packets, [int]$ReceiveBuffer=0, [int]$PauseAt=0, [int]$PauseMilliseconds=0, [string]$Label='natural', [switch]$RepairRack, [int]$DropAt=0, [switch]$Production)
$ErrorActionPreference = 'Stop'
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../../..'))
Set-Location -LiteralPath $repositoryRoot
[IO.Directory]::CreateDirectory((Join-Path $repositoryRoot '.agents')) | Out-Null
if ($Production -and $DropAt -gt 0) { throw 'Fault injection requires the diagnostic binary' }
$binaryName = if ($Production) { if ($RepairRack) {'production-after'} else {'production-before'} } else { if ($RepairRack) {'native-rack-repair'} else {'native'} }
$env:WS_RESEARCH_BINARY_DIRECTORY = (Resolve-Path (Join-Path '.tmp/webrtc-stall-20261022' $binaryName)).Path
$env:WS_STALL_DROP_AT = [string]$DropAt
$env:WS_RESEARCH_ISOLATE_BROWSER_DNS = '1'
$env:WS_RESEARCH_TRACE = 'on'
$env:WS_RESEARCH_OUTPUT = 'opfs'
$env:WS_RESEARCH_FIXTURE = (Resolve-Path '.tmp/webrtc-stall-20261022/fixture.bin').Path
$env:WS_STALL_PACKET_TRACE = if ($Packets) {'1'} else {'0'}
$env:WS_STALL_RECEIVE_BUFFER = [string]$ReceiveBuffer
$env:WS_STALL_PAUSE_AT = [string]$PauseAt
$env:WS_STALL_PAUSE_MS = [string]$PauseMilliseconds
foreach ($variable in @('WS_RESEARCH_LARGE_BUFFER','WS_RESEARCH_NETLOG','WS_RESEARCH_DIAGNOSTIC')) {
    if (Test-Path -LiteralPath "Env:$variable") { Remove-Item -LiteralPath "Env:$variable" }
}
for ($index=0; $index -lt $Rounds; $index++) {
    $env:WS_RESEARCH_LABEL = $Label + '-' + ($index+1)
    $debugLog = Join-Path $repositoryRoot '.agents/debug.log'
    if ((Test-Path -LiteralPath $debugLog) -and (Get-Item -LiteralPath $debugLog).Length -gt 0) {
        $archiveIndex = 1
        while (Test-Path -LiteralPath ($debugLog.Replace('.log','-'+$archiveIndex+'.log'))) { $archiveIndex++ }
        Move-Item -LiteralPath $debugLog -Destination ($debugLog.Replace('.log','-'+$archiveIndex+'.log'))
    }
    @{event='[DEBUG H1 H2 H3 H4] reproduce';label=$env:WS_RESEARCH_LABEL;packets=[bool]$Packets;receiveBuffer=$ReceiveBuffer;pauseAt=$PauseAt;pauseMilliseconds=$PauseMilliseconds;repairRack=[bool]$RepairRack;dropAt=$DropAt} | ConvertTo-Json -Compress | Set-Content -LiteralPath $debugLog -Encoding utf8
    $consoleLog = '.tmp/webrtc-stall-20261022/console-' + [DateTimeOffset]::Now.ToUnixTimeMilliseconds() + '.log'
    @{event='[DEBUG H1 H2 H3 H4] console';path=$consoleLog} | ConvertTo-Json -Compress | Add-Content -LiteralPath $debugLog -Encoding utf8
    node --import ./testdata/connectivity/webrtc/slowdown-20261022/loader.mjs ./testdata/connectivity/webrtc/stall-20261022/probe.mjs baseline 256 1 0 none 1 lan-isolated *> $consoleLog
    if ($LASTEXITCODE -ne 0) { throw ('Stall probe failed; inspect .agents/debug.log: '+$env:WS_RESEARCH_LABEL) }
}
# #endregion DEBUG
