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

Write-Output '== vectors-update =='
Invoke-Step 'update protocol-contract vectors' {
    go -C core test -count=1 ./internal/protocolcontract -update
}
Invoke-Step 'update peer-signaling vectors' {
    go test -count=1 ./connectivity/v2signal -update
}

Write-Output ('== vectors-update: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
