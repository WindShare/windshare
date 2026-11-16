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
