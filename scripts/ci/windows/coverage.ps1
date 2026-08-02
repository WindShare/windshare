# Coverage is one ordinary sweep per Go module. Keeping profiles outside the
# repository prevents a failed verdict from dirtying the developer's worktree.
# The root sweep owns integration/E2E, so one gate run ID spans their packages.
# JSON mode keeps passing scenario evidence visible in that single instrumented
# sweep instead of rerunning the packages without coverage.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
Import-Module (Join-Path $ciRoot 'goauthority/authority.psm1') -Force
$null = Enter-WindShareGoAuthority
Import-Module (Join-Path $ciRoot 'test-run-id.psm1') -Force
$hadRunID = Test-Path Env:WINDSHARE_TEST_RUN_ID
$previousRunID = $env:WINDSHARE_TEST_RUN_ID
$runID = New-WindShareTestRunID -Suite 'coverage'
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
$coverageTool = 'github.com/vladopajic/go-test-coverage/v2@v2.18.8'
# A fixed native-suite budget keeps platform parity independent of caller state.
$coreSuiteTestTimeout = '30m'
$temporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$profileDirectory = [IO.Path]::GetFullPath(
    (Join-Path $temporaryRoot ("windshare-coverage-{0}" -f [guid]::NewGuid().ToString('N')))
)
if (-not $profileDirectory.StartsWith($temporaryRoot, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Coverage profile directory escaped the temporary root: $profileDirectory"
}
[IO.Directory]::CreateDirectory($profileDirectory) | Out-Null
$rootProfile = Join-Path $profileDirectory 'root.cover.out'
$coreProfile = Join-Path $profileDirectory 'core.cover.out'
function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

try {
    $env:WINDSHARE_TEST_RUN_ID = $runID
    Write-Output "== coverage: run_id=$runID =="
    try {
        Invoke-Step 'root module coverage tests' {
            Invoke-WindShareGoTestJSON -count=1 ./... -covermode=atomic "-coverprofile=$rootProfile"
        }
        Invoke-Step 'root coverage gate (total >=80%, package >=70%)' {
            Invoke-WindShareGo run $coverageTool --config=.testcoverage.yml "--profile=$rootProfile"
        }
        Invoke-Step 'core module coverage tests' {
            Invoke-WindShareGo -C core test -count=1 "-timeout=$coreSuiteTestTimeout" ./... `
                -covermode=atomic "-coverprofile=$coreProfile"
        }
        Invoke-Step 'core coverage gate (total >=90%, package >=70%)' {
            Push-Location core
            try {
                Invoke-WindShareGo run $coverageTool --config=.testcoverage.yml "--profile=$coreProfile"
            } finally {
                Pop-Location
            }
        }
        Write-Output ('== coverage: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
    } finally {
        if (Test-Path -LiteralPath $profileDirectory -PathType Container) {
            Remove-Item -LiteralPath $profileDirectory -Recurse -Force
        }
    }
} finally {
    if ($hadRunID) {
        $env:WINDSHARE_TEST_RUN_ID = $previousRunID
    } else {
        Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
    }
}
