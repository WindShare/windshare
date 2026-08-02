[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$entrypoints = @('check', 'race', 'coverage', 'integration', 'e2e-go', 'browser-process')
$windowsEntrypointRoot = Join-Path $PSScriptRoot 'windows'
$callerRunID = 'entrypoint-contract-seed'
$hadRunID = Test-Path Env:WINDSHARE_TEST_RUN_ID
$previousRunID = $env:WINDSHARE_TEST_RUN_ID
$observedRunIDs = [Collections.Generic.List[string]]::new()

try {
    $env:WINDSHARE_TEST_RUN_ID = $callerRunID
    function go {
        $observedRunIDs.Add($env:WINDSHARE_TEST_RUN_ID)
        $global:LASTEXITCODE = 0
    }
    function pnpm {
        $observedRunIDs.Add($env:WINDSHARE_TEST_RUN_ID)
        $global:LASTEXITCODE = 0
    }

    foreach ($entrypoint in $entrypoints) {
        $observedRunIDs.Clear()
        . (Join-Path $windowsEntrypointRoot "$entrypoint.ps1") | Out-Null
        if ($env:WINDSHARE_TEST_RUN_ID -ne $callerRunID) {
            throw "$entrypoint did not restore the caller run ID after success"
        }
        $distinctRunIDs = @($observedRunIDs | Sort-Object -Unique)
        $expectedPattern = "^$callerRunID-$entrypoint-[a-f0-9]{32}$"
        if ($distinctRunIDs.Count -ne 1 -or $distinctRunIDs[0] -notmatch $expectedPattern) {
            throw "$entrypoint did not propagate one invocation-owned run ID"
        }
    }

    function go { $global:LASTEXITCODE = 17 }
    function pnpm { $global:LASTEXITCODE = 17 }
    foreach ($entrypoint in $entrypoints) {
        $failed = $false
        try {
            . (Join-Path $windowsEntrypointRoot "$entrypoint.ps1") | Out-Null
        } catch {
            $failed = $true
        }
        if (-not $failed) {
            throw "$entrypoint did not surface a child command failure"
        }
        if ($env:WINDSHARE_TEST_RUN_ID -ne $callerRunID) {
            throw "$entrypoint did not restore the caller run ID after failure"
        }
    }
} finally {
    Remove-Item Function:go -ErrorAction SilentlyContinue
    Remove-Item Function:pnpm -ErrorAction SilentlyContinue
    if ($hadRunID) {
        $env:WINDSHARE_TEST_RUN_ID = $previousRunID
    } else {
        Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
    }
}

Write-Output 'test run-ID entrypoint contracts: PASS'
