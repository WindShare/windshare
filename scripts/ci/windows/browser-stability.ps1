[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
Import-Module (Join-Path $ciRoot 'goauthority/authority.psm1') -Force
$null = Enter-WindShareGoAuthority
Invoke-WindShareGoConsumer node scripts/ci/browsergate/main.mjs local --run-policy stability
exit $LASTEXITCODE
