[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== browser-generated =='

pnpm -C web run test:browser:generated-semantic:process
if ($LASTEXITCODE -ne 0) {
    throw "generated browser semantic process exited with code $LASTEXITCODE"
}

Write-Output ('== browser-generated: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
