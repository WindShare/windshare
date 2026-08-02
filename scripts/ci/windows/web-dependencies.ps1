[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== web-dependencies =='

pnpm -C web install --frozen-lockfile
if ($LASTEXITCODE -ne 0) {
    throw "frozen web dependency installation exited with code $LASTEXITCODE"
}

Write-Output ('== web-dependencies: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
