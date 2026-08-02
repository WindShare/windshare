[CmdletBinding()]
param()

# E2E scenarios publish their verdicts to stdout, so the shared JSON boundary is
# required even on success; a second verbose replay would create false evidence.

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
$runID = New-WindShareTestRunID -Suite 'e2e-go'
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

try {
    $env:WINDSHARE_TEST_RUN_ID = $runID
    Write-Output "== e2e-go: run_id=$runID =="
    Invoke-WindShareGoTestJSON -count=1 ./e2e
    if ($LASTEXITCODE -ne 0) {
        throw "Go E2E tests exited with code $LASTEXITCODE"
    }
    Write-Output ('== e2e-go: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
} finally {
    if ($hadRunID) {
        $env:WINDSHARE_TEST_RUN_ID = $previousRunID
    } else {
        Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
    }
}
