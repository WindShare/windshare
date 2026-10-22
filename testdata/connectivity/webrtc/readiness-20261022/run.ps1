param([ValidateSet('baseline','interaction','preview','profile')][string]$Stage = 'baseline')
$ErrorActionPreference = 'Stop'
$probeRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path
Set-Location -LiteralPath $probeRoot
python (Join-Path $PSScriptRoot 'prepare.py')
if ($LASTEXITCODE -ne 0) { throw 'Snapshot verification failed' }
foreach ($arm in @('old','new')) {
    $modules = Join-Path $probeRoot ('.tmp/webrtc-readiness-20261022/' + $arm + '/web/node_modules')
    if (!(Test-Path -LiteralPath $modules)) { New-Item -ItemType Junction -Path $modules -Target (Join-Path $probeRoot 'web/node_modules') | Out-Null }
}
$env:WS_RESEARCH_BINARY_DIRECTORY = (Resolve-Path $(if ($Stage -eq 'profile') { '.tmp/webrtc-slowdown-20261022/native-stats' } else { '.tmp/single-connection-implementation/baseline' })).Path
$env:WS_RESEARCH_ISOLATE_BROWSER_DNS = '1'
$env:WS_RESEARCH_TRACE = 'on'
$env:WS_RESEARCH_OUTPUT = 'opfs'
$env:WS_RESEARCH_FIXTURE = (Resolve-Path '.tmp/webrtc-interleaved-20261022/fixture.bin').Path
$env:WS_RETEST_STAGE = $Stage
$env:WS_RETEST_COHORT = [guid]::NewGuid().ToString('N')
foreach ($variable in @('WS_RESEARCH_LARGE_BUFFER','WS_RESEARCH_NETLOG','WS_RESEARCH_DIAGNOSTIC')) {
    if (Test-Path -LiteralPath "Env:$variable") { Remove-Item -LiteralPath "Env:$variable" }
}
$order = if ($Stage -eq 'baseline') { @('old','new','new','old') * 4 } else { @('old','new','new','old') }
$interactions = switch ($Stage) {
    'interaction' { @('preview','cancel-preview') }
    'preview' { @('preview') }
    default { @('none') }
}
foreach ($interaction in $interactions) {
    for ($index = 0; $index -lt $order.Length; $index++) {
        $env:WS_RETEST_ARM = $order[$index]
        $env:WS_RETEST_PAIR = $interaction + '-' + ([math]::Floor($index / 2) + 1)
        $env:WS_RETEST_ORDER = [string]($index + 1)
        $env:WS_RESEARCH_LABEL = $Stage + '-' + $interaction + '-' + $env:WS_RETEST_ORDER + '-' + $env:WS_RETEST_ARM
        Write-Output $env:WS_RESEARCH_LABEL
        node --import ./testdata/connectivity/webrtc/slowdown-20261022/loader.mjs ./testdata/connectivity/webrtc/readiness-20261022/probe.mjs baseline 256 1 0 $interaction 1 lan-isolated
        if ($LASTEXITCODE -ne 0) { throw ('Probe failed: ' + $env:WS_RESEARCH_LABEL) }
    }
}
