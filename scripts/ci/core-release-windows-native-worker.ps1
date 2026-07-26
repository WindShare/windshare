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

function Resolve-RequiredDirectory([string]$Path, [string]$Label) {
    if (-not [IO.Path]::IsPathFullyQualified($Path)) {
        throw "$Label must be an absolute path"
    }
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "$Label does not exist or is not a directory"
    }
    return [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $Path).Path)
}

function Assert-LocalFixedNTFSPath([string]$Path, [string]$Label) {
    $volumeRoot = [IO.Path]::GetPathRoot($Path)
    if ([string]::IsNullOrWhiteSpace($volumeRoot)) {
        throw "$Label has no volume root"
    }
    $drive = [IO.DriveInfo]::new($volumeRoot)
    if (-not $drive.IsReady -or $drive.DriveType -ne [IO.DriveType]::Fixed) {
        throw "$Label must be on a ready local fixed drive"
    }
    if (-not [string]::Equals($drive.DriveFormat, 'NTFS', [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label must be on NTFS, found $($drive.DriveFormat)"
    }

    $currentDirectory = [IO.DirectoryInfo]::new($Path)
    while ($null -ne $currentDirectory) {
        $attributes = [IO.File]::GetAttributes($currentDirectory.FullName)
        if (($attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "$Label ancestry contains a reparse point: $($currentDirectory.FullName)"
        }
        $currentDirectory = $currentDirectory.Parent
    }
}

function Assert-WritableDirectory([string]$Path, [string]$Label) {
    New-Item -ItemType Directory -Path $Path -Force | Out-Null
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

$resolvedArtifactRoot = Resolve-RequiredDirectory $ArtifactRoot 'extracted artifact root'
$resolvedWorkRoot = Resolve-RequiredDirectory $WorkRoot 'native worker root'
Assert-LocalFixedNTFSPath $resolvedArtifactRoot 'extracted artifact root'
Assert-LocalFixedNTFSPath $resolvedWorkRoot 'native worker root'
$directorySeparator = [IO.Path]::DirectorySeparatorChar
if ([string]::Equals($resolvedArtifactRoot, $resolvedWorkRoot, [StringComparison]::OrdinalIgnoreCase) -or
    $resolvedArtifactRoot.StartsWith($resolvedWorkRoot + $directorySeparator, [StringComparison]::OrdinalIgnoreCase) -or
    $resolvedWorkRoot.StartsWith($resolvedArtifactRoot + $directorySeparator, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'extracted artifact and writable native worker roots must be disjoint'
}
Assert-WindowsNativeReadOnlyDirectory `
    -Path $resolvedArtifactRoot `
    -Label 'extracted artifact root'
Write-Output '-- extracted artifact write denial verified'

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
Assert-WritableDirectory $testTempRoot 'test temporary directory'
Assert-WritableDirectory $goBuildCache 'Go build cache'
Assert-WritableDirectory $goModuleCache 'Go module cache'
Assert-WritableDirectory $goPath 'Go workspace cache'

$env:TEMP = $testTempRoot
$env:TMP = $testTempRoot
$env:GOCACHE = $goBuildCache
$env:GOMODCACHE = $goModuleCache
$env:GOPATH = $goPath
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
# Start-Process with another credential is not an environment-inheritance
# contract. Establish host targeting explicitly inside the worker.
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOEXPERIMENT')) {
    if (Test-Path -LiteralPath "Env:$name") {
        Remove-Item -LiteralPath "Env:$name"
    }
}
$env:WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION = 'windows-ntfs'

$jsonLogPath = Join-Path $resolvedWorkRoot 'native-test-events.jsonl'
$goStderrPath = Join-Path $resolvedWorkRoot 'native-test-stderr.log'
$testExpression = Get-WindowsNativeRequiredTestExpression
$goArguments = @('test', '-json', '-count=1', '-run', $testExpression, './osfs')
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
