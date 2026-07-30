# GitHub workflow lint gate (Windows host). Keep this pin and invocation identical
# to workflow-lint.sh so local and hosted validation apply one schema contract.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$actionlint = 'github.com/rhysd/actionlint/cmd/actionlint@v1.7.12'
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot

function Get-GoEnv([string]$Name) {
    $value = go env $Name
    if ($LASTEXITCODE -ne 0) {
        throw "go env $Name exited with code $LASTEXITCODE"
    }
    return $value.Trim()
}

$hostOperatingSystem = Get-GoEnv 'GOHOSTOS'
$hostArchitecture = Get-GoEnv 'GOHOSTARCH'
$originalTargetOperatingSystem = [Environment]::GetEnvironmentVariable('GOOS', 'Process')
$originalTargetArchitecture = [Environment]::GetEnvironmentVariable('GOARCH', 'Process')
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== workflow-lint =='
Write-Output '-- actionlint v1.7.12 (all repository workflows)'

try {
    # go run must build for the host even when a caller has selected another
    # GOOS. Empty external-linter commands keep the contract cross-platform.
    $env:GOOS = $hostOperatingSystem
    $env:GOARCH = $hostArchitecture
    $global:LASTEXITCODE = 0
    go run $actionlint '-shellcheck=' '-pyflakes='
    if ($LASTEXITCODE -ne 0) {
        throw "actionlint exited with code $LASTEXITCODE"
    }
} finally {
    if ($null -eq $originalTargetOperatingSystem) {
        Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    } else {
        $env:GOOS = $originalTargetOperatingSystem
    }
    if ($null -eq $originalTargetArchitecture) {
        Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    } else {
        $env:GOARCH = $originalTargetArchitecture
    }
}

Write-Output ('== workflow-lint: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
