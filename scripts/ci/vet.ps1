# CI-parity vet gate (Windows). Mirrors:
#  - the vet analysis of ci.yml go-root and go-core; the native run below is
#    GOOS=windows, so it simultaneously covers ci.yml windows-tests' vet steps
#    (Windows-tagged files are analyzed, not just compiled).
#  - the ubuntu analysis path via a GOOS=linux cross-vet of both modules
#    (work-plan §10.2 cross-platform compile/type-check).
#  - ci.yml gowork-off-root: the released-core consumer build. The stronger
#    core invariant lives in the separate extracted-artifact `core-release`
#    gate, where no parent repository or go.work can mask a missing file.
#
# The plain same-GOOS `go build ./...` steps (root + core) are intentionally
# absent: `go vet` already compiles every package for analysis, the race and
# coverage gates recompile the identical code so any compile break surfaces
# there, and main-package linking is exercised by the D5 stable-children
# builds. Repeating a same-GOOS build here would be pure duplication; only the
# cross-GOOS vet and the root GOWORK=off consumer build below cover ground
# those gates cannot.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
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

Invoke-Step 'go vet (root, GOOS=windows)' { go vet ./... }
Invoke-Step 'go vet (core, GOOS=windows)' { go -C core vet ./... }

$originalGOOS = $env:GOOS
$env:GOOS = 'linux'
try {
    Invoke-Step 'go vet (root, GOOS=linux)' { go vet ./... }
    Invoke-Step 'go vet (core, GOOS=linux)' { go -C core vet ./... }
} finally {
    $env:GOOS = $originalGOOS
}

$originalGOWORK = $env:GOWORK
$env:GOWORK = 'off'
try {
    Invoke-Step 'GOWORK=off go build (root released-core consumer)' { go build ./... }
} finally {
    $env:GOWORK = $originalGOWORK
}

Write-Output ('== vet: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
