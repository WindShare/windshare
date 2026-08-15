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

Write-Output '== race =='
Invoke-Step 'production short race sweep' {
    go test -short -race -count=1 $allPackages
}
Write-Output ('== race: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
