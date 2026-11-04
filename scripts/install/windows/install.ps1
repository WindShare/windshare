[CmdletBinding()]
param(
    [string]$Destination = (Join-Path $env:LOCALAPPDATA 'Programs\WindShare'),
    [string]$StateDirectory = (Join-Path $env:LOCALAPPDATA 'WindShare'),
    [ValidateSet('Ask', 'Configure', 'Skip')][string]$Firewall = 'Ask',
    [switch]$Uninstall
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'network.psm1') -Force
$sourceRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$destinationRoot = [IO.Path]::GetFullPath($Destination)
$executable = Join-Path $destinationRoot 'wind.exe'
if ($Uninstall) {
    Invoke-WindShareNetworkSetup -Executable $executable -StateDirectory $StateDirectory -Choice Remove
    if (Test-Path -LiteralPath $executable) { Remove-Item -LiteralPath $executable -Force }
    return
}
[IO.Directory]::CreateDirectory($destinationRoot) | Out-Null
$binary = Join-Path $sourceRoot 'wind.exe'
# Source identity takes precedence over untracked executables left by local builds.
if (Test-Path -LiteralPath (Join-Path $sourceRoot 'go.mod')) {
    Push-Location $sourceRoot
    $previousWorkspace = $env:GOWORK
    try {
        $env:GOWORK = 'off'
        & go run ./scripts/ci/_piondeps
        if ($LASTEXITCODE -ne 0) { throw 'Pinned dependency verification failed.' }
        & go build -trimpath -buildvcs=false -o $executable ./cmd/wind
        if ($LASTEXITCODE -ne 0) { throw 'WindShare build failed.' }
    } finally { $env:GOWORK = $previousWorkspace; Pop-Location }
} elseif (Test-Path -LiteralPath $binary -PathType Leaf) {
    Copy-Item -LiteralPath $binary -Destination $executable -Force
} else {
    throw 'Expected a complete source checkout/source bundle or a release binary bundle.'
}
try { Invoke-WindShareNetworkSetup -Executable $executable -StateDirectory $StateDirectory -Choice $Firewall }
catch { Write-Warning "Optional network setup: $($_.Exception.Message) Run wind doctor network to inspect current policy." }
Write-Output "Installed $executable. Add $destinationRoot to PATH if desired."
