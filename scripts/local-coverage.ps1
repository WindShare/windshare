# One-command local coverage gate: runs both Go modules' full test suites,
# including the OS-network cases through the D5 fixed-path runner, and applies
# the exact go-test-coverage verdicts CI applies. Windows dev runs no longer
# under-count network-heavy packages.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    throw 'local-coverage drives the D5 fixed-path Windows runner and is Windows-only.'
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$coverageRoot = Join-Path $repositoryRoot 'tmp\local-coverage'
# Same pinned gate tool as .github/workflows/ci.yml (GO_TEST_COVERAGE).
$goTestCoverage = 'github.com/vladopajic/go-test-coverage/v2@v2.18.8'

Write-Output ('Full-suite coverage run (core + root incl. OS-network cases): ' +
    'expect 10-20 minutes; the root sweep alone carries the ~6.5-minute d5networkpolicy package.')

if (Test-Path -LiteralPath $coverageRoot) {
    Remove-Item -LiteralPath $coverageRoot -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $coverageRoot | Out-Null

function Invoke-LocalGo([string]$Directory, [string[]]$Arguments, [string]$Context) {
    Push-Location $Directory
    try {
        & go @Arguments
        if ($LASTEXITCODE -ne 0) {
            throw "$Context exited with code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
}

# core is pure (no network gating), so one plain CI-parity sweep measures it.
$coreProfile = Join-Path $coverageRoot 'core.cover.out'
Invoke-LocalGo (Join-Path $repositoryRoot 'core') @(
    'test', '-count=1', '-covermode=atomic', "-coverprofile=$coreProfile", './...'
) 'core module coverage tests'

$networkManifest = @(
    Get-Content -LiteralPath (Join-Path $PSScriptRoot 'd5-windows-network-packages.json') -Raw |
        ConvertFrom-Json
)
Push-Location $repositoryRoot
try {
    # `go list -m` reports every go.work module; the root go.mod is the
    # single authority for this module's import-path prefix.
    $goModJSON = & go mod edit -json
    if ($LASTEXITCODE -ne 0) {
        throw 'Could not read the root go.mod'
    }
    $modulePath = [string](($goModJSON -join "`n") | ConvertFrom-Json).Module.Path
    $allPackages = @(& go list ./...)
    if ($LASTEXITCODE -ne 0) {
        throw 'Could not enumerate the root module packages'
    }
} finally {
    Pop-Location
}
function Get-LocalNetworkImportPath([object]$Package) {
    return "$modulePath/$(([string]$Package.Path).TrimStart('.', '/'))"
}
$networkImportPaths = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
foreach ($package in $networkManifest) {
    [void]$networkImportPaths.Add((Get-LocalNetworkImportPath $package))
}

# Each package is counted from exactly one execution strategy: classified
# network packages run only through the fixed-path runner (full suite, gated
# cases included); every other package runs only in this ordinary sweep.
$ordinaryPackages = @($allPackages | Where-Object { -not $networkImportPaths.Contains($_) })
if ($allPackages.Count - $ordinaryPackages.Count -ne $networkImportPaths.Count) {
    throw 'The network package manifest does not match go list output exactly'
}
$ordinaryProfile = Join-Path $coverageRoot 'root-ordinary.cover.out'
Invoke-LocalGo $repositoryRoot (@(
    'test', '-count=1', '-covermode=atomic', "-coverprofile=$ordinaryProfile"
) + $ordinaryPackages) 'root module ordinary coverage tests'

# Classified packages execute their real OS-network cases under the D5
# fixed-path identities (pre-registered rule pairs: no prompts, no mutations).
& (Join-Path $PSScriptRoot 'd5-windows-performance.ps1') `
    -Mode NetworkTests `
    -CoverProfileRoot $coverageRoot

function Read-LocalCoverageBody([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Coverage profile is missing: $Path"
    }
    $lines = @(Get-Content -LiteralPath $Path)
    if ($lines.Count -eq 0 -or $lines[0] -cne 'mode: atomic') {
        throw "Coverage profile does not declare mode atomic: $Path"
    }
    return @($lines | Select-Object -Skip 1 | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}

function Get-LocalCoverageLinePackage([string]$Line) {
    $file = $Line.Substring(0, $Line.IndexOf(':'))
    return $file.Substring(0, $file.LastIndexOf('/'))
}

# The two strategies cover disjoint package sets (asserted below), so profile
# concatenation is a sound merge: no block can appear twice.
$mergedLines = [Collections.Generic.List[string]]::new()
foreach ($line in Read-LocalCoverageBody $ordinaryProfile) {
    if ($networkImportPaths.Contains((Get-LocalCoverageLinePackage $line))) {
        throw "Ordinary sweep double-counts a fixed-path package: $line"
    }
    $mergedLines.Add($line)
}
foreach ($package in $networkManifest) {
    $profilePath = Join-Path $coverageRoot "$($package.Name).cover.out"
    $importPath = Get-LocalNetworkImportPath $package
    foreach ($line in Read-LocalCoverageBody $profilePath) {
        if ((Get-LocalCoverageLinePackage $line) -cne $importPath) {
            throw "Fixed-path profile $($package.Name) contains a foreign package: $line"
        }
        $mergedLines.Add($line)
    }
}
$rootProfile = Join-Path $coverageRoot 'root.cover.out'
[IO.File]::WriteAllText(
    $rootProfile,
    "mode: atomic`n" + (($mergedLines -join "`n") + "`n"),
    [Text.UTF8Encoding]::new($false)
)

# The gate text must reach the console, so the verdict travels through the
# native exit code ($LASTEXITCODE survives the call) instead of the pipeline.
function Invoke-LocalCoverageGate([string]$Config, [string]$Profile, [string]$Label) {
    Write-Output ''
    Write-Output "==== $Label coverage gate ($Config) ===="
    Push-Location $repositoryRoot
    try {
        & go run $goTestCoverage --config=$Config --profile=$Profile
    } finally {
        Pop-Location
    }
}

Invoke-LocalCoverageGate 'core/.testcoverage.yml' $coreProfile 'core module'
$coreVerdict = $LASTEXITCODE -eq 0
Invoke-LocalCoverageGate '.testcoverage.yml' $rootProfile 'root module'
$rootVerdict = $LASTEXITCODE -eq 0

Write-Output ''
Write-Output ('core module coverage gate: ' + $(if ($coreVerdict) { 'PASS' } else { 'FAIL' }))
Write-Output ('root module coverage gate: ' + $(if ($rootVerdict) { 'PASS' } else { 'FAIL' }))
if (-not ($coreVerdict -and $rootVerdict)) {
    exit 1
}
