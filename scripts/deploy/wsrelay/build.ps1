[CmdletBinding()]
param([switch]$RunTests)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$artifactDirectory = Join-Path $repositoryRoot 'dist/deploy/wsrelay'
$artifactPath = Join-Path $artifactDirectory 'wsrelay-linux-amd64'
$manifestPath = Join-Path $artifactDirectory 'manifest.json'
$operationID = '{0}-{1}' -f (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ'), $PID
$imageTag = 'windshare-relay-build:' + [Guid]::NewGuid().ToString('N')
$containerID = ''
$imageCreated = $false
$binaryCandidate = ''
$manifestCandidate = ''
$originalLocation = Get-Location

function Invoke-NativeCommand([string]$FilePath, [string[]]$Arguments) {
    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$FilePath exited with code $LASTEXITCODE" }
}

try {
    Set-Location $repositoryRoot
    if ($RunTests) {
        Invoke-NativeCommand 'make' @('rust')
    }
    # Docker owns the Linux/musl toolchain, so a Windows deployment build cannot
    # accidentally publish a PE executable or depend on the host's C linker.
    $revision = (Invoke-NativeCommand 'git' @('rev-parse', 'HEAD')).Trim()
    $sourceStatus = @(Invoke-NativeCommand 'git' @(
        'status', '--porcelain=v1', '--untracked-files=all', '--',
        'relay-rust', 'deploy/wsrelay/Dockerfile', 'scripts/deploy/wsrelay/build.ps1'
    ))
    [IO.Directory]::CreateDirectory($artifactDirectory) | Out-Null
    $candidateSuffix = [Guid]::NewGuid().ToString('N')
    $binaryCandidate = "$artifactPath.$candidateSuffix.tmp"
    $manifestCandidate = "$manifestPath.$candidateSuffix.tmp"
    Write-Output "wsrelay_build operation_id=$operationID milestone=build_started revision=$revision"
    Invoke-NativeCommand 'docker' @(
        'build', '--platform', 'linux/amd64', '-f', 'deploy/wsrelay/Dockerfile', '-t', $imageTag, '.'
    )
    $imageCreated = $true
    $containerID = (Invoke-NativeCommand 'docker' @('create', $imageTag)).Trim()
    Invoke-NativeCommand 'docker' @('cp', "${containerID}:/wsrelay", $binaryCandidate)
    $digest = (Get-FileHash -LiteralPath $binaryCandidate -Algorithm SHA256).Hash.ToLowerInvariant()
    $manifest = [ordered]@{
        schema_version = 2
        operation_id = $operationID
        created_at_utc = (Get-Date).ToUniversalTime().ToString('o')
        artifact = [IO.Path]::GetFileName($artifactPath)
        sha256 = $digest
        revision = $revision
        target = 'linux/amd64'
        implementation = 'rust-native'
        source_dirty = $sourceStatus.Count -ne 0
        source_status = @($sourceStatus)
    }
    [IO.File]::WriteAllText($manifestCandidate, ($manifest | ConvertTo-Json -Depth 4) + [Environment]::NewLine)
    [IO.File]::Move($binaryCandidate, $artifactPath, $true)
    $binaryCandidate = ''
    [IO.File]::Move($manifestCandidate, $manifestPath, $true)
    $manifestCandidate = ''
    Write-Output "wsrelay_build operation_id=$operationID milestone=complete revision=$revision sha256=$digest"
    Write-Output "artifact=$artifactPath"
    Write-Output "manifest=$manifestPath"
} finally {
    if ($containerID) { & docker rm $containerID | Out-Null }
    if ($imageCreated) { & docker image rm $imageTag | Out-Null }
    foreach ($candidate in @($binaryCandidate, $manifestCandidate)) {
        if ($candidate -and [IO.File]::Exists($candidate)) { [IO.File]::Delete($candidate) }
    }
    Set-Location $originalLocation
}
