[CmdletBinding()]
param(
    [string]$Destination = (Join-Path $env:LOCALAPPDATA 'Programs\WindShare'),
    [string]$StateDirectory = (Join-Path $env:LOCALAPPDATA 'WindShare'),
    [ValidateSet('Ask', 'Configure', 'Skip')][string]$Firewall = 'Ask',
    [switch]$Uninstall
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'firewall.psm1') -Force
$sourceRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$destinationRoot = [IO.Path]::GetFullPath($Destination)
$executable = Join-Path $destinationRoot 'wind.exe'
$statusPath = Join-Path ([IO.Path]::GetFullPath($StateDirectory)) 'connectivity-setup.json'
if ($Uninstall) {
    $status = Invoke-WindShareFirewallSetup -Executable $executable -StatusPath $statusPath -Choice Remove
    if ($status.reason -ne 'rule-removed') { throw 'Remove the owned firewall rule from a permitted shell before uninstalling.' }
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
if ($Firewall -eq 'Ask') {
    if (Test-Path -LiteralPath $statusPath) {
        try {
            $previous = Get-Content -LiteralPath $statusPath -Raw | ConvertFrom-Json
            if ($previous.schema -eq 1 -and $previous.executable -ieq $executable -and
                $previous.state -in @('configured', 'declined', 'unavailable')) {
                Write-Output "Installed $executable. Kept first-setup decision: $($previous.state). Use -Firewall Configure to retry explicitly."
                return
            }
        } catch { Write-Warning 'Previous setup status is unreadable; select optional first setup again.' }
    }
    $answer = Read-Host 'Allow WindShare inbound UDP/TCP for direct connections? [y/N] (optional; requires a shell already permitted to manage firewall rules)'
    $Firewall = if ($answer -ieq 'y') { 'Configure' } else { 'Skip' }
}
$status = Invoke-WindShareFirewallSetup -Executable $executable -StatusPath $statusPath -Choice $Firewall
Write-Output "Installed $executable. Firewall: $($status.state) ($($status.reason)). Add $destinationRoot to PATH if desired."
