# The cross-platform orchestrator owns ordering and final-status reduction so a
# failed main suite cannot prevent Pion evidence, guards, or the verdict.
# Firewall prompts and WBEM state are never inspected because neither is
# evidence about browser correctness.
[CmdletBinding()]
param(
    [switch]$Plan,
    [string]$OutputRoot = '',
    [string]$Profile = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
Set-Location $repositoryRoot
Import-Module (Join-Path $ciRoot 'goauthority/authority.psm1') -Force
$null = Enter-WindShareGoAuthority

$arguments = @(
    (Join-Path $repositoryRoot 'scripts\ci\browsergate\main.mjs'),
    'local'
)
if ($Plan) {
    $arguments += '--plan'
}
if (-not [string]::IsNullOrWhiteSpace($OutputRoot)) {
    $arguments += @('--output-root', [IO.Path]::GetFullPath($OutputRoot))
}
if (-not [string]::IsNullOrWhiteSpace($Profile)) {
    $arguments += @('--profile', [IO.Path]::GetFullPath($Profile))
}

Invoke-WindShareGoConsumer node @arguments
exit $LASTEXITCODE
