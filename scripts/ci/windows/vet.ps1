# CI-parity vet gate (Windows). Mirrors the current-commit Windows authority:
#  - native GOOS=windows vet analysis of both modules.
#  - the Linux authority via a GOOS=linux cross-vet of both modules.
#  - the released-core consumer build formerly isolated in its own workflow job. The stronger
#    core invariant lives in the separate extracted-artifact `core-release`
#    gate, where no parent repository or go.work can mask a missing file.
#
# The plain same-GOOS `go build ./...` steps (root + core) are intentionally
# absent: `go vet` already compiles every package for analysis, the race and
# coverage gates recompile the identical code so any compile break surfaces
# there, and main-package linking is exercised by the process and E2E fixture
# builds. Repeating a same-GOOS build here would be pure duplication; only the
# cross-GOOS vet and the root GOWORK=off consumer build below cover ground
# those gates cannot.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
$null = Import-Module (Join-Path $ciRoot 'goauthority/authority.psm1') -Force
$goAuthority = Enter-WindShareGoAuthority
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== vet =='

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Invoke-Step 'go vet (root, native)' { Invoke-WindShareGo vet ./... }
Invoke-Step 'go vet (core, native)' { Invoke-WindShareGo -C core vet ./... }

$originalGOOS = $env:GOOS
$env:GOOS = 'linux'
try {
    Invoke-Step 'go vet (root, GOOS=linux)' { Invoke-WindShareGo vet ./... }
    Invoke-Step 'go vet (core, GOOS=linux)' { Invoke-WindShareGo -C core vet ./... }
} finally {
    $env:GOOS = $originalGOOS
}

$originalGOWORK = $env:GOWORK
$env:GOWORK = 'off'
try {
    Invoke-Step 'GOWORK=off go build (root released-core consumer)' { Invoke-WindShareGo build ./... }
} finally {
    $env:GOWORK = $originalGOWORK
}

Write-Output ('== vet: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
