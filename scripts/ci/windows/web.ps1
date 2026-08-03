# CI-parity web gate (Windows). Dependency acquisition is a shared Make leaf;
# `pnpm build` owns the single TypeScript compilation before Vite bundles the
# application, avoiding a second forced project build in this wrapper.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== web =='

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Invoke-Step 'pnpm lint' { pnpm -C web lint }
Invoke-Step 'pnpm build (single TypeScript compile and Vite bundle)' { pnpm -C web build }
Invoke-Step 'v1 forbidden production graph and bundle' { pnpm -C web forbidden }
Invoke-Step 'vitest remainder (browser contracts have one dedicated owner)' {
    pnpm -C web run test:unit:remainder
}

Write-Output ('== web: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
