# CI-parity race gate (Windows). CI assigns the ordinary and NTFS-heavy core
# workloads to separate runners; the local gate executes those same workloads
# sequentially so one disk never serves both at once. OS-network cases gate-skip
# on Windows outside the D5 runner by design (internal/testnetwork constructors);
# their race coverage comes from `make network`. d5networkpolicy is excluded
# from race builds (//go:build !race on its test files): a deterministic
# static-analysis gate the race detector cannot inform; it runs in `make
# coverage` instead. The core timeout is a native-suite ceiling, not a runtime
# target.
[CmdletBinding()]
param(
    [ValidateSet('all', 'ordinary', 'output-runtime')]
    [string]$CoreWorkload = 'all',
    [switch]$SkipRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
$coreSuiteTestTimeout = if ([string]::IsNullOrWhiteSpace($env:CORE_SUITE_TEST_TIMEOUT)) {
    '30m'
} else {
    $env:CORE_SUITE_TEST_TIMEOUT
}
$outputRuntimePackage = 'github.com/windshare/windshare/core/osfs/internal/outputruntime'
Write-Output '== race =='

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

if (-not $SkipRoot) {
    Invoke-Step 'go test -race (root, OS-network cases gated)' { go test -race -count=1 ./... }
}

$corePackagePaths = @(go -C core list ./...)
if ($LASTEXITCODE -ne 0) {
    throw "go list (core) exited with code $LASTEXITCODE"
}
if ($corePackagePaths -notcontains $outputRuntimePackage) {
    throw "Core race workload package is missing: $outputRuntimePackage"
}
$ordinaryCorePackages = @(
    $corePackagePaths | Where-Object { $_ -ne $outputRuntimePackage }
)
if ($ordinaryCorePackages.Count -eq 0) {
    throw 'The ordinary core race workload must contain at least one package'
}

# outputruntime exercises real NTFS durability operations. Its internal test
# parallelism reduces its own latency but starves catalog's filesystem workload
# when both share hosted Windows storage, so their scheduling boundary is the VM.
if ($CoreWorkload -in @('all', 'ordinary')) {
    Invoke-Step 'go test -race (ordinary core packages)' {
        go -C core test -race -count=1 "-timeout=$coreSuiteTestTimeout" @ordinaryCorePackages
    }
}
if ($CoreWorkload -in @('all', 'output-runtime')) {
    Invoke-Step 'go test -race (NTFS-heavy output runtime)' {
        go -C core test -race -count=1 "-timeout=$coreSuiteTestTimeout" $outputRuntimePackage
    }
}

Write-Output ('== race: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
