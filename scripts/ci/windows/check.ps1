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

Write-Output '== check =='
Invoke-Step 'production short tests' { go test -short $allPackages }
Invoke-Step 'Web typecheck' { pnpm -C web exec tsc -b --force }
Invoke-Step 'Web unit tests' { pnpm -C web test }
Write-Output ('== check: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
