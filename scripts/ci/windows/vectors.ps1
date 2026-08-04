[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
$vectorRoot = 'core/testvectors'
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

Write-Output '== vectors =='
Invoke-Step 'regenerate protocol-contract vectors' {
    go -C core test -count=1 ./internal/protocolcontract -update
}
Invoke-Step 'regenerate peer-signaling vectors' {
    go test -count=1 ./connectivity/v2signal -update
}

Write-Output '-- compare vector inventory'
$expected = @(
    Get-Content -LiteralPath (Join-Path $vectorRoot 'inventory.txt') |
        ForEach-Object { $_.Trim() } |
        Where-Object { $_ -ne '' -and -not $_.StartsWith('#') } |
        Sort-Object
)
$actual = @(
    Get-ChildItem -LiteralPath $vectorRoot -File -Filter '*.json' |
        Select-Object -ExpandProperty Name |
        Sort-Object
)
$inventoryDrift = @(Compare-Object $expected $actual)
if ($inventoryDrift.Count -ne 0) {
    $inventoryDrift | Format-Table -AutoSize | Out-String | Write-Output
    throw "$vectorRoot/inventory.txt does not match the JSON vector inventory"
}

Write-Output '-- compare regenerated vectors with the worktree'
$status = @(git -c core.quotepath=false status --short -- $vectorRoot)
if ($LASTEXITCODE -ne 0) {
    throw "git status exited with code $LASTEXITCODE"
}
if ($status.Count -ne 0) {
    $status | Write-Output
    throw "regenerated vectors differ from committed $vectorRoot/"
}

Write-Output ('== vectors: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
