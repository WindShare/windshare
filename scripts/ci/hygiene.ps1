# CI-parity hygiene gate (Windows). Mirrors ci.yml jobs `hygiene` + `sloc`:
#  - gofmt over tracked AND untracked Go files (work-plan §10.1: pre-commit
#    runs must catch new sources; CI checks tracked only because a clean
#    checkout has no untracked files).
#  - whitespace: `git diff --check` against the empty tree, so every tracked
#    file's worktree content is inspected — on a clean tree this equals CI's
#    committed-tree diff.
#  - gopls check -severity=hint over tracked Go files (the AGENTS.md
#    GOPLS_CHECK command; CI pins gopls@v0.22.0, locally PATH's gopls is used).
#  - sloc-guard check (CI installs the latest release via the sloc-guard
#    action; locally the binary is a PATH prerequisite).
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== hygiene =='

Write-Output '-- gofmt (tracked + untracked Go files)'
$gofmtFiles = @(git -c core.quotepath=false ls-files --cached --others --exclude-standard -- '*.go')
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

Write-Output '-- gopls check (severity=hint, tracked Go files)'
$trackedGoFiles = @(git -c core.quotepath=false ls-files -- '*.go')
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

Write-Output '-- sloc-guard check'
sloc-guard.exe check
if ($LASTEXITCODE -ne 0) {
    throw "sloc-guard check exited with code $LASTEXITCODE"
}

Write-Output ('== hygiene: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
