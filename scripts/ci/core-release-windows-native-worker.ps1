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
$directorySeparator = [IO.Path]::DirectorySeparatorChar
if ([string]::Equals($resolvedArtifactRoot, $resolvedWorkRoot, [StringComparison]::OrdinalIgnoreCase) -or
    $resolvedArtifactRoot.StartsWith($resolvedWorkRoot + $directorySeparator, [StringComparison]::OrdinalIgnoreCase) -or
    $resolvedWorkRoot.StartsWith($resolvedArtifactRoot + $directorySeparator, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'extracted artifact and writable native worker roots must be disjoint'
}
Assert-WindowsNativeReadOnlyDirectory `
    -Path $resolvedArtifactRoot `
    -Label 'extracted artifact root'
Write-Output '-- standard-user NTFS roots and artifact write denial verified'

if (-not [IO.Path]::IsPathFullyQualified($GoExecutable)) {
    throw 'Go executable must be an absolute path'
}
$resolvedGoExecutable = [IO.Path]::GetFullPath($GoExecutable)
if (-not (Test-Path -LiteralPath $resolvedGoExecutable -PathType Leaf)) {
    throw 'Go executable is not an absolute existing file'
}
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

$goVersionOutput = @(& $resolvedGoExecutable version 2>&1)
$goVersionExitCode = $LASTEXITCODE
if ($goVersionExitCode -ne 0 -or $goVersionOutput.Count -eq 0) {
    throw "Go toolchain identity check failed with code $goVersionExitCode"
}
Write-Output "-- standard-user Go toolchain verified: $($goVersionOutput -join ' ')"

$jsonLogPath = Join-Path $resolvedWorkRoot 'native-test-events.jsonl'
$goStderrPath = Join-Path $resolvedWorkRoot 'native-test-stderr.log'
$testExpression = Get-WindowsNativeRequiredTestExpression
$goArguments = @(
    'test', '-json', '-count=1', "-timeout=$coreSuiteTestTimeout",
    '-run', $testExpression, './osfs'
)
Set-Location $resolvedArtifactRoot
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
    -RequiredTests (Get-WindowsNativeRequiredTestNames)
Write-Output '-- required Windows/NTFS certification passed under the standard-user token'
