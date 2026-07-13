# CI-parity lint gate (Windows). Mirrors ci.yml job `lint`: golangci-lint v2
# over both Go modules (root, then core), governed by the one root
# .golangci.yml each run discovers upward. The tool is version-pinned and
# executed via `go run` — the GO_TEST_COVERAGE precedent — so local and CI
# always run the identical linter version with no PATH prerequisite; the Go
# build cache absorbs the one-time source build.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Same pin as ci.yml env GOLANGCI_LINT and scripts/ci/lint.sh; bump together.
$golangciLint = 'github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== lint =='

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Invoke-Step 'golangci-lint (root)' { go run $golangciLint run ./... }
Invoke-Step 'golangci-lint (core)' { go -C core run $golangciLint run ./... }

Write-Output ('== lint: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
