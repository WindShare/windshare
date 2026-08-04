[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$ArtifactRoot,

    [Parameter(Mandatory)]
    [string]$WorkRoot,

    [Parameter(Mandatory)]
    [string]$GoExecutable,

    [Parameter(Mandatory)]
    [string]$ExpectedUserSID
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (Test-Path Variable:PSNativeCommandUseErrorActionPreference) {
    $PSNativeCommandUseErrorActionPreference = $false
}

Import-Module (Join-Path $PSScriptRoot 'core-release-windows-native.psm1') -Force

# Native certification is itself a substantial osfs suite. Keep it on the same
# package bound as extracted-core validation so Go's default cannot preempt the
# release gate's explicit worker and workflow limits.
$coreSuiteTestTimeout = '30m'

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

$resolvedArtifactRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
    -Path $ArtifactRoot `
    -Label 'extracted artifact root'
$resolvedWorkRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
    -Path $WorkRoot `
    -Label 'native worker root'
if (-not [IO.Path]::IsPathFullyQualified($GoExecutable)) {
    throw 'Go executable must be an absolute path'
}
$resolvedGoExecutable = [IO.Path]::GetFullPath($GoExecutable)
$goExecutableInfo = [IO.FileInfo]::new($resolvedGoExecutable)
if ($null -eq $goExecutableInfo.Directory -or
    $null -eq $goExecutableInfo.Directory.Parent) {
    throw 'Go executable does not have a GOROOT parent'
}
$resolvedGoRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
    -Path $goExecutableInfo.Directory.Parent.FullName `
    -Label 'staged GOROOT'
Assert-WindowsNativeTreeHasNoReparsePoints `
    -RootPath $resolvedGoRoot `
    -Label 'staged GOROOT'
$expectedGoExecutable = Join-Path $resolvedGoRoot 'bin\go.exe'
if (-not (Test-Path -LiteralPath $expectedGoExecutable -PathType Leaf)) {
    throw 'staged GOROOT has no bin\go.exe'
}
$resolvedExpectedGoExecutable = [IO.Path]::GetFullPath(
    (Resolve-Path -LiteralPath $expectedGoExecutable).Path
)
if (-not [string]::Equals(
    $resolvedGoExecutable,
    $resolvedExpectedGoExecutable,
    [StringComparison]::OrdinalIgnoreCase
)) {
    throw 'Go executable is not the staged GOROOT bin\go.exe'
}
Assert-DisjointWindowsNativeRoots `
    -LeftPath $resolvedArtifactRoot `
    -LeftLabel 'extracted artifact root' `
    -RightPath $resolvedWorkRoot `
    -RightLabel 'native worker root'
Assert-DisjointWindowsNativeRoots `
    -LeftPath $resolvedArtifactRoot `
    -LeftLabel 'extracted artifact root' `
    -RightPath $resolvedGoRoot `
    -RightLabel 'staged GOROOT'
Assert-DisjointWindowsNativeRoots `
    -LeftPath $resolvedWorkRoot `
    -LeftLabel 'native worker root' `
    -RightPath $resolvedGoRoot `
    -RightLabel 'staged GOROOT'

$releaseRoot = [IO.Directory]::GetParent($resolvedArtifactRoot)
if ($null -eq $releaseRoot) {
    throw 'extracted artifact root has no protected release parent'
}
foreach ($rootPath in @($resolvedWorkRoot, $resolvedGoRoot)) {
    $rootParent = [IO.Directory]::GetParent($rootPath)
    if ($null -eq $rootParent -or -not [string]::Equals(
        $rootParent.FullName,
        $releaseRoot.FullName,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'artifact, worker, and staged GOROOT must be direct siblings in the protected release root'
    }
}
Assert-WindowsNativeReadOnlyDirectory `
    -Path $resolvedArtifactRoot `
    -Label 'extracted artifact root'
Assert-WindowsNativeReadOnlyDirectory `
    -Path $resolvedGoRoot `
    -Label 'staged GOROOT'
Write-Output '-- standard-user NTFS roots, artifact immutability, and staged GOROOT immutability verified'

$goModPath = Join-Path $resolvedArtifactRoot 'go.mod'
if (-not (Test-Path -LiteralPath $goModPath -PathType Leaf)) {
    throw 'extracted core artifact has no go.mod'
}
$goModStream = [IO.File]::OpenRead($goModPath)
$goModStream.Dispose()

$testTempRoot = Join-Path $resolvedWorkRoot 'test-temp'
$goBuildCache = Join-Path $resolvedWorkRoot 'go-build-cache'
$goModuleCache = Join-Path $resolvedWorkRoot 'go-module-cache'
$goPath = Join-Path $resolvedWorkRoot 'go-path'
$goTempRoot = Join-Path $resolvedWorkRoot 'go-temp'
Initialize-WritableDirectory $testTempRoot 'test temporary directory'
Initialize-WritableDirectory $goBuildCache 'Go build cache'
Initialize-WritableDirectory $goModuleCache 'Go module cache'
Initialize-WritableDirectory $goPath 'Go workspace cache'
Initialize-WritableDirectory $goTempRoot 'Go tool temporary directory'

$env:TEMP = $testTempRoot
$env:TMP = $testTempRoot
$env:GOCACHE = $goBuildCache
$env:GOMODCACHE = $goModuleCache
$env:GOPATH = $goPath
$env:GOTMPDIR = $goTempRoot
$env:GOENV = 'off'
$env:GOFLAGS = ''
$env:GOTOOLCHAIN = 'local'
$env:GOWORK = 'off'
$env:GOROOT = $resolvedGoRoot
$env:GOPROXY = 'https://proxy.golang.org'
$env:GOSUMDB = 'sum.golang.org'
$env:GOPRIVATE = ''
$env:GONOSUMDB = ''
$env:GONOPROXY = ''
$env:GOINSECURE = ''
$env:GOTELEMETRY = 'off'
# UseNewEnvironment blocks coordinator state but may expose machine-scope
# targeting defaults. Establish the native host contract explicitly here.
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOEXPERIMENT')) {
    if (Test-Path -LiteralPath "Env:$name") {
        Remove-Item -LiteralPath "Env:$name"
    }
}
$env:WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION = 'windows-ntfs'

$reportedGoRootOutput = @(& $resolvedGoExecutable env GOROOT)
$reportedGoRootExitCode = $LASTEXITCODE
$reportedGoRootValues = @($reportedGoRootOutput | ForEach-Object {
    ([string]$_).Trim()
} | Where-Object {
    -not [string]::IsNullOrWhiteSpace($_)
})
if ($reportedGoRootExitCode -ne 0 -or $reportedGoRootValues.Count -ne 1) {
    throw "staged GOROOT identity check failed with code $reportedGoRootExitCode"
}
$reportedGoRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
    -Path $reportedGoRootValues[0] `
    -Label 'Go-reported staged GOROOT'
if (-not [string]::Equals(
    $reportedGoRoot,
    $resolvedGoRoot,
    [StringComparison]::OrdinalIgnoreCase
)) {
    throw "Go reported GOROOT $reportedGoRoot, want staged root $resolvedGoRoot"
}
$goVersionOutput = @(& $resolvedGoExecutable version 2>&1)
$goVersionExitCode = $LASTEXITCODE
if ($goVersionExitCode -ne 0 -or $goVersionOutput.Count -eq 0) {
    throw "Go toolchain identity check failed with code $goVersionExitCode"
}
Write-Output "-- standard-user Go toolchain verified: $($goVersionOutput -join ' ')"

Set-Location $resolvedArtifactRoot
$requiredSelectors = @(Get-WindowsNativeRequiredTestSelectors)
for ($selectorIndex = 0; $selectorIndex -lt $requiredSelectors.Count; $selectorIndex++) {
    $selector = $requiredSelectors[$selectorIndex]
    $selectorPackage = [string]$selector.PackageArgument
    $selectorTests = @($selector.TestNames)
    $testExpression = Get-WindowsNativeRequiredTestExpression -TestNames $selectorTests
    $selectorLogID = '{0:D2}' -f ($selectorIndex + 1)
    $jsonLogPath = Join-Path $resolvedWorkRoot "native-test-events-$selectorLogID.jsonl"
    $goStderrPath = Join-Path $resolvedWorkRoot "native-test-stderr-$selectorLogID.log"
    $goArguments = @(
        'test', '-json', '-count=1', "-timeout=$coreSuiteTestTimeout",
        '-run', $testExpression, $selectorPackage
    )

    Write-Output "-- required Windows/NTFS native selector: package=$selectorPackage expression=$testExpression"
    $jsonLines = @(& $resolvedGoExecutable @goArguments 2> $goStderrPath)
    $nativeExitCode = $LASTEXITCODE
    $jsonLines | Set-Content -LiteralPath $jsonLogPath -Encoding utf8
    $jsonLines | ForEach-Object { Write-Output $_ }
    if (Test-Path -LiteralPath $goStderrPath) {
        Get-Content -LiteralPath $goStderrPath | ForEach-Object {
            Write-Output "[go test stderr] $_"
        }
    }

    $events = @(ConvertFrom-WindowsNativeTestJSONLines -Lines $jsonLines)
    Assert-WindowsNativeTestEvents `
        -ExitCode $nativeExitCode `
        -Events $events `
        -RequiredTests $selectorTests `
        -SelectorPackage $selectorPackage `
        -SelectorExpression $testExpression
}
Write-Output '-- required Windows/NTFS certification passed under the standard-user token'
