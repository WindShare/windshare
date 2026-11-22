[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$BinariesRoot,

    [Parameter(Mandatory)]
    [string]$WorkRoot,

    [Parameter(Mandatory)]
    [string]$ExpectedUserSID
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (Test-Path Variable:PSNativeCommandUseErrorActionPreference) {
    $PSNativeCommandUseErrorActionPreference = $false
}

Import-Module (Join-Path $PSScriptRoot 'certify.psm1') -Force

# Native certification runs precompiled test binaries under an unprivileged
# token, mirroring Linux loop-ext4 certification without duplicating GOROOT.
$moduleSuiteTestTimeout = '30m'

function Assert-WritableExistingDirectory([string]$Path, [string]$Label) {
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "$Label does not exist or is not a directory"
    }
    $probePath = Join-Path $Path ('access-{0}.probe' -f [Guid]::NewGuid().ToString('N'))
    try {
        [IO.File]::WriteAllText($probePath, 'windshare-native-gate')
        $probeContents = [IO.File]::ReadAllText($probePath)
        if ($probeContents -cne 'windshare-native-gate') {
            throw "$Label did not preserve the access probe"
        }
    } finally {
        if (Test-Path -LiteralPath $probePath) {
            Remove-Item -LiteralPath $probePath -Force
        }
    }
}

function Initialize-WritableDirectory([string]$Path, [string]$Label) {
    New-Item -ItemType Directory -Path $Path | Out-Null
    Assert-WritableExistingDirectory -Path $Path -Label $Label
}

function Assert-DisjointWindowsNativeRoots(
    [string]$LeftPath,
    [string]$LeftLabel,
    [string]$RightPath,
    [string]$RightLabel
) {
    $directorySeparator = [IO.Path]::DirectorySeparatorChar
    if ([string]::Equals($LeftPath, $RightPath, [StringComparison]::OrdinalIgnoreCase) -or
        $LeftPath.StartsWith($RightPath + $directorySeparator, [StringComparison]::OrdinalIgnoreCase) -or
        $RightPath.StartsWith($LeftPath + $directorySeparator, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$LeftLabel and $RightLabel must be disjoint"
    }
}

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
$groupSIDs = @($identity.Groups | ForEach-Object { $_.Value })
$isAdministrator = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
Assert-WindowsNativeStandardUserIdentity `
    -ActualUserSID $identity.User.Value `
    -ExpectedUserSID $ExpectedUserSID `
    -GroupSIDs $groupSIDs `
    -IsAdministrator $isAdministrator
Write-Output '-- standard-user token identity verified'

$userProfile = [Environment]::GetFolderPath([Environment+SpecialFolder]::UserProfile)
$resolvedUserProfile = Resolve-WindowsNativeLocalFixedNTFSDirectory `
    -Path $userProfile `
    -Label 'standard-user profile'
Assert-WritableExistingDirectory `
    -Path $resolvedUserProfile `
    -Label 'standard-user profile'
$profileVolumeRoot = [IO.Path]::GetPathRoot($resolvedUserProfile)
$homeDrive = $profileVolumeRoot.TrimEnd(
    [IO.Path]::DirectorySeparatorChar,
    [IO.Path]::AltDirectorySeparatorChar
)
if ([string]::IsNullOrWhiteSpace($homeDrive)) {
    throw 'standard-user profile has no local home drive'
}
$homePath = $resolvedUserProfile.Substring($homeDrive.Length)
if (-not $homePath.StartsWith(
    [string][IO.Path]::DirectorySeparatorChar,
    [StringComparison]::Ordinal
)) {
    $homePath = [IO.Path]::DirectorySeparatorChar + $homePath
}
$identityNameParts = $identity.Name.Split([char]'\', 2)
if ($identityNameParts.Count -eq 2) {
    $env:USERDOMAIN = $identityNameParts[0]
    $env:USERDOMAIN_ROAMINGPROFILE = $identityNameParts[0]
    $env:USERNAME = $identityNameParts[1]
} else {
    $env:USERNAME = $identity.Name
}
$env:USERPROFILE = $resolvedUserProfile
$env:HOME = $resolvedUserProfile
$env:HOMEDRIVE = $homeDrive
$env:HOMEPATH = $homePath
Write-Output '-- standard-user profile and identity environment verified'

$resolvedBinariesRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
    -Path $BinariesRoot `
    -Label 'native test binaries root'
$resolvedWorkRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
    -Path $WorkRoot `
    -Label 'native worker root'
Assert-WindowsNativeTreeHasNoReparsePoints `
    -RootPath $resolvedBinariesRoot `
    -Label 'native test binaries root'
Assert-DisjointWindowsNativeRoots `
    -LeftPath $resolvedBinariesRoot `
    -LeftLabel 'native test binaries root' `
    -RightPath $resolvedWorkRoot `
    -RightLabel 'native worker root'

$binariesParent = [IO.Directory]::GetParent($resolvedBinariesRoot)
$workParent = [IO.Directory]::GetParent($resolvedWorkRoot)
if ($null -eq $binariesParent -or $null -eq $workParent -or -not [string]::Equals(
    $binariesParent.FullName,
    $workParent.FullName,
    [StringComparison]::OrdinalIgnoreCase
)) {
    throw 'native test binaries and worker root must be direct siblings in the protected release root'
}
Assert-WindowsNativeReadOnlyDirectory `
    -Path $resolvedBinariesRoot `
    -Label 'native test binaries root'
Write-Output '-- standard-user NTFS roots and precompiled test binary immutability verified'

$testTempRoot = Join-Path $resolvedWorkRoot 'test-temp'
Initialize-WritableDirectory $testTempRoot 'test temporary directory'
$env:TEMP = $testTempRoot
$env:TMP = $testTempRoot

$requiredSelectors = @(Get-WindowsNativeRequiredTestSelectors)
$firstFailureExitCode = 0
for ($selectorIndex = 0; $selectorIndex -lt $requiredSelectors.Count; $selectorIndex++) {
    $selector = $requiredSelectors[$selectorIndex]
    $selectorPackage = [string]$selector.PackageArgument
    $selectorBinaryName = [string]$selector.BinaryName
    $selectorTests = @($selector.TestNames)
    $testExpression = Get-WindowsNativeRequiredTestExpression -TestNames $selectorTests
    $binaryPath = Join-Path $resolvedBinariesRoot $selectorBinaryName
    if (-not (Test-Path -LiteralPath $binaryPath -PathType Leaf)) {
        throw "native test binary is missing: $binaryPath"
    }
    $selectorLogID = '{0:D2}' -f ($selectorIndex + 1)
    $rawOutputPath = Join-Path $resolvedWorkRoot "native-test-raw-$selectorLogID.log"
    $stderrPath = Join-Path $resolvedWorkRoot "native-test-stderr-$selectorLogID.log"
    $exitCodePath = Join-Path $resolvedWorkRoot "native-test-exit-$selectorLogID.txt"

    Write-Output "-- required Windows/NTFS native selector: package=$selectorPackage binary=$selectorBinaryName expression=$testExpression"
    $binaryExitCode = Invoke-WindowsNativeTestBinary `
        -BinaryPath $binaryPath `
        -WorkingDirectory $testTempRoot `
        -Arguments @(
            '-test.v=test2json',
            '-test.count=1',
            "-test.timeout=$moduleSuiteTestTimeout",
            "-test.run=$testExpression"
        ) `
        -StdoutPath $rawOutputPath `
        -StderrPath $stderrPath
    [IO.File]::WriteAllText($exitCodePath, [string]$binaryExitCode, [Text.UTF8Encoding]::new($false))
    if ((Test-Path -LiteralPath $stderrPath) -and ([IO.FileInfo]::new($stderrPath)).Length -gt 0) {
        Get-Content -LiteralPath $stderrPath | ForEach-Object {
            Write-Output "[native test stderr] $_"
        }
    }
    if ($binaryExitCode -ne 0 -and $firstFailureExitCode -eq 0) {
        $firstFailureExitCode = $binaryExitCode
    }
}
if ($firstFailureExitCode -ne 0) {
    exit $firstFailureExitCode
}
Write-Output '-- required Windows/NTFS precompiled native tests completed under the standard-user token'
