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
Import-Module (Join-Path $ciRoot 'test-run-id.psm1') -Force
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

Invoke-WithWindShareTestRunID -Suite 'race' -Body {
    param([string]$RunID)
    Write-Output "== race: run_id=$runID =="
    Invoke-Step 'go test -race (root)' { go test -json -race -count=1 ./... }
    Invoke-Step 'go test -race (core)' {
        go -C core test -race -count=1 "-timeout=$coreSuiteTestTimeout" ./...
    }
    Write-Output ('== race: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
}
