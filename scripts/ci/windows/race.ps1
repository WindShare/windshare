# Root already owns its integration and E2E packages, so a single module sweep
# preserves complete race coverage without replaying selected workloads. One
# gate-owned run ID keeps every package and child process in that sweep joinable.
# The root sweep stays JSON-visible so race instrumentation cannot hide passing
# integration/E2E scenario events.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
Import-Module (Join-Path $ciRoot 'goauthority/authority.psm1') -Force
$null = Enter-WindShareGoAuthority
Import-Module (Join-Path $ciRoot 'test-run-id.psm1') -Force
$hadRunID = Test-Path Env:WINDSHARE_TEST_RUN_ID
$previousRunID = $env:WINDSHARE_TEST_RUN_ID
$runID = New-WindShareTestRunID -Suite 'race'
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
# A fixed native-suite budget keeps platform parity independent of caller state.
$coreSuiteTestTimeout = '30m'
function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

try {
    $env:WINDSHARE_TEST_RUN_ID = $runID
    Write-Output "== race: run_id=$runID =="
    Invoke-Step 'go test -race (root)' { Invoke-WindShareGoTestJSON -race -count=1 ./... }
    Invoke-Step 'go test -race (core)' {
        Invoke-WindShareGo -C core test -race -count=1 "-timeout=$coreSuiteTestTimeout" ./...
    }
    Write-Output ('== race: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
} finally {
    if ($hadRunID) {
        $env:WINDSHARE_TEST_RUN_ID = $previousRunID
    } else {
        Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
    }
}
