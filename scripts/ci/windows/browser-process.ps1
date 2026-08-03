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
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Invoke-WithWindShareTestRunID -Suite 'browser-process' -Body {
    param([string]$RunID)
    Write-Output "== browser-process: run_id=$runID =="
    Invoke-WindShareGoConsumer pnpm -C web run test:browser:process:integration
    if ($LASTEXITCODE -ne 0) {
        throw "browser process integration exited with code $LASTEXITCODE"
    }
    Invoke-WindShareGo test -count=1 ./cmd/testprocessowner ./internal/testprocess ./internal/processowner/protocol ./internal/processowner/windowsjob
    if ($LASTEXITCODE -ne 0) {
        throw "Windows process ownership stack tests exited with code $LASTEXITCODE"
    }
    Write-Output ('== browser-process: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
}
