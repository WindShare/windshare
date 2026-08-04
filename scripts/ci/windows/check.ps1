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

Write-Output '== check =='
Invoke-Step 'root short tests' { go test -short ./... }
Invoke-Step 'core short tests' { go -C core test -short ./... }
Invoke-Step 'Web typecheck' { pnpm -C web exec tsc -b --force }
Invoke-Step 'Web unit tests' { pnpm -C web test }
Write-Output ('== check: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
