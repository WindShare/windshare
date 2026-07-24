# CI-parity coverage gate (Windows): dispatches to scripts/local-coverage.ps1,
# which runs both modules' full suites — the classified OS-network packages
# through the D5 fixed-path runner so Windows does not under-count them — and
# applies the exact go-test-coverage v2.18.8 verdicts of ci.yml's go-root /
# go-core coverage steps (core total >=90%, root total >=80%, package >=70%).
# Expect 10-20 minutes.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== coverage =='

& (Join-Path $repositoryRoot 'scripts\local-coverage.ps1')
if ($LASTEXITCODE -ne 0) {
    throw "local-coverage exited with code $LASTEXITCODE"
}

Write-Output ('== coverage: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
