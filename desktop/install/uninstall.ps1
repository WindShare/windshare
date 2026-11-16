param([ValidateRange(0, 3600)][int]$WaitTimeoutSeconds = 300)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lifetime.ps1')
$metadata = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'identity/identity.json') -Raw | ConvertFrom-Json
$root = Get-WindShareInstallRoot $metadata.channel
Wait-WindShareIdle $root $WaitTimeoutSeconds
Get-AppxPackage -Name $metadata.name | Remove-AppxPackage
Remove-WindShareDirectory $root $root
Write-Host 'Removed WindShare and its Explorer / Windows Share registrations. Saved sharing settings are retained.'
