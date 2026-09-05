[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Import-Module (Join-Path $ciRoot 'hygiene/native-argument-batches.psm1') -Force
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

# Both launchers use the verified dependency boundary before opening maintained
# sources in their owning modules, preserving the root provider replacements.
$trackedGoFiles = @(go run ./scripts/ci/_piondeps -maintained-go-files)
if ($LASTEXITCODE -ne 0) {
    throw "maintained Go source selection exited with code $LASTEXITCODE"
}

$batches = @(Split-WindowsNativeArguments -Arguments $trackedGoFiles)
$diagnostics = [Collections.Generic.List[string]]::new()
Write-Output "== gopls: tracked-files=$($trackedGoFiles.Count); batches=$($batches.Count) =="
foreach ($batch in $batches) {
    $batchDiagnostics = @(gopls check -severity=hint @($batch.Arguments))
    if ($LASTEXITCODE -ne 0) {
        throw "gopls check exited with code $LASTEXITCODE"
    }
    foreach ($diagnostic in $batchDiagnostics) {
        $diagnostics.Add([string]$diagnostic)
    }
}

if ($diagnostics.Count -ne 0) {
    $diagnostics | Write-Output
    throw 'gopls reported diagnostics'
}

Write-Output ('== gopls: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
