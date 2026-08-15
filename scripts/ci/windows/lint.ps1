[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
. scripts/ci/windows/go-package-sets.ps1
Get-WindShareGoPackageSet -Set all | Out-Null
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
Invoke-Step 'golangci-lint (production packages)' {
    # golangci-lint treats import paths as filesystem paths, so use the same
    # complete pattern after the package authority has validated its expansion.
    golangci-lint run ./...
}

Write-Output ('== lint: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
