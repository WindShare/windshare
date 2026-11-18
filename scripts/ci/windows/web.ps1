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

Write-Output '== web =='
Invoke-Step 'Web ESLint' { pnpm -C web lint }
Invoke-Step 'Consoles ESLint' { pnpm -C consoles lint }
Invoke-Step 'Consoles TypeScript' { pnpm -C consoles typecheck }
Invoke-Step 'TypeScript and Vite build' { pnpm -C web build }
Invoke-Step 'Vitest' { pnpm -C web test }
Write-Output ('== web: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
