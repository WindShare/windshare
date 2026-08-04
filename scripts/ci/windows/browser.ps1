[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Write-Output '== browser =='
pnpm -C web run test:browser:smoke
if ($LASTEXITCODE -ne 0) {
    throw "Chromium product smoke exited with code $LASTEXITCODE"
}
Write-Output ('== browser: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
