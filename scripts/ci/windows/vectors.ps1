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

Write-Output '== vectors =='
Invoke-Step 'verify protocol-contract vectors' {
    go test -count=1 ./core/internal/protocolcontract
}
Invoke-Step 'verify peer-signaling vectors' {
    go test -count=1 ./connectivity/v2signal
}
Invoke-Step 'verify diagnostic-correlation vectors' {
    go test -count=1 ./cmd/wind/internal/runtrace -run 'Test(CorrelationV1|DiagnosticCorrelationVectors)'
}

Write-Output ('== vectors: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
