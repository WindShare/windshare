# CI-parity race gate (Windows). Mirrors ci.yml windows-tests' race sweeps
# and the -race test steps of go-root / go-core. OS-network cases gate-skip on
# Windows outside the D5 runner by design (internal/testnetwork constructors);
# their race coverage comes from `make network`. Expect several minutes: the
# root d5networkpolicy package alone takes ~6.5 minutes.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== race =='

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Invoke-Step 'go test -race (root, OS-network cases gated)' { go test -race -count=1 ./... }
Invoke-Step 'go test -race (core)' { go -C core test -race -count=1 ./... }

Write-Output ('== race: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
