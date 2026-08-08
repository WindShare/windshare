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

Write-Output '== root-release-graph =='

# The workspace intentionally exposes current core source. Disabling it here
# proves that the root module remains buildable from its published dependency.
$originalGoWork = [Environment]::GetEnvironmentVariable('GOWORK', 'Process')
try {
    $env:GOWORK = 'off'
    Invoke-Step 'root build against released core' { go build ./... }
} finally {
    if ($null -eq $originalGoWork) {
        Remove-Item Env:GOWORK -ErrorAction SilentlyContinue
    } else {
        $env:GOWORK = $originalGoWork
    }
}

Write-Output ('== root-release-graph: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
