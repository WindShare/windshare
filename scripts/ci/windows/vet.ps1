# Windows preflight proves the native source set can both link and pass vet.
# Linux owns its own tagged source set and the sole released-core consumer build,
# so keeping those checks here would create duplicate owners rather than evidence.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
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

Invoke-Step 'go build (root, native)' { go build ./... }
Invoke-Step 'go build (core, native)' { go -C core build ./... }
Invoke-Step 'go vet (root, native)' { go vet ./... }
Invoke-Step 'go vet (core, native)' { go -C core vet ./... }

Write-Output ('== vet: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
