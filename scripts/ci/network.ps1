# Real-network race gate (Windows-only; no dedicated CI job). ci.yml's ubuntu
# jobs run the OS-network cases natively and ungated; on Windows those cases
# gate-skip outside the D5 fixed-path runner, so this gate runs the classified
# network packages' full suites through the runner (NetworkTests mode builds
# with -race; pre-registered firewall rule pairs mean no prompts and no
# mutations). Together with `make race` this restores the race coverage the
# ubuntu jobs get natively.
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
