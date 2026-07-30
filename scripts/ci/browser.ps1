# The cross-platform orchestrator owns ordering and final-status reduction so a
# failed main suite cannot prevent Pion evidence, guards, or the verdict. On
# Windows it enters the D5 lease once; firewall prompts and WBEM state are never
# inspected because neither is evidence about browser correctness.
[CmdletBinding()]
param(
    [switch]$Plan,
    [switch]$SkipDependencyInstall,
    [string]$OutputRoot = '',
    [string]$Profile = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot

$arguments = @(
    (Join-Path $repositoryRoot 'scripts\ci\browsergate\main.mjs'),
    'local'
)
if ($Plan) {
    $arguments += '--plan'
}
if ($SkipDependencyInstall) {
    $arguments += '--skip-dependency-install'
}
if (-not [string]::IsNullOrWhiteSpace($OutputRoot)) {
    $arguments += @('--output-root', [IO.Path]::GetFullPath($OutputRoot))
}
if (-not [string]::IsNullOrWhiteSpace($Profile)) {
    $arguments += @('--profile', [IO.Path]::GetFullPath($Profile))
}

& node @arguments
exit $LASTEXITCODE
