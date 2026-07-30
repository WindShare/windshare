# The external network matrix is intentionally outside the blocking PR gate.
# With no explicit authority configuration this entry emits canonical
# unavailable/not-executed evidence without acquiring helpers or credentials.
[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$NetworkArguments
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
& node (Join-Path $repositoryRoot 'scripts\ci\browsergate\network-entry.mjs') @NetworkArguments
exit $LASTEXITCODE
