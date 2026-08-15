[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
. scripts/ci/windows/go-package-sets.ps1
$allPackages = Get-WindShareGoPackageSet -Set all
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Write-Output '== vet =='
Invoke-Step 'go vet (production packages)' { go vet $allPackages }

Write-Output ('== vet: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
