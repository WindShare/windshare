# CI-parity race gate (Windows). Mirrors ci.yml windows-tests' race sweeps
# and the -race test steps of go-root / go-core. OS-network cases gate-skip on
# Windows outside the D5 runner by design (internal/testnetwork constructors);
# their race coverage comes from `make network`. d5networkpolicy is excluded
# from race builds (//go:build !race on its test files): a deterministic
# static-analysis gate the race detector cannot inform; it runs in `make
# coverage` instead. Expect well under a minute on a warm cache.
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
