[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$helperPath = Join-Path $PSScriptRoot 'test-run-id.psm1'
$helperSource = [IO.File]::ReadAllText($helperPath)
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

function Assert-Fails {
    param(
        [Parameter(Mandatory)][scriptblock]$Action,
        [Parameter(Mandatory)][string]$Label
    )

    $failed = $false
    try {
        & $Action
    } catch {
        $failed = $true
    }
    if (-not $failed) {
        throw "$Label did not fail"
    }
}

try {
    foreach ($retiredToken in @(
        'WindShareStabilityHelperSemantics',
        'stability-helper-semantics',
        'Assert-WindShareGoAuthorityActive'
    )) {
        if ($helperSource.Contains($retiredToken, [StringComparison]::Ordinal)) {
            throw "test-run-id.psm1 retains retired control-plane coupling: $retiredToken"
        }
    }

    Import-Module $helperPath -Force
    $env:WINDSHARE_TEST_RUN_ID = $callerRunID

    $directRunID = New-WindShareTestRunID -Suite 'check'
    if ($directRunID -notmatch "^$callerRunID-check-[a-f0-9]{32}$") {
        throw 'direct run-ID construction did not preserve the validated seed and 128-bit suffix'
    }
    if ($env:WINDSHARE_TEST_RUN_ID -cne $callerRunID) {
        throw 'pure run-ID construction mutated the caller environment'
    }

    $env:WINDSHARE_TEST_RUN_ID = 'x'
    $singleCharacterSeedRunID = New-WindShareTestRunID -Suite 'check'
    if ($singleCharacterSeedRunID -notmatch '^x-check-[a-f0-9]{32}$') {
        throw 'a one-character portable seed was rejected or rewritten'
    }
    $env:WINDSHARE_TEST_RUN_ID = '.invalid'
    Assert-Fails -Label 'edge-punctuation seed' -Action {
        New-WindShareTestRunID -Suite 'check' | Out-Null
    }
    $env:WINDSHARE_TEST_RUN_ID = $callerRunID

    foreach ($entrypoint in $entrypoints) {
        $source = [IO.File]::ReadAllText((Join-Path $repositoryRoot $entrypoint.Path))
        $invocation = "Invoke-WithWindShareTestRunID -Suite '$($entrypoint.Suite)'"
        if ([regex]::Matches($source, [regex]::Escape($invocation)).Count -ne 1) {
            throw "$($entrypoint.Path) must enter exactly one run-ID scope for $($entrypoint.Suite)"
        }
        if ($source.Contains('New-WindShareTestRunID', [StringComparison]::Ordinal)) {
            throw "$($entrypoint.Path) must not duplicate run-ID construction outside the shared helper"
        }
        foreach ($retiredToken in @(
            'goauthority/authority.psm1',
            'Enter-WindShareGoAuthority',
            'Invoke-WindShareGo'
        )) {
            if ($source.Contains($retiredToken, [StringComparison]::Ordinal)) {
                throw "$($entrypoint.Path) retains retired Go control-plane coupling: $retiredToken"
            }
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

        Assert-Fails -Label "$($entrypoint.Suite) child failure" -Action {
            Invoke-WithWindShareTestRunID -Suite $entrypoint.Suite -Body {
                throw 'synthetic child failure'
            }
        }
        if ($env:WINDSHARE_TEST_RUN_ID -cne $callerRunID) {
            throw "$($entrypoint.Suite) did not restore the caller run ID after failure"
        }
    }

    $integrationSource = [IO.File]::ReadAllText(
        (Join-Path $repositoryRoot 'scripts/ci/windows/integration.ps1')
    )
    $integrationTest = 'go test -json -count=1 ./integration/...'
    if ([regex]::Matches(
        $integrationSource,
        '(?m)^\s*go\s+test\s+-json\b'
    ).Count -ne 1 -or
        [regex]::Matches($integrationSource, [regex]::Escape($integrationTest)).Count -ne 1) {
        throw 'Windows integration must invoke exactly one local go test -json execution'
    }
    $scopeIndex = $integrationSource.IndexOf(
        "Invoke-WithWindShareTestRunID -Suite 'integration'",
        [StringComparison]::Ordinal
    )
    $startedIndex = $integrationSource.IndexOf(
        'node scripts/ci/stability/result.mjs started',
        [StringComparison]::Ordinal
    )
    $secretCleanupIndex = $integrationSource.IndexOf(
        'Remove-Item Env:WINDSHARE_STABILITY_START_REQUEST',
        [StringComparison]::Ordinal
    )
    $testIndex = $integrationSource.IndexOf($integrationTest, [StringComparison]::Ordinal)
    if ($scopeIndex -lt 0 -or $startedIndex -le $scopeIndex -or
        $secretCleanupIndex -le $startedIndex -or $testIndex -le $secretCleanupIndex) {
        throw 'Windows integration must settle run identity before publishing the authenticated start event'
    }

    Remove-Item Env:WINDSHARE_TEST_RUN_ID
    Invoke-WithWindShareTestRunID -Suite 'check' -Body {
        if ($env:WINDSHARE_TEST_RUN_ID -notmatch '^local-check-[a-f0-9]{32}$') {
            throw 'unset caller scope did not receive a local run ID'
        }
    }
    if (Test-Path Env:WINDSHARE_TEST_RUN_ID) {
        throw 'successful local scope did not restore an unset caller run ID'
    }
    Assert-Fails -Label 'unset caller child failure' -Action {
        Invoke-WithWindShareTestRunID -Suite 'check' -Body {
            throw 'synthetic child failure'
        }
    }
    if (Test-Path Env:WINDSHARE_TEST_RUN_ID) {
        throw 'failed local scope did not restore an unset caller run ID'
    }
} finally {
    Remove-Module test-run-id -ErrorAction SilentlyContinue
    if ($hadRunID) {
        $env:WINDSHARE_TEST_RUN_ID = $previousRunID
    } else {
        Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
    }
}

Write-Output 'test run-ID PowerShell entrypoint contracts: PASS'
