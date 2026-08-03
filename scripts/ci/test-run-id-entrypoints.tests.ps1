[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$callerRunID = 'entrypoint-contract-seed'
$hadRunID = Test-Path Env:WINDSHARE_TEST_RUN_ID
$previousRunID = $env:WINDSHARE_TEST_RUN_ID
$observedRunIDs = [Collections.Generic.List[string]]::new()
$entrypoints = @(
    [pscustomobject]@{ Suite = 'check'; Path = 'scripts/ci/windows/check.ps1' },
    [pscustomobject]@{ Suite = 'race'; Path = 'scripts/ci/windows/race.ps1' },
    [pscustomobject]@{ Suite = 'coverage'; Path = 'scripts/ci/windows/coverage.ps1' },
    [pscustomobject]@{ Suite = 'integration'; Path = 'scripts/ci/windows/integration.ps1' },
    [pscustomobject]@{ Suite = 'e2e-go'; Path = 'scripts/ci/windows/e2e-go.ps1' },
    [pscustomobject]@{ Suite = 'browser-process'; Path = 'scripts/ci/windows/browser-process.ps1' },
    [pscustomobject]@{ Suite = 'browser-smoke'; Path = 'scripts/ci/windows/browser/smoke.ps1' }
)

try {
    function global:Assert-WindShareGoAuthorityActive {}
    Import-Module (Join-Path $PSScriptRoot 'test-run-id.psm1') -Force
    $env:WINDSHARE_TEST_RUN_ID = $callerRunID

    foreach ($entrypoint in $entrypoints) {
        $source = [IO.File]::ReadAllText((Join-Path $repositoryRoot $entrypoint.Path))
        $invocation = "Invoke-WithWindShareTestRunID -Suite '$($entrypoint.Suite)'"
        if ([regex]::Matches($source, [regex]::Escape($invocation)).Count -ne 1) {
            throw "$($entrypoint.Path) must enter exactly one run-ID scope for $($entrypoint.Suite)"
        }
        if ($source.Contains('New-WindShareTestRunID', [StringComparison]::Ordinal)) {
            throw "$($entrypoint.Path) must not duplicate run-ID construction outside its scope authority"
        }

        $observedRunIDs.Clear()
        Invoke-WithWindShareTestRunID -Suite $entrypoint.Suite -Body {
            param([string]$RunID)
            $observedRunIDs.Add($env:WINDSHARE_TEST_RUN_ID)
            if ($RunID -cne $env:WINDSHARE_TEST_RUN_ID) {
                throw 'run-ID scope argument diverged from its environment identity'
            }
        }
        if ($env:WINDSHARE_TEST_RUN_ID -cne $callerRunID) {
            throw "$($entrypoint.Suite) did not restore the caller run ID after success"
        }
        $expectedPattern = "^$callerRunID-$($entrypoint.Suite)-[a-f0-9]{32}$"
        if ($observedRunIDs.Count -ne 1 -or $observedRunIDs[0] -notmatch $expectedPattern) {
            throw "$($entrypoint.Suite) did not propagate one invocation-owned run ID"
        }

        $failed = $false
        try {
            Invoke-WithWindShareTestRunID -Suite $entrypoint.Suite -Body {
                throw 'synthetic child failure'
            }
        } catch {
            $failed = $true
        }
        if (-not $failed) {
            throw "$($entrypoint.Suite) did not surface a child failure"
        }
        if ($env:WINDSHARE_TEST_RUN_ID -cne $callerRunID) {
            throw "$($entrypoint.Suite) did not restore the caller run ID after failure"
        }
    }
} finally {
    Remove-Module test-run-id -ErrorAction SilentlyContinue
    Remove-Item Function:Assert-WindShareGoAuthorityActive -ErrorAction SilentlyContinue
    if ($hadRunID) {
        $env:WINDSHARE_TEST_RUN_ID = $previousRunID
    } else {
        Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
    }
}

Write-Output 'test run-ID entrypoint contracts: PASS'
