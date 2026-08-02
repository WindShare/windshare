# The root short sweep still visits integration/E2E packages. A gate-owned run
# ID keeps any non-skipped loopback work correlated without changing its budget.
# JSON mode preserves successful scenario evidence that ordinary Go output hides.
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
$runID = New-WindShareTestRunID -Suite 'check'
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

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
    Write-Output "== check: run_id=$runID =="
    Invoke-Step 'root short tests' { Invoke-WindShareGoTestJSON -short -count=1 ./... }
    Invoke-Step 'core short tests' { Invoke-WindShareGo -C core test -short -count=1 ./... }
    Invoke-Step 'root vet' { Invoke-WindShareGo vet ./... }
    Invoke-Step 'core vet' { Invoke-WindShareGo -C core vet ./... }
    Invoke-Step 'web lint' { Invoke-WindShareGoConsumer pnpm -C web lint }
    Invoke-Step 'web typecheck' { Invoke-WindShareGoConsumer pnpm -C web exec tsc -b --force }
    Invoke-Step 'web unit tests' { Invoke-WindShareGoConsumer pnpm -C web run test:unit:remainder }
    Write-Output ('== check: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
} finally {
    if ($hadRunID) {
        $env:WINDSHARE_TEST_RUN_ID = $previousRunID
    } else {
        Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
    }
}
