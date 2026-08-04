[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Write-Output '== workflow-lint =='
actionlint '-shellcheck=' '-pyflakes='
if ($LASTEXITCODE -ne 0) {
    throw "actionlint exited with code $LASTEXITCODE"
}
Write-Output ('== workflow-lint: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
