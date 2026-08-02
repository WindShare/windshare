# CI-parity web gate (Windows). Dependency acquisition is a shared Make leaf;
# this gate owns only lint, forced typecheck, build, the v1-forbidden production
# graph, and vitest (which consumes every retained golden-vector family).
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
Invoke-Step 'forced typecheck (tsc -b --force)' { pnpm -C web exec tsc -b --force }
Invoke-Step 'pnpm build' { pnpm -C web build }
Invoke-Step 'v1 forbidden production graph and bundle' { pnpm -C web forbidden }
Invoke-Step 'vitest remainder (browser contracts have one dedicated owner)' {
    pnpm -C web run test:unit:remainder
}

Write-Output ('== web: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
