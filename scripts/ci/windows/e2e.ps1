[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

Write-Output '== e2e =='
go run ./scripts/ci/_gotestsuite -run '^TestUserTraceCriticalSenderRelayReceiver$' ./e2e
if ($LASTEXITCODE -ne 0) {
    throw "critical Go E2E exited with code $LASTEXITCODE"
}
Write-Output ('== e2e: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
