# The Windows public gate verifies a retained completion artifact; network and
# workload-identity authority never enter this process tree.
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
    # Public Make has no caller-owned path operand; the retained hosted launcher
    # supplies this variable only when it has locked the transferred artifact.
    $env:BROWSER_NETWORK_COMPLETION = [IO.Path]::GetFullPath(
        (Join-Path $repositoryRoot 'test-results/browser-network-completion.json'))
}
& node --experimental-strip-types scripts/ci/browsergate/network-completion.mjs consume
if ($LASTEXITCODE -ne 0) {
    throw "browser network completion consumer exited with code $LASTEXITCODE"
}
