[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
$chromiumShortContractPort = '4197'

Write-Output '== browser =='
pnpm -C web run test:browser:smoke
if ($LASTEXITCODE -ne 0) {
    throw "Chromium product smoke exited with code $LASTEXITCODE"
}
Write-Output '-- Chromium short browser contracts'
$env:WINDSHARE_CONTRACT_PORT = $chromiumShortContractPort
pnpm -C web run test:browser:contract:short
if ($LASTEXITCODE -ne 0) {
    throw "Chromium short browser contracts exited with code $LASTEXITCODE"
}
Write-Output ('== browser: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
