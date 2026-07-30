[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
& node scripts/ci/browsergate/main.mjs local --run-policy stability
exit $LASTEXITCODE
