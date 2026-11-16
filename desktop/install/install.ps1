param(
    [switch]$DevelopmentRegistration,
    [ValidateRange(0, 3600)][int]$WaitTimeoutSeconds = 300
)
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSHOME 'Modules/Microsoft.PowerShell.Security/Microsoft.PowerShell.Security.psd1')
. (Join-Path $PSScriptRoot 'lifetime.ps1')
$metadata = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'identity/identity.json') -Raw | ConvertFrom-Json
$root = Get-WindShareInstallRoot $metadata.channel
$package = Join-Path $PSScriptRoot 'identity.msix'
if ($DevelopmentRegistration -and $metadata.channel -ne 'development') {
    throw 'Development registration cannot be used for a release package'
}
$hash = (Get-FileHash -LiteralPath $package -Algorithm SHA256).Hash.Substring(0, 12).ToLowerInvariant()
$destination = Join-Path $root "$($metadata.version)-$hash"
$existing = Get-AppxPackage -Name $metadata.name
if ($existing -and [version]$existing.Version -gt [version]$metadata.version) {
    throw 'A newer WindShare package is already installed'
}
Wait-WindShareIdle $root $WaitTimeoutSeconds
# Publish complete immutable directories so retries cannot reuse partial copies or
# overwrite binaries referenced by the currently registered package.
if (-not (Test-Path -LiteralPath $destination)) {
    $staging = Join-Path $root ("install-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $staging -Force | Out-Null
    try {
        foreach ($name in @('windshare.exe', 'windshare_host.dll', 'windshare_shell.dll', 'windshare.ico', 'resources.pri', 'identity.msix', 'LICENSE', 'README.txt', 'install.ps1', 'uninstall.ps1', 'lifetime.ps1')) {
            Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination (Join-Path $staging $name)
        }
        foreach ($name in @('identity', 'Assets')) {
            Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $staging -Recurse
        }
        $resolvedStaging = [IO.Path]::GetFullPath($staging)
        $resolvedDestination = [IO.Path]::GetFullPath($destination)
        $prefix = [IO.Path]::GetFullPath($root).TrimEnd('\') + '\'
        if (-not $resolvedStaging.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase) -or
            -not $resolvedDestination.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'Installation path escaped the WindShare directory'
        }
        Move-Item -LiteralPath $resolvedStaging -Destination $resolvedDestination
    } finally {
        if (Test-Path -LiteralPath $staging) {
            $resolved = [IO.Path]::GetFullPath($staging)
            $prefix = [IO.Path]::GetFullPath($root).TrimEnd('\') + '\'
            if (-not $resolved.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
                throw 'Staging cleanup escaped the WindShare directory'
            }
            Remove-Item -LiteralPath $resolved -Recurse -Force
        }
    }
}
# Registration is the final switch; the prior package keeps its complete binary set.
if ($DevelopmentRegistration) {
    # Developer mode must already be enabled. Installation never changes machine trust.
    Add-AppxPackage -Register (Join-Path $destination 'identity/AppxManifest.xml') -ExternalLocation $destination
} else {
    Add-AppxPackage -Path (Join-Path $destination 'identity.msix') -ExternalLocation $destination
}
$check = Start-Process -FilePath (Join-Path $destination 'windshare.exe') -ArgumentList '--identity-check' -WindowStyle Hidden -PassThru -Wait
if ($check.ExitCode -ne 0) { throw 'Installed package identity check failed' }
Write-Host "Installed WindShare at $destination"
