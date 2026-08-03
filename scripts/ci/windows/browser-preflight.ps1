# One Browsergate command owns PR contract and generated-semantic validation so
# hosted and focused runs cannot accidentally acquire separate evidence owners.
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot

& node scripts/ci/browsergate/main.mjs preflight
exit $LASTEXITCODE
