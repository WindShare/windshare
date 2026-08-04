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

Write-Output '== vet =='
Invoke-Step 'go vet (root)' { go vet ./... }
Invoke-Step 'go vet (core)' { go -C core vet ./... }

# This is the one build that tests a different dependency graph: disabling the
# workspace proves the root module consumes the released core module cleanly.
$originalGoWork = [Environment]::GetEnvironmentVariable('GOWORK', 'Process')
try {
    $env:GOWORK = 'off'
    Invoke-Step 'GOWORK=off released-core consumer build' { go build ./... }
} finally {
    if ($null -eq $originalGoWork) {
        Remove-Item Env:GOWORK -ErrorAction SilentlyContinue
    } else {
        $env:GOWORK = $originalGoWork
    }
}

Write-Output ('== vet: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
