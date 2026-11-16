param(
    [Parameter(Mandatory)][string]$Directory,
    [string]$SigningThumbprint = $env:WINDSHARE_SIGNING_THUMBPRINT
)
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSHOME 'Modules/Microsoft.PowerShell.Security/Microsoft.PowerShell.Security.psd1')
$Directory = (Resolve-Path -LiteralPath $Directory).Path
$metadata = Get-Content -LiteralPath (Join-Path $Directory 'identity/identity.json') -Raw | ConvertFrom-Json
$sdkBase = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits/10/bin'
$sdk = Get-ChildItem -LiteralPath $sdkBase -Directory | Where-Object { $_.Name -match '^10\.0\.\d+\.0$' } |
    Sort-Object { [version]$_.Name } -Descending | Select-Object -First 1
if (!$sdk) { throw 'Windows SDK packaging tools are required' }
$tools = Join-Path $sdk.FullName 'x64'
function Invoke-Sdk([string]$Tool, [string[]]$Arguments) {
    & (Join-Path $tools $Tool) @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Tool failed with exit code $LASTEXITCODE" }
}
# Preserve GPUI's Common Controls, OS compatibility and heap declarations. Identity
# augments the executable's runtime manifest; replacing it prevents Windows loading it.
$baseline = Join-Path $Directory 'runtime.manifest'
try {
    Invoke-Sdk 'mt.exe' @('-nologo', "-inputresource:$(Join-Path $Directory 'windshare.exe');#1", "-out:$baseline")
    Invoke-Sdk 'mt.exe' @('-nologo', '-manifest', $baseline, (Join-Path $Directory 'identity/windshare.manifest'), "-outputresource:$(Join-Path $Directory 'windshare.exe');#1")
} finally {
    if (Test-Path -LiteralPath $baseline) { Remove-Item -LiteralPath $baseline }
}
# Sparse packages resolve visual resources from the external content root, not the
# identity manifest directory. The index must live beside the executable and Assets.
$identityDirectory = Join-Path $Directory 'identity'
$resourceConfig = Join-Path $Directory 'priconfig.xml'
$resourceFiles = Join-Path $Directory 'resources.resfiles'
try {
    Invoke-Sdk 'makepri.exe' @('createconfig', '/cf', $resourceConfig, '/dq', 'en-US', '/pv', '10.0.0', '/o')
    $resourceConfiguration = [xml](Get-Content -LiteralPath $resourceConfig -Raw)
    # Explicit relative paths preserve the manifest's Assets namespace and exclude
    # binaries or old indexes. All themes and sizes belong in one runtime index.
    [void]$resourceConfiguration.resources.RemoveChild($resourceConfiguration.resources.packaging)
    $resourceConfiguration.resources.index.startIndexAt = 'resources.resfiles'
    $fileIndexer = $resourceConfiguration.CreateElement('indexer-config')
    $fileIndexer.SetAttribute('type', 'resfiles')
    $fileIndexer.SetAttribute('qualifierDelimiter', '.')
    [void]$resourceConfiguration.resources.index.AppendChild($fileIndexer)
    $resourceConfiguration.Save($resourceConfig)
    Get-ChildItem -LiteralPath (Join-Path $Directory 'Assets') -Filter '*.png' -File |
        Sort-Object Name | ForEach-Object { 'Assets\' + $_.Name } |
        Set-Content -LiteralPath $resourceFiles -Encoding utf8
    Invoke-Sdk 'makepri.exe' @('new', '/pr', $Directory, '/mn', (Join-Path $identityDirectory 'AppxManifest.xml'), '/cf', $resourceConfig, '/of', (Join-Path $Directory 'resources.pri'), '/o')
} finally {
    foreach ($temporary in @($resourceConfig, $resourceFiles)) {
        if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary }
    }
}
$developmentCertificate = $null
try {
    if (!$SigningThumbprint) {
        if ($metadata.channel -ne 'development') { throw 'Release packaging requires WINDSHARE_SIGNING_THUMBPRINT' }
        $developmentCertificate = New-SelfSignedCertificate -Type Custom -Subject $metadata.publisher -KeyUsage DigitalSignature -KeyExportPolicy NonExportable -CertStoreLocation 'Cert:\CurrentUser\My' -NotAfter (Get-Date).AddYears(1) -TextExtension @('2.5.29.37={text}1.3.6.1.5.5.7.3.3')
        $SigningThumbprint = $developmentCertificate.Thumbprint
        Export-Certificate -Cert $developmentCertificate -FilePath (Join-Path $Directory 'development.cer') | Out-Null
    }
    $certificate = Get-Item -LiteralPath "Cert:\CurrentUser\My\$SigningThumbprint"
    if ($certificate.Subject -ne $metadata.publisher) { throw 'Signing certificate does not match the package publisher' }
    foreach ($binary in @('windshare.exe', 'windshare_host.dll', 'windshare_shell.dll')) {
        Invoke-Sdk 'signtool.exe' @('sign', '/fd', 'SHA256', '/sha1', $SigningThumbprint, '/s', 'My', (Join-Path $Directory $binary))
    }
    # External binaries are intentionally absent from the identity package.
    Invoke-Sdk 'makeappx.exe' @('pack', '/o', '/nv', '/d', (Join-Path $Directory 'identity'), '/p', (Join-Path $Directory 'identity.msix'))
    Invoke-Sdk 'signtool.exe' @('sign', '/fd', 'SHA256', '/sha1', $SigningThumbprint, '/s', 'My', (Join-Path $Directory 'identity.msix'))
} finally {
    if ($developmentCertificate) { Remove-Item -LiteralPath "Cert:\CurrentUser\My\$($developmentCertificate.Thumbprint)" }
}
