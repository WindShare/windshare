# Browsergate owns orchestration and final reduction; this file is only the
# native Make boundary and deliberately forwards the supported local options.
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

& node @arguments
exit $LASTEXITCODE
