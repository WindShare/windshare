# The public browser graph consumes only immutable completion evidence. The
# protected workflow supplies its target SHA; focused local consumption derives
# the checked-out commit so both paths bind the same consumer API.
[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments)]
    [string[]]$UnexpectedArguments = @()
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($UnexpectedArguments.Count -ne 0) {
    throw 'browser-network.ps1 does not accept positional operands'
}

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
if ([string]::IsNullOrWhiteSpace($env:BROWSER_NETWORK_COMPLETION)) {
    $env:BROWSER_NETWORK_COMPLETION = [IO.Path]::GetFullPath(
        (Join-Path $repositoryRoot 'test-results/browser-network-completion.json'))
}

$targetShaSource = 'environment'
if ([string]::IsNullOrWhiteSpace($env:WINDSHARE_TARGET_SHA)) {
    $resolvedTargetSha = (& git rev-parse --verify HEAD | Select-Object -First 1)
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($resolvedTargetSha)) {
        throw 'cannot resolve the browser network target SHA from the checkout'
    }
    $env:WINDSHARE_TARGET_SHA = $resolvedTargetSha.Trim()
    $targetShaSource = 'checkout'
}
if ($env:WINDSHARE_TARGET_SHA -cnotmatch '^[0-9a-f]{40}$') {
    throw 'browser network target SHA must be an exact lowercase commit identity'
}

Write-Output "== browser-network: target_sha=$($env:WINDSHARE_TARGET_SHA) source=$targetShaSource =="
& node --experimental-strip-types scripts/ci/browsergate/network-completion.mjs consume
if ($LASTEXITCODE -ne 0) {
    throw "browser network completion consumer exited with code $LASTEXITCODE"
}
