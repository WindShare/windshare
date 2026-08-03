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
Import-Module (Join-Path $ciRoot 'test-run-id.psm1') -Force
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Invoke-WithWindShareTestRunID -Suite 'check' -Body {
    param([string]$RunID)
    Write-Output "== check: run_id=$runID =="
    Invoke-Step 'root short tests' { go test -json -short -count=1 ./... }
    Invoke-Step 'core short tests' { go -C core test -short -count=1 ./... }
    Invoke-Step 'root vet' { go vet ./... }
    Invoke-Step 'core vet' { go -C core vet ./... }
    Invoke-Step 'web lint' { pnpm -C web lint }
    Invoke-Step 'web typecheck' { pnpm -C web exec tsc -b --force }
    Invoke-Step 'web unit tests' { pnpm -C web run test:unit:remainder }
    Write-Output ('== check: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
}
