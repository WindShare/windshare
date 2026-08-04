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

Write-Output '== lint =='
Invoke-Step 'golangci-lint (root)' { golangci-lint run ./... }

Push-Location (Join-Path $repositoryRoot 'core')
try {
    Invoke-Step 'golangci-lint (core)' { golangci-lint run ./... }
} finally {
    Pop-Location
}

Write-Output ('== lint: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
