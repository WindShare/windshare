# CI-parity golden-vector idempotence gate (Windows). Mirrors ci.yml
# golden-vectors / work-plan §10.3: regenerate every vector family twice; the
# two regenerations must be byte-identical (Get-FileHash stands in for CI's
# sha256sum) and must exactly match the committed testvectors/.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== vectors =='

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

function Update-VectorFamilies([string]$Pass) {
    Invoke-Step "regenerate core/share vectors ($Pass)" { go -C core test -count=1 ./share -update }
    Invoke-Step "regenerate relay/protocol vectors ($Pass)" { go test -count=1 ./relay/protocol -update }
}

function Get-VectorHashes {
    return @(Get-FileHash -Algorithm SHA256 -Path 'testvectors/*.json' | Sort-Object Path)
}

Update-VectorFamilies 'first pass'
$firstHashes = Get-VectorHashes
Update-VectorFamilies 'second pass'
$secondHashes = Get-VectorHashes

Write-Output '-- regenerations must be byte-identical'
$drift = @(Compare-Object $firstHashes $secondHashes -Property Path, Hash)
if ($drift.Count -gt 0) {
    $drift | Format-Table -AutoSize | Out-String | Write-Output
    throw 'vector regeneration is not idempotent: hashes differ between passes'
}

Write-Output '-- committed vectors must match regeneration'
$status = @(git -c core.quotepath=false status --short -- testvectors)
if ($LASTEXITCODE -ne 0) {
    throw "git status exited with code $LASTEXITCODE"
}
if ($status.Count -gt 0) {
    Write-Output 'regenerated vectors differ from committed testvectors/:'
    $status | Write-Output
    throw 'committed vectors do not match regeneration'
}

Write-Output ('== vectors: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
