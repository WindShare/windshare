# Cross-platform lint gate (Windows host). The pinned linter is installed for
# the host before GOOS is varied: `go run` honors GOOS and would otherwise
# build a Linux executable that Windows cannot launch. Each module is then
# analyzed for both production OS file sets with the same root configuration.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Same pin as scripts/ci/linux/lint.sh; bump both platform entry points together.
$golangciLint = 'github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2'
$targetOperatingSystems = @('linux', 'windows')

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== lint =='

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

function Get-GoEnv([string]$Name) {
    $value = go env $Name
    if ($LASTEXITCODE -ne 0) {
        throw "go env $Name exited with code $LASTEXITCODE"
    }
    return $value.Trim()
}

Invoke-Step 'GitHub Actions workflow lint' {
    & (Join-Path $PSScriptRoot 'workflow-lint.ps1')
}

$hostOperatingSystem = Get-GoEnv 'GOHOSTOS'
$hostArchitecture = Get-GoEnv 'GOHOSTARCH'
$goBin = Get-GoEnv 'GOBIN'
if ([string]::IsNullOrWhiteSpace($goBin)) {
    $goPath = Get-GoEnv 'GOPATH'
    $firstGoPath = ($goPath -split [IO.Path]::PathSeparator)[0]
    $goBin = Join-Path $firstGoPath 'bin'
}

$linterExecutable = if ($hostOperatingSystem -eq 'windows') {
    'golangci-lint.exe'
} else {
    'golangci-lint'
}
$linterPath = Join-Path $goBin $linterExecutable

$originalTargetOperatingSystem = [Environment]::GetEnvironmentVariable('GOOS', 'Process')
$originalTargetArchitecture = [Environment]::GetEnvironmentVariable('GOARCH', 'Process')
try {
    # Installing under host settings keeps the executable runnable while the
    # later invocations select target-specific source files through GOOS.
    $env:GOOS = $hostOperatingSystem
    $env:GOARCH = $hostArchitecture
    Invoke-Step 'install golangci-lint (host)' { go install $golangciLint }
    if (-not (Test-Path -LiteralPath $linterPath -PathType Leaf)) {
        throw "golangci-lint was not installed at $linterPath"
    }

    foreach ($targetOperatingSystem in $targetOperatingSystems) {
        $env:GOOS = $targetOperatingSystem
        $env:GOARCH = $hostArchitecture
        Invoke-Step "golangci-lint (root, GOOS=$targetOperatingSystem)" {
            & $linterPath run ./...
        }

        Push-Location (Join-Path $repositoryRoot 'core')
        try {
            Invoke-Step "golangci-lint (core, GOOS=$targetOperatingSystem)" {
                & $linterPath run ./...
            }
        } finally {
            Pop-Location
        }
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

Write-Output ('== lint: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
