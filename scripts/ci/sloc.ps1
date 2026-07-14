# CI-parity sloc gate (Windows). Mirrors ci.yml job `sloc`: sloc-guard check
# (CI installs the latest release via the sloc-guard action; locally the
# binary is a PATH prerequisite).
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== sloc =='

Write-Output '-- sloc-guard check'
sloc-guard.exe check
if ($LASTEXITCODE -ne 0) {
    throw "sloc-guard check exited with code $LASTEXITCODE"
}

Write-Output ('== sloc: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
