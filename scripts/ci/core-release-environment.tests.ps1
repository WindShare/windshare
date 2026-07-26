Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'core-release-environment.psm1') -Force

function Assert-Throws([string]$Label, [scriptblock]$Body) {
    $threw = $false
    try {
        & $Body
    } catch {
        $threw = $true
    }
    if (-not $threw) {
        throw "$Label did not fail closed"
    }
}

$testRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-core-environment-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
$freshRoot = Join-Path $testRoot 'fresh'
$blockedRoot = Join-Path $testRoot 'blocked'
$state = $null
$seededNames = @(
    'GOMODCACHE', 'GOCACHE', 'GOPATH', 'GOENV', 'GOFLAGS', 'GOTOOLCHAIN', 'GOWORK',
    'GOPROXY', 'GOSUMDB', 'GOPRIVATE', 'GONOSUMDB', 'GONOPROXY', 'GOINSECURE', 'GOTELEMETRY',
    'GOOS', 'GOARCH', 'CGO_ENABLED', 'GOEXPERIMENT'
)
$callerValues = [ordered]@{}
foreach ($name in $seededNames) {
    $callerValues[$name] = [Environment]::GetEnvironmentVariable(
        $name,
        [EnvironmentVariableTarget]::Process
    )
}

try {
    New-Item -ItemType Directory -Path @($freshRoot, $blockedRoot) | Out-Null
    foreach ($name in $seededNames) {
        [Environment]::SetEnvironmentVariable(
            $name,
            "caller-$($name.ToLowerInvariant())",
            [EnvironmentVariableTarget]::Process
        )
    }

    $state = Enter-CoreReleaseGoEnvironment -ReleaseRoot $freshRoot
    $expectedCaches = [ordered]@{
        GOMODCACHE = Join-Path $freshRoot 'go-module-cache'
        GOCACHE    = Join-Path $freshRoot 'go-build-cache'
        GOPATH     = Join-Path $freshRoot 'go-path'
    }
    foreach ($entry in $expectedCaches.GetEnumerator()) {
        $actual = [Environment]::GetEnvironmentVariable(
            $entry.Key,
            [EnvironmentVariableTarget]::Process
        )
        if ($actual -cne $entry.Value) {
            throw "$($entry.Key) = $actual, want $($entry.Value)"
        }
        if (-not (Test-Path -LiteralPath $entry.Value -PathType Container) -or
            @(Get-ChildItem -LiteralPath $entry.Value -Force).Count -ne 0) {
            throw "$($entry.Key) is not a new empty directory"
        }
    }

    $expectedFixed = [ordered]@{
        GOENV       = 'off'
        GOFLAGS     = ''
        GOTOOLCHAIN = 'local'
        GOWORK      = 'off'
        GOPROXY     = 'https://proxy.golang.org'
        GOSUMDB     = 'sum.golang.org'
        GOPRIVATE   = ''
        GONOSUMDB   = ''
        GONOPROXY   = ''
        GOINSECURE  = ''
        GOTELEMETRY = 'off'
    }
    foreach ($entry in $expectedFixed.GetEnumerator()) {
        $actual = [Environment]::GetEnvironmentVariable(
            $entry.Key,
            [EnvironmentVariableTarget]::Process
        )
        if (-not [string]::Equals($actual, $entry.Value, [StringComparison]::Ordinal)) {
            throw "$($entry.Key) = $actual, want $($entry.Value)"
        }
    }
    foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOEXPERIMENT')) {
        $actual = [Environment]::GetEnvironmentVariable(
            $name,
            [EnvironmentVariableTarget]::Process
        )
        if ($null -ne $actual) {
            throw "$name still overrides the host toolchain"
        }
    }

    Exit-CoreReleaseGoEnvironment -State $state
    $state = $null
    foreach ($name in $seededNames) {
        $actual = [Environment]::GetEnvironmentVariable(
            $name,
            [EnvironmentVariableTarget]::Process
        )
        $want = "caller-$($name.ToLowerInvariant())"
        if ($actual -cne $want) {
            throw "environment restoration changed $name"
        }
    }

    New-Item -ItemType Directory -Path (Join-Path $blockedRoot 'go-build-cache') | Out-Null
    $beforeFailure = [Environment]::GetEnvironmentVariables(
        [EnvironmentVariableTarget]::Process
    )
    Assert-Throws 'pre-existing cache' {
        Enter-CoreReleaseGoEnvironment -ReleaseRoot $blockedRoot
    }
    if ((Test-Path -LiteralPath (Join-Path $blockedRoot 'go-module-cache')) -or
        (Test-Path -LiteralPath (Join-Path $blockedRoot 'go-path'))) {
        throw 'failed cache preflight created another cache directory'
    }
    foreach ($name in $seededNames) {
        $after = [Environment]::GetEnvironmentVariable(
            $name,
            [EnvironmentVariableTarget]::Process
        )
        if ($after -cne [string]$beforeFailure[$name]) {
            throw "failed cache preflight changed $name"
        }
    }
} finally {
    if ($null -ne $state) {
        Exit-CoreReleaseGoEnvironment -State $state
    }
    foreach ($name in $seededNames) {
        [Environment]::SetEnvironmentVariable(
            $name,
            $callerValues[$name],
            [EnvironmentVariableTarget]::Process
        )
    }
    if (Test-Path -LiteralPath $testRoot) {
        $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
        $resolvedTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
        $ownedPrefix = Join-Path $resolvedTemp 'windshare-core-environment-contract-'
        if (-not $resolvedRoot.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "refusing to remove unowned environment-test path: $resolvedRoot"
        }
        Remove-Item -LiteralPath $resolvedRoot -Recurse -Force
    }
}

Write-Output 'core release Go environment contract: PASS'
