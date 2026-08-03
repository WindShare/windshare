# CI-parity hygiene gate (Windows). Mirrors ci.yml job `hygiene`
# (sloc-guard is standalone): formatting, thin dispatch, native run identity,
# stability and exact-SHA release-readiness contracts, argument batching,
# source-only product/security scans, and gopls diagnostics.
# Local gofmt also inspects untracked Go files so pre-commit runs catch new sources.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ciRoot = Split-Path -Parent $PSScriptRoot
Import-Module (Join-Path $ciRoot 'hygiene/native-argument-batches.psm1') -Force

function Invoke-HygieneNativeStep {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Step,
        [Parameter(Mandatory)][scriptblock]$Action
    )

    & $Action
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        throw "hygiene native step failed (step=$Step; exit_code=$exitCode)"
    }
}

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
$goplsVersion = (Get-Content -LiteralPath (Join-Path $ciRoot 'gopls.version') -Raw).Trim()
if ($goplsVersion -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') {
    throw "gopls.version is not a canonical release version: $goplsVersion"
}
$goplsModule = "golang.org/x/tools/gopls@$goplsVersion"
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== hygiene =='

$gofmtFiles = @(
    git -c core.quotepath=false ls-files --cached --others --exclude-standard -- '*.go' |
        Where-Object { Test-Path -LiteralPath $_ -PathType Leaf }
)
if ($LASTEXITCODE -ne 0) {
    throw "git ls-files exited with code $LASTEXITCODE"
}
# gofmt with an empty argument list would block reading stdin. Batching also
# mirrors xargs on Linux while staying below the Windows command-line ceiling.
$gofmtBatches = @(Split-WindowsNativeArguments -Arguments $gofmtFiles)
Write-Output (
    '-- gofmt (tracked + untracked Go files; files={0}; batches={1})' -f
        $gofmtFiles.Count, $gofmtBatches.Count
)
$unformatted = [Collections.Generic.List[string]]::new()
$gofmtBatchIndex = 0
foreach ($batch in $gofmtBatches) {
    $gofmtBatchIndex++
    $batchArguments = @($batch.Arguments)
    $batchOutput = @(& gofmt -l @batchArguments)
    if ($LASTEXITCODE -ne 0) {
        throw (
            'gofmt exited with code {0} (batch={1}/{2}; files={3})' -f
                $LASTEXITCODE, $gofmtBatchIndex, $gofmtBatches.Count, $batchArguments.Count
        )
    }
    foreach ($path in $batchOutput) {
        $unformatted.Add([string]$path)
    }
}
if ($unformatted.Count -gt 0) {
    $unformatted | Write-Output
    throw 'files need gofmt'
}

Write-Output '-- whitespace (git diff --check against the empty tree)'
$emptyTree = @() | git hash-object -t tree --stdin
if ($LASTEXITCODE -ne 0) {
    throw "git hash-object exited with code $LASTEXITCODE"
}
# --no-pager: in an interactive terminal git would otherwise hand the diff to
# less and park the whole gate on a keypress; gate scripts must never page.
git --no-pager diff --check $emptyTree
if ($LASTEXITCODE -ne 0) {
    throw 'git diff --check reported whitespace errors'
}

Write-Output '-- thin CI dispatch contract'
Invoke-HygieneNativeStep -Step 'ci-contract-tests' -Action { node scripts/ci/contract.tests.mjs }
Invoke-HygieneNativeStep -Step 'ci-contract' -Action { node scripts/ci/contract.mjs }

Write-Output '-- native run-ID contract'
Invoke-HygieneNativeStep -Step 'test-run-id-entrypoint-contracts' -Action {
    pwsh -NoProfile -File scripts/ci/test-run-id-entrypoints.tests.ps1
}

Write-Output '-- stability evidence contracts'
Invoke-HygieneNativeStep -Step 'stability-result-contracts' -Action {
    node scripts/ci/stability/result.tests.mjs
}
Invoke-HygieneNativeStep -Step 'stability-release-reducer-contracts' -Action {
    node scripts/ci/stability/release-reducer.tests.mjs
}
Invoke-HygieneNativeStep -Step 'stability-workflow-contracts' -Action {
    node --test scripts/ci/stability/workflow.tests.mjs
}

Write-Output '-- exact-SHA release-readiness contracts'
Invoke-HygieneNativeStep -Step 'release-readiness-resolver-contracts' -Action {
    node --test scripts/ci/release-readiness/resolver.tests.mjs
}
Invoke-HygieneNativeStep -Step 'release-readiness-verifier-contracts' -Action {
    node --test scripts/ci/release-readiness/verifier.tests.mjs
}
Invoke-HygieneNativeStep -Step 'release-readiness-workflow-contracts' -Action {
    node --test scripts/ci/release-readiness/workflow.tests.mjs
}

Write-Output '-- Windows native argument batching contract'
Invoke-HygieneNativeStep -Step 'windows-native-argument-contracts' -Action {
    pwsh -NoProfile -File scripts/ci/hygiene/native-argument-batches.tests.ps1
}

Write-Output '-- Web v1 forbidden references (source-only)'
Invoke-HygieneNativeStep -Step 'web-v1-forbidden' -Action {
    node scripts/ci/web-forbidden.mjs --source-only
}

Write-Output '-- Go v1 forbidden roots and production dependencies'
Invoke-HygieneNativeStep -Step 'go-v1-forbidden' -Action {
    node scripts/ci/go-v1-forbidden.mjs
}

$trackedGoFiles = @(
    git -c core.quotepath=false ls-files -- '*.go' |
        Where-Object { Test-Path -LiteralPath $_ -PathType Leaf }
)
if ($LASTEXITCODE -ne 0) {
    throw "git ls-files exited with code $LASTEXITCODE"
}
# CI fails on non-zero exit (pipefail) or any stdout diagnostic; mirror both
# across every Windows-safe argument batch.
$goplsBatches = @(Split-WindowsNativeArguments -Arguments $trackedGoFiles)
Write-Output (
    '-- gopls check (version={0}; severity=hint; tracked-files={1}; batches={2})' -f
        $goplsVersion, $trackedGoFiles.Count, $goplsBatches.Count
)
$goplsDiagnosticCount = 0
$goplsBatchIndex = 0
foreach ($batch in $goplsBatches) {
    $goplsBatchIndex++
    $batchArguments = @($batch.Arguments)
    $batchDiagnostics = @(go run $goplsModule check -severity=hint @batchArguments)
    $goplsExitCode = $LASTEXITCODE
    $batchDiagnostics | Write-Output
    $goplsDiagnosticCount += $batchDiagnostics.Count
    if ($goplsExitCode -ne 0) {
        throw (
            'gopls exited with code {0} (batch={1}/{2}; files={3})' -f
                $goplsExitCode, $goplsBatchIndex, $goplsBatches.Count, $batchArguments.Count
        )
    }
}
if ($goplsDiagnosticCount -gt 0) {
    throw 'gopls reported diagnostics'
}

Write-Output ('== hygiene: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
