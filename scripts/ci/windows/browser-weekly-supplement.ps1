[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
$firefoxWebkitContractPort = '4198'
$chromiumPeriodicContractPort = '4199'

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Write-Output '== browser-weekly-supplement =='
Invoke-Step 'Firefox and WebKit component contracts' {
    $env:WINDSHARE_CONTRACT_PORT = $firefoxWebkitContractPort
    pnpm -C web run test:browser:contract:cross
}
Invoke-Step 'Chromium periodic component contracts' {
    $env:WINDSHARE_CONTRACT_PORT = $chromiumPeriodicContractPort
    pnpm -C web run test:browser:contract:periodic
}
Invoke-Step 'progressive catalog paging' { pnpm -C web run test:browser:progressive }
Invoke-Step 'direct and TURN relay-cut routes' { pnpm -C web run test:browser:network }
Invoke-Step 'browser and Pion interoperability' { pnpm -C web run test:browser:interop }
Invoke-Step 'Firefox and WebKit product routes' { pnpm -C web run test:browser:cross }
Write-Output ('== browser-weekly-supplement: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
