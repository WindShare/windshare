# Deterministic core module release gate (Windows). The gate constructs the
# prospective core tag from tracked files plus non-ignored worktree additions,
# extracts the canonical module zip outside the repository, and validates that
# module without go.work or any parent-module files.
[CmdletBinding()]
param(
    [string]$Version = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = if (-not [string]::IsNullOrWhiteSpace($env:CORE_RELEASE_VERSION)) {
        $env:CORE_RELEASE_VERSION
    } elseif (
        -not [string]::IsNullOrWhiteSpace($env:GITHUB_REF) -and
        $env:GITHUB_REF.StartsWith('refs/tags/core/', [StringComparison]::Ordinal)
    ) {
        $env:GITHUB_REF.Substring('refs/tags/core/'.Length)
    } else {
        'v0.2.0'
    }
}

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$repositoryRoot = [IO.Path]::GetFullPath($repositoryRoot)
$originalLocation = Get-Location
$originalGOWORK = $env:GOWORK
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-core-release-{0}' -f [Guid]::NewGuid().ToString('N')
)
$stageDirectory = Join-Path $temporaryRoot 'projected-core'
$zipPath = Join-Path $temporaryRoot 'core.zip'
$artifactRoot = Join-Path $temporaryRoot 'extracted-core'
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

function Remove-OwnedTemporaryRoot {
    if (-not (Test-Path -LiteralPath $temporaryRoot)) {
        return
    }

    $resolvedTemporaryRoot = [IO.Path]::GetFullPath($temporaryRoot)
    $resolvedSystemTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    $ownedPrefix = Join-Path $resolvedSystemTemp 'windshare-core-release-'
    if (-not $resolvedTemporaryRoot.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "refusing to remove unowned temporary path: $resolvedTemporaryRoot"
    }
    Remove-Item -LiteralPath $resolvedTemporaryRoot -Recurse -Force
}

Write-Output '== core-release =='
New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
$env:GOWORK = 'off'

try {
    Set-Location $repositoryRoot
    Invoke-Step "construct deterministic core module zip ($Version)" {
        go run ./scripts/ci/_coremodulezip/main.go -repo $repositoryRoot -stage $stageDirectory -zip $zipPath -extract $artifactRoot -version $Version
    }

    $resolvedArtifactRoot = [IO.Path]::GetFullPath($artifactRoot)
    if ($resolvedArtifactRoot.StartsWith(
        $repositoryRoot + [IO.Path]::DirectorySeparatorChar,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'extracted core artifact must live outside the repository'
    }
    if (Test-Path -LiteralPath (Join-Path $artifactRoot 'go.work')) {
        throw 'core module artifact must not contain go.work'
    }

    Set-Location $artifactRoot
    Invoke-Step 'GOWORK=off go mod tidy -diff (extracted core)' { go mod tidy -diff }
    Invoke-Step 'GOWORK=off go mod verify (extracted core)' { go mod verify }
    Invoke-Step 'GOWORK=off go list ./... (extracted core)' { go list ./... }
    Invoke-Step 'GOWORK=off go build ./... (extracted core)' { go build ./... }
    Invoke-Step 'GOWORK=off go test ./... (extracted core)' { go test -count=1 ./... }
    Invoke-Step 'GOWORK=off go test -race ./... (extracted core)' { go test -race -count=1 ./... }
} finally {
    Set-Location $originalLocation
    $env:GOWORK = $originalGOWORK
    Remove-OwnedTemporaryRoot
}

Write-Output ('== core-release: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
