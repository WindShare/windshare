# The recorder writes canonical JSONL to test stdout. -json preserves successful
# output in a machine-readable Actions stream instead of exposing only failures.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$stabilityHandshakeVariableCount = 3
$stabilityHandshakePresenceCount = 0
$processEnvironment = [Environment]::GetEnvironmentVariables(
    [EnvironmentVariableTarget]::Process
)
if ($processEnvironment.Contains('WINDSHARE_STABILITY_START_REQUEST')) {
    $stabilityHandshakePresenceCount++
}
if ($processEnvironment.Contains('WINDSHARE_STABILITY_STARTED_OUTPUT')) {
    $stabilityHandshakePresenceCount++
}
if ($processEnvironment.Contains('WINDSHARE_STABILITY_START_SECRET')) {
    $stabilityHandshakePresenceCount++
}
if ($stabilityHandshakePresenceCount -ne 0 -and
    $stabilityHandshakePresenceCount -ne $stabilityHandshakeVariableCount) {
    throw "WindShare stability handshake is partial (present=$stabilityHandshakePresenceCount required=$stabilityHandshakeVariableCount)"
}
$stabilityEvidenceMode = 'ordinary'
if ($stabilityHandshakePresenceCount -eq $stabilityHandshakeVariableCount) {
    $stabilityEvidenceMode = 'authenticated'
}

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
Import-Module (Join-Path $ciRoot 'test-run-id.psm1') -Force
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Invoke-WithWindShareTestRunID -Suite 'integration' -Body {
    param([string]$RunID)
    Write-Output "== integration: run_id=$runID stability_evidence=$stabilityEvidenceMode =="
    # Stability evidence begins only after the invocation run identity has settled.
    if ($stabilityEvidenceMode -eq 'authenticated') {
        node scripts/ci/stability/result.mjs started
        if ($LASTEXITCODE -ne 0) {
            throw "stability start handshake exited with code $LASTEXITCODE"
        }
        Remove-Item Env:WINDSHARE_STABILITY_START_REQUEST, Env:WINDSHARE_STABILITY_STARTED_OUTPUT, Env:WINDSHARE_STABILITY_START_SECRET -ErrorAction SilentlyContinue
    }
    go test -json -count=1 ./integration/...
    if ($LASTEXITCODE -ne 0) {
        throw "integration tests exited with code $LASTEXITCODE"
    }
    Write-Output ('== integration: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
}
