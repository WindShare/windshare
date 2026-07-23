# CI-parity hygiene gate (Windows). Mirrors ci.yml job `hygiene`
# (sloc-guard lives in the standalone `sloc` gate since 2026-07-14):
#  - gofmt over tracked AND untracked Go files (work-plan §10.1: pre-commit
#    runs must catch new sources; CI checks tracked only because a clean
#    checkout has no untracked files).
#  - whitespace: `git diff --check` against the empty tree, so every tracked
#    file's worktree content is inspected — on a clean tree this equals CI's
#    committed-tree diff.
#  - source-only Go/Web v1 forbidden-reference scans (the Web gate also checks
#    the built bundle).
#  - short PowerShell contracts for R8 benchmark parsing, command transcripts,
#    source checkpoints, schema validation, and atomic evidence publication.
#  - gopls check -severity=hint over tracked Go files (the AGENTS.md
#    GOPLS_CHECK command; CI pins gopls@v0.22.0, locally PATH's gopls is used).
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== hygiene =='

Write-Output '-- gofmt (tracked + untracked Go files)'
$gofmtFiles = @(
    git -c core.quotepath=false ls-files --cached --others --exclude-standard -- '*.go' |
        Where-Object { Test-Path -LiteralPath $_ -PathType Leaf }
)
if ($LASTEXITCODE -ne 0) {
    throw "git ls-files exited with code $LASTEXITCODE"
}
# gofmt with an empty argument list would block reading stdin.
$unformatted = @()
if ($gofmtFiles.Count -gt 0) {
    $unformatted = @(& gofmt -l @gofmtFiles)
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt exited with code $LASTEXITCODE"
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

Write-Output '-- Web v1 forbidden references (source-only)'
node scripts/ci/web-forbidden.mjs --source-only
if ($LASTEXITCODE -ne 0) {
    throw 'Web v1 forbidden-reference gate failed'
}

Write-Output '-- Go v1 forbidden roots and production dependencies'
node scripts/ci/go-v1-forbidden.mjs
if ($LASTEXITCODE -ne 0) {
    throw 'Go v1 forbidden-reference gate failed'
}

Write-Output '-- R8 performance evidence contracts'
$r8EvidenceSuites = @(
    'scripts/go-benchmark-evidence.tests.ps1',
    'scripts/local-coverage.tests.ps1',
    'scripts/r8-go-evidence.tests.ps1',
    'scripts/r8-evidence-summary.tests.ps1',
    'scripts/r8-web-performance-authority.tests.ps1'
)
foreach ($suite in $r8EvidenceSuites) {
    & pwsh -NoProfile -File $suite
    if ($LASTEXITCODE -ne 0) {
        throw "R8 performance evidence contract failed: $suite"
    }
}

Write-Output '-- gopls check (severity=hint, tracked Go files)'
$trackedGoFiles = @(
    git -c core.quotepath=false ls-files -- '*.go' |
        Where-Object { Test-Path -LiteralPath $_ -PathType Leaf }
)
if ($LASTEXITCODE -ne 0) {
    throw "git ls-files exited with code $LASTEXITCODE"
}
# CI fails on non-zero exit (pipefail) or any stdout diagnostic; mirror both.
$diagnostics = @(& gopls check -severity=hint @trackedGoFiles)
$goplsExitCode = $LASTEXITCODE
$diagnostics | Write-Output
if ($goplsExitCode -ne 0 -or $diagnostics.Count -gt 0) {
    throw 'gopls reported diagnostics'
}

Write-Output ('== hygiene: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
