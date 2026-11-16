[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Write-Output '== gopls =='
node --test scripts/ci/gopls/check.integration.mjs
if ($LASTEXITCODE -ne 0) {
    throw "gopls coverage contracts exited with code $LASTEXITCODE"
}
node scripts/ci/gopls/run.mjs
if ($LASTEXITCODE -ne 0) {
    throw "gopls gate exited with code $LASTEXITCODE"
}
Write-Output ('== gopls: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
