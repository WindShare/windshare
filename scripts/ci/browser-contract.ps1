# Fast browsergate semantic contract entrypoint. Product browser execution and
# real process fixtures remain in their dedicated gates.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== browser-contract =='

pnpm -C web run test:browser:evidence:contract
if ($LASTEXITCODE -ne 0) {
    throw "browser contract exited with code $LASTEXITCODE"
}

Write-Output ('== browser-contract: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
