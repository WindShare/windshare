param([ValidateSet('baseline','profile','wake')][string]$Stage = 'baseline')
$ErrorActionPreference = 'Stop'
$retestRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path
Set-Location -LiteralPath $retestRoot
$retestDirectory = Join-Path $retestRoot '.tmp/webrtc-interleaved-20261022'
New-Item -ItemType Directory -Path $retestDirectory -Force | Out-Null
foreach ($arm in @(@{Name='old';Revision='197fe6f4'}, @{Name='new';Revision='86446a6a'})) {
  $snapshot = Join-Path $retestDirectory $arm.Name
  if (!(Test-Path -LiteralPath $snapshot)) {
    $archive = Join-Path $retestDirectory ($arm.Name + '.zip')
    git archive --format=zip --output=$archive $arm.Revision web
    if ($LASTEXITCODE -ne 0) { throw 'Git archive failed' }
    Expand-Archive -LiteralPath $archive -DestinationPath $snapshot
    New-Item -ItemType Junction -Path (Join-Path $snapshot 'web/node_modules') -Target (Join-Path $retestRoot 'web/node_modules') | Out-Null
  }
}
python (Join-Path $PSScriptRoot 'verify.py')
if ($LASTEXITCODE -ne 0) { throw 'Snapshot verification failed' }
node -e 'const fs=require("node:fs"), c=require("node:crypto"), p=".tmp/webrtc-interleaved-20261022/fixture.bin"; if(!fs.existsSync(p)) fs.writeFileSync(p,c.randomBytes(256*1024*1024))'
if ($LASTEXITCODE -ne 0) { throw 'Fixture preparation failed' }
$env:WS_RESEARCH_BINARY_DIRECTORY = (Resolve-Path $(if ($Stage -eq 'profile') { '.tmp/webrtc-slowdown-20261022/native-stats' } else { '.tmp/single-connection-implementation/baseline' })).Path
$env:WS_RESEARCH_ISOLATE_BROWSER_DNS = '1'
$env:WS_RESEARCH_TRACE = 'on'
$env:WS_RESEARCH_OUTPUT = 'opfs'
$env:WS_RESEARCH_FIXTURE = (Resolve-Path '.tmp/webrtc-interleaved-20261022/fixture.bin').Path
$env:WS_RETEST_DIAGNOSTIC = $(if ($Stage -eq 'profile') {'1'} else {'0'})
$env:WS_RETEST_COUNTERS = $(if ($Stage -eq 'wake') {'1'} else {'0'})
$env:WS_RETEST_STAGE = $Stage
$env:WS_RETEST_COHORT = [guid]::NewGuid().ToString('N')
foreach ($variable in @('WS_RESEARCH_LARGE_BUFFER','WS_RESEARCH_NETLOG','WS_RESEARCH_DIAGNOSTIC')) {
  if (Test-Path -LiteralPath "Env:$variable") { Remove-Item -LiteralPath "Env:$variable" }
}
$order = switch ($Stage) {
  'baseline' { @('old','new','new','old') * 4 }
  'profile' { @('old','new','new','old') }
  'wake' { @('new','readiness-wake','readiness-wake','new') * 2 }
}
for ($index = 0; $index -lt $order.Length; $index++) {
  $env:WS_RETEST_ARM = $order[$index]
  $env:WS_RETEST_PAIR = [string]([math]::Floor($index / 2) + 1)
  $env:WS_RETEST_ORDER = [string]($index + 1)
  $env:WS_RESEARCH_LABEL = $Stage + '-' + $env:WS_RETEST_ORDER + '-' + $env:WS_RETEST_ARM
  Write-Output ("{0} {1}/{2}: {3}" -f $Stage, ($index + 1), $order.Length, $env:WS_RETEST_ARM)
  node --import ./testdata/connectivity/webrtc/slowdown-20261022/loader.mjs ./testdata/connectivity/webrtc/interleaved-20261022/probe.mjs baseline 256 1 0 none 1 lan-isolated
  if ($LASTEXITCODE -ne 0) { throw "Probe failed: $env:WS_RESEARCH_LABEL" }
}
