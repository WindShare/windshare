# Real-network race gate (Windows-only; no dedicated CI job). ci.yml's ubuntu
# jobs run the OS-network cases natively and ungated; on Windows those cases
# gate-skip outside the D5 fixed-path runner, so this gate runs the classified
# network packages' full suites through the runner (NetworkTests mode builds
# with -race). Firewall prompts and host-owned rules are outside the verdict;
# the fixed binary hashes, compiler plans and one-use capability remain the
# launch authority. Together with `make race` this restores the race coverage
# the ubuntu jobs get natively. The runner executes the classified packages
# concurrently and reports every package result.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== network =='

& (Join-Path $repositoryRoot 'scripts\d5-windows-performance.ps1') -Mode NetworkTests
if ($LASTEXITCODE -ne 0) {
    throw "D5 runner NetworkTests exited with code $LASTEXITCODE"
}

Write-Output ('== network: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
