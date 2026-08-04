[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Write-Output '== sloc =='
sloc-guard check
if ($LASTEXITCODE -ne 0) {
    throw "sloc-guard check exited with code $LASTEXITCODE"
}
Write-Output ('== sloc: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
