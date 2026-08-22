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
Invoke-Step 'Web production graph resolver tests' { node --test scripts/ci/web-forbidden.tests.mjs }
Invoke-Step 'Browser FSA reviewed support artifact syntax' {
    $evidenceScripts = @(Get-ChildItem 'web/scripts/browser-evidence-review/fsa-resumable-zip' -Recurse -File -Filter '*.mjs')
    foreach ($script in $evidenceScripts) {
        & node --check $script.FullName
        if ($LASTEXITCODE -ne 0) { throw "node --check failed for $($script.FullName)" }
    }
}
Invoke-Step 'Browser FSA reviewed support artifacts' {
    $evidenceTests = @(Get-ChildItem 'web/scripts/browser-evidence-review/fsa-resumable-zip/tests' -File -Filter '*.test.mjs')
    & node --test @($evidenceTests.FullName)
    if ($LASTEXITCODE -ne 0) { throw 'Browser FSA reviewed support artifact tests failed' }
    & node web/scripts/browser-evidence-review/fsa-resumable-zip/review.mjs | Out-Null
}
Invoke-Step 'Browser workspace ZIP review JavaScript syntax' {
    $evidenceScripts = @(Get-ChildItem 'web/scripts/browser-evidence/workspace-zip-recommendation' -Recurse -File -Filter '*.mjs')
    foreach ($script in $evidenceScripts) {
        & node --check $script.FullName
        if ($LASTEXITCODE -ne 0) { throw "node --check failed for $($script.FullName)" }
    }
}
Invoke-Step 'Browser workspace ZIP review contracts' {
    $evidenceTests = @(Get-ChildItem 'web/scripts/browser-evidence/workspace-zip-recommendation/tests' -File -Filter '*.test.mjs')
    & node --test @($evidenceTests.FullName)
}
Invoke-Step 'Frozen Unicode Go tables' { node scripts/unicode15/generate-go.mjs --check }
Invoke-Step 'Web retired paths and production graph' { node scripts/ci/web-forbidden.mjs }
Invoke-Step 'Go retired paths and production graph' { node scripts/ci/go-v1-forbidden.mjs }
Invoke-Step 'Core production dependency boundary tests' { go test ./scripts/ci/_coreboundary }
Invoke-Step 'Core production dependency boundary' { go run ./scripts/ci/_coreboundary }
Invoke-Step 'Go validation package ownership tests' { go test ./scripts/ci/_gopackages }
Invoke-Step 'Go validation package ownership' {
    go run ./scripts/ci/_gopackages -set all | Out-Null
}
Invoke-Step 'Named Go test suite selection tests' { go test ./scripts/ci/_gotestsuite }

Write-Output ('== hygiene: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
