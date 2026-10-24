param([ValidateSet('baseline','pressure','small','fault','controls','zip','competition')][string]$Stage = 'baseline')
$ErrorActionPreference = 'Stop'
$probeRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path
Set-Location -LiteralPath $probeRoot
$labSettings = Join-Path $probeRoot '.tmp/webrtc-preflight-20261023/lab.json'
if (Test-Path -LiteralPath $labSettings) {
    (Get-Content -LiteralPath $labSettings -Raw | ConvertFrom-Json -AsHashtable).GetEnumerator() | ForEach-Object { [Environment]::SetEnvironmentVariable($_.Key, $_.Value, 'Process') }
}
$env:GOWORK = 'off'
$env:GIT_OPTIONAL_LOCKS = '0'
$env:WS_RESEARCH_ISOLATE_BROWSER_DNS = '1'
$env:WS_RESEARCH_OUTPUT = 'opfs'
$env:WS_RESEARCH_TRACE = 'off'
$env:WS_PREFLIGHT_LAUNCH = 'serial'
$env:WS_PREFLIGHT_FAULT = 'none'
$env:WS_PREFLIGHT_LATE = '0'
$env:WS_PREFLIGHT_COLD = '0'
$env:WS_PREFLIGHT_PROFILE = '0'
$matrix = switch ($Stage) {
    'baseline' { @(1,2,4,4,2,1,1,4,2,2,4,1) | ForEach-Object { @{ connections=$_; size=256; delay=0; interaction='none'; topology='lan-isolated' } } }
    'pressure' {
        foreach ($interaction in @('none','preview','cancel-preview')) {
            foreach ($connections in @(1,4,4,1)) {
                @{ connections=$connections; size=64; delay=$(if ($interaction -eq 'none') {125} else {0}); interaction=$interaction; topology='lan-isolated' }
            }
        }
    }
    'small' { @(1,4,4,1,1,4) | ForEach-Object { @{ connections=$_; size=0.125; delay=0; interaction='none'; topology='lan-isolated'; cold='1' } } }
    'fault' {
        @{connections=4;size=256;delay=0;interaction='none';topology='lan-isolated';late='1'}
        @{connections=4;size=256;delay=0;interaction='none';topology='lan-isolated';fault='retire'}
        @{connections=2;size=256;delay=0;interaction='none';topology='lan-isolated';fault='unavailable';trace='on'}
        @{connections=4;size=64;delay=0;interaction='none';topology='lan-isolated';launch='parallel';trace='on'}
    }
    'zip' { @(1,4,4,1) | ForEach-Object { @{connections=$_;size=64;delay=0;interaction='none';topology='lan-isolated';workflow='zip'} } }
    'competition' { @(1,2,4,4,2,1) | ForEach-Object { @{connections=$_;size=128;delay=0;interaction='none';topology='lan-isolated';receivers='2';trace=$(if ($_ -eq 4) {'on'} else {'off'})} } }
    'controls' {
        foreach ($connections in @(1,4,4,1)) { @{connections=$connections;size=256;delay=0;interaction='none';topology='lan-isolated';output='memory'} }
        foreach ($connections in @(1,4,4,1)) { @{connections=$connections;size=256;delay=0;interaction='none';topology='loopback'} }
        foreach ($connections in @(1,4)) { @{connections=$connections;size=256;delay=0;interaction='none';topology='lan-isolated';profile='1';trace='on'} }
    }
}
$batch = "$Stage-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
$logDirectory = Join-Path $probeRoot ".tmp/webrtc-preflight-20261023/batches/$batch"
New-Item -ItemType Directory -Path $logDirectory -ErrorAction Stop | Out-Null
$index = 0
foreach ($entry in $matrix) {
    $index++
    $env:WS_PREFLIGHT_CONNECTIONS = [string]$entry.connections
    $env:WS_PREFLIGHT_WORKFLOW = if ($entry.workflow) { $entry.workflow } else { 'file' }
    $env:WS_PREFLIGHT_RECEIVERS = if ($entry.receivers) { $entry.receivers } else { '1' }
    $env:WS_PREFLIGHT_LATE = if ($entry.late) { $entry.late } else { '0' }
    $env:WS_PREFLIGHT_COLD = if ($entry.cold) { $entry.cold } else { '0' }
    $env:WS_PREFLIGHT_PROFILE = if ($entry.profile) { $entry.profile } else { '0' }
    $env:WS_PREFLIGHT_FAULT = if ($entry.fault) { $entry.fault } else { 'none' }
    $env:WS_PREFLIGHT_LAUNCH = if ($entry.launch) { $entry.launch } else { 'serial' }
    $env:WS_RESEARCH_OUTPUT = if ($entry.output) { $entry.output } else { 'opfs' }
    $env:WS_RESEARCH_TRACE = if ($entry.trace) { $entry.trace } else { 'off' }
    $env:WS_RESEARCH_LABEL = "$batch-$index-$($entry.connections)"
    $entry | ConvertTo-Json -Compress | Set-Content -LiteralPath (Join-Path $logDirectory "$index-config.json")
    Write-Output "Starting $env:WS_RESEARCH_LABEL"
    node --import ./testdata/connectivity/webrtc/slowdown-20261022/loader.mjs ./testdata/connectivity/webrtc/preflight-20261023/probe.mjs baseline $entry.size 1 $entry.delay $entry.interaction 1 $entry.topology *> (Join-Path $logDirectory "$index.log")
    $exitCode = $LASTEXITCODE
    Get-Content -LiteralPath (Join-Path $logDirectory "$index.log") | Select-String '"repetition":|"report":' | ForEach-Object { $_.Line }
    [ordered]@{index=$index;label=$env:WS_RESEARCH_LABEL;exitCode=$exitCode;config=$entry} | ConvertTo-Json -Compress | Add-Content -LiteralPath (Join-Path $logDirectory 'ledger.ndjson')
    if ($exitCode -ne 0 -and $Stage -notin @('fault','competition')) { throw "Probe failed; retained $logDirectory/$index.log" }
}
