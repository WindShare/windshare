[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$artifactDirectory = Join-Path $repositoryRoot 'dist/deploy/wsrelay'
$artifactPath = Join-Path $artifactDirectory 'wsrelay-linux-amd64'
$manifestPath = Join-Path $artifactDirectory 'manifest.json'
$operationID = '{0}-{1}' -f (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ'), $PID
$targetOperatingSystem = 'linux'
$targetArchitecture = 'amd64'
$modulePath = 'github.com/windshare/windshare'
$relayPackage = './relay/cmd/wsrelay'

function Invoke-NativeCommand {
    param(
        [Parameter(Mandatory)] [string] $FilePath,
        [Parameter(Mandatory)] [string[]] $Arguments
    )

    $global:LASTEXITCODE = 0
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$FilePath exited with code $LASTEXITCODE"
    }
}

function Set-ProcessEnvironment {
    param(
        [Parameter(Mandatory)] [hashtable] $Previous,
        [Parameter(Mandatory)] [hashtable] $Next
    )

    foreach ($name in $Next.Keys) {
        $Previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $Next[$name], 'Process')
    }
}

$previousLocation = Get-Location
$previousEnvironment = @{}
$binaryCandidate = $null
$manifestCandidate = $null

try {
    Set-Location $repositoryRoot
    Set-ProcessEnvironment -Previous $previousEnvironment -Next @{
        GOTOOLCHAIN = 'local'
        GOWORK = 'off'
    }

    Write-Output "wsrelay_build operation_id=$operationID milestone=tests_started"
    Invoke-NativeCommand -FilePath 'go' -Arguments @('test', '-count=1', './relay/...')

    # Deriving source paths from the compiler graph keeps the manifest accurate
    # when the relay starts consuming another in-repository package.
    $directoryTemplate = '{{if .Module}}{{if eq .Module.Path "' + $modulePath + '"}}{{.Dir}}{{end}}{{end}}'
    $packageDirectories = @(
        Invoke-NativeCommand -FilePath 'go' -Arguments @(
            'list', '-deps', '-f', $directoryTemplate, $relayPackage
        )
    ) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }

    $sourcePaths = [Collections.Generic.List[string]]::new()
    $sourcePaths.Add('go.mod')
    $sourcePaths.Add('go.sum')
    foreach ($directory in $packageDirectories) {
        $relative = [IO.Path]::GetRelativePath($repositoryRoot, $directory).Replace('\', '/')
        if ($relative -ne '.') {
            $sourcePaths.Add($relative)
        }
    }
    $sourcePaths = @($sourcePaths | Sort-Object -Unique)
    $statusArguments = @('status', '--porcelain=v1', '--untracked-files=all', '--') + $sourcePaths
    $sourceStatus = @(Invoke-NativeCommand -FilePath 'git' -Arguments $statusArguments)

    $revision = (@(
        Invoke-NativeCommand -FilePath 'git' -Arguments @('rev-parse', 'HEAD')
    )[-1]).Trim()
    if ($revision -notmatch '^[0-9a-f]{40}$') {
        throw "git returned an invalid revision: $revision"
    }

    [IO.Directory]::CreateDirectory($artifactDirectory) | Out-Null
    $candidateSuffix = [Guid]::NewGuid().ToString('N')
    $binaryCandidate = "$artifactPath.$candidateSuffix.tmp"
    $manifestCandidate = "$manifestPath.$candidateSuffix.tmp"

    Set-ProcessEnvironment -Previous $previousEnvironment -Next @{
        CGO_ENABLED = '0'
        GOARCH = $targetArchitecture
        GOOS = $targetOperatingSystem
    }
    Write-Output "wsrelay_build operation_id=$operationID milestone=build_started revision=$revision"
    Invoke-NativeCommand -FilePath 'go' -Arguments @(
        'build', '-trimpath', '-ldflags', '-s -w', '-o', $binaryCandidate, $relayPackage
    )

    $digest = (Get-FileHash -LiteralPath $binaryCandidate -Algorithm SHA256).Hash.ToLowerInvariant()
    $goVersion = (@(Invoke-NativeCommand -FilePath 'go' -Arguments @('version')) -join "`n").Trim()
    $manifest = [ordered]@{
        schema_version = 1
        operation_id = $operationID
        created_at_utc = (Get-Date).ToUniversalTime().ToString('o')
        artifact = [IO.Path]::GetFileName($artifactPath)
        sha256 = $digest
        revision = $revision
        target = "$targetOperatingSystem/$targetArchitecture"
        go_version = $goVersion
        source_graph_dirty = $sourceStatus.Count -ne 0
        source_graph_status = @($sourceStatus | ForEach-Object { $_.TrimEnd() })
    }
    $manifestJSON = $manifest | ConvertTo-Json -Depth 4
    [IO.File]::WriteAllText(
        $manifestCandidate,
        $manifestJSON + [Environment]::NewLine,
        [Text.UTF8Encoding]::new($false)
    )

    # Publishing both files only after every check preserves the last usable
    # artifact when compilation or manifest generation fails.
    [IO.File]::Move($binaryCandidate, $artifactPath, $true)
    $binaryCandidate = $null
    [IO.File]::Move($manifestCandidate, $manifestPath, $true)
    $manifestCandidate = $null
} finally {
    foreach ($candidate in @($binaryCandidate, $manifestCandidate)) {
        if ($null -ne $candidate -and [IO.File]::Exists($candidate)) {
            [IO.File]::Delete($candidate)
        }
    }
    foreach ($name in $previousEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process')
    }
    Set-Location $previousLocation
}

Write-Output (
    'wsrelay_build operation_id={0} milestone=complete revision={1} sha256={2} source_graph_dirty={3}' -f
    $operationID, $revision, $digest, ($sourceStatus.Count -ne 0).ToString().ToLowerInvariant()
)
Write-Output "artifact=$artifactPath"
Write-Output "manifest=$manifestPath"
