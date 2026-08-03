# The Windows Chromium sample is a PR-equivalent critical-path obligation. Full
# validation retains it before appending Browsergate's broader protected suite.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$windowsRoot = Split-Path -Parent $PSScriptRoot
$ciRoot = Split-Path -Parent $windowsRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
Import-Module (Join-Path $ciRoot 'goauthority/authority.psm1') -Force
$null = Enter-WindShareGoAuthority
Import-Module (Join-Path $ciRoot 'test-run-id.psm1') -Force
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Invoke-WithWindShareTestRunID -Suite 'browser-smoke' -Body {
    param([string]$RunID)
    Write-Output "== browser-smoke: run_id=$runID =="
    Invoke-WindShareGoConsumer pnpm -C web exec playwright install chromium
    if ($LASTEXITCODE -ne 0) {
        throw "Chromium runtime installation exited with code $LASTEXITCODE"
    }
    Invoke-WindShareGoConsumer pnpm -C web run test:browser:smoke -- --run-id $runID
    if ($LASTEXITCODE -ne 0) {
        throw "Windows Chromium smoke exited with code $LASTEXITCODE"
    }
    Write-Output ('== browser-smoke: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
}
