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

Write-Output '== browser-weekly-supplement =='
Invoke-Step 'progressive catalog paging' { pnpm -C web run test:browser:progressive }
Invoke-Step 'direct and TURN relay-cut routes' { pnpm -C web run test:browser:network }
Invoke-Step 'browser and Pion interoperability' { pnpm -C web run test:browser:interop }
Invoke-Step 'Firefox and WebKit product smoke' { pnpm -C web run test:browser:cross }
Write-Output ('== browser-weekly-supplement: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
