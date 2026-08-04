[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Write-Output '== long-go =='
Invoke-Step 'named E2E long suites' { go test -count=1 -run '^TestLong' ./e2e }
Invoke-Step 'integration packages' {
    go test -count=1 ./integration/relayv2 ./integration/v2peer
}
Invoke-Step 'catalog long suites' {
    go -C core test -count=1 -run '^TestLong' ./catalog
}
Invoke-Step 'output runtime long suites' {
    go -C core test -count=1 -run '^TestLong' ./osfs/internal/outputruntime
}
Write-Output ('== long-go: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
