[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Import-Module (Join-Path $ciRoot 'hygiene/native-argument-batches.psm1') -Force
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Write-Output '== hygiene =='

$goFiles = @(
    git -c core.quotepath=false ls-files --cached --others --exclude-standard -- '*.go' |
        Where-Object { Test-Path -LiteralPath $_ -PathType Leaf }
)
if ($LASTEXITCODE -ne 0) {
    throw "git ls-files exited with code $LASTEXITCODE"
}

$gofmtBatches = @(Split-WindowsNativeArguments -Arguments $goFiles)
Write-Output "-- gofmt (tracked and untracked Go files; files=$($goFiles.Count); batches=$($gofmtBatches.Count))"
$unformatted = [Collections.Generic.List[string]]::new()
foreach ($batch in $gofmtBatches) {
    $batchOutput = @(& gofmt -l @($batch.Arguments))
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt exited with code $LASTEXITCODE"
    }
    foreach ($path in $batchOutput) {
        $unformatted.Add([string]$path)
    }
}
if ($unformatted.Count -ne 0) {
    $unformatted | Write-Output
    throw 'files need gofmt'
}

Invoke-Step 'whitespace' { git --no-pager diff --check }
Invoke-Step 'Web retired paths and production graph' { node scripts/ci/web-forbidden.mjs }
Invoke-Step 'Go retired paths and production graph' { node scripts/ci/go-v1-forbidden.mjs }

Write-Output ('== hygiene: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
