[CmdletBinding()]
param(
    [ValidateSet('Baseline', 'Profile')]
    [string]$Mode = 'Baseline',

    [ValidatePattern('^[1-9][0-9]*(ns|us|ms|s|m|h)$')]
    [string]$BenchTime = '2s',

    [ValidateRange(1, 100)]
    [int]$Count = 5,

    [string]$EvidenceRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Browser/UI trends use scripts/r8-web-performance.ps1 so Windows evidence inherits
# the audited D5 browser lease instead of weakening this real-network host boundary.
if ($IsWindows) {
    throw 'D5 real OS-network measurements are disabled on Windows; use an isolated non-Windows host.'
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
    $EvidenceRoot = Join-Path $repositoryRoot 'tmp/d5-evidence'
}
. (Join-Path $PSScriptRoot 'd5-evidence.ps1')
. (Join-Path $PSScriptRoot 'go-benchmark-evidence.ps1')
. (Join-Path $PSScriptRoot 'd5-pion-performance-evidence.ps1')

if ($Mode -eq 'Baseline' -and $Count -ne $script:D5PionBaselineSampleCount) {
    throw (
        "D5 Baseline requires exactly $script:D5PionBaselineSampleCount independent samples; " +
        "-Count must be $script:D5PionBaselineSampleCount."
    )
}

function Invoke-D5LoggedCommand(
    [string]$Executable,
    [string[]]$Arguments,
    [string]$LogPath
) {
    $parent = Split-Path -Parent $LogPath
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
    $checkpoint = [IO.Path]::GetFileNameWithoutExtension($LogPath)
    [void](Add-D5SourceCheckpoint $script:run "before-command-$checkpoint")
    try {
        & $Executable @Arguments 2>&1 | Tee-Object -FilePath $LogPath
        $exitCode = $LASTEXITCODE
    } finally {
        [void](Add-D5SourceCheckpoint $script:run "after-command-$checkpoint")
    }
    if ($exitCode -ne 0) {
        throw "$Executable exited with code $exitCode; see $LogPath"
    }
}

$command = "scripts/d5-performance.ps1 -Mode $Mode -BenchTime $BenchTime -Count $Count"
$script:run = New-D5EvidenceRun $repositoryRoot $EvidenceRoot $Mode $command
$run = $script:run
$failure = $null
try {
    Push-Location $repositoryRoot
    try {
        switch ($Mode) {
            'Baseline' {
                $baselineLogPath = Join-Path $run.StagePath 'go/pion-baseline.txt'
                Invoke-D5LoggedCommand 'go' @(
                    'test', './transport/webrtc', '-run', '^$',
                    '-bench', '^BenchmarkPionChunkTransfer$', '-benchmem',
                    "-benchtime=$BenchTime", "-count=$Count"
                ) $baselineLogPath
                $baseline = Get-D5PionBaselineEvidence $baselineLogPath
                [IO.File]::WriteAllText(
                    (Join-Path $run.StagePath 'go/pion-summary.json'),
                    ($baseline | ConvertTo-Json -Depth 24),
                    [Text.UTF8Encoding]::new($false)
                )
            }
            'Profile' {
                New-Item -ItemType Directory -Force -Path (Join-Path $run.StagePath 'profiles') | Out-Null
                Invoke-D5LoggedCommand 'go' @(
                    'test', './transport/webrtc', '-run', '^$',
                    '-bench', '^BenchmarkPionChunkTransfer/chunk_1024KiB$',
                    "-benchtime=$BenchTime",
                    "-cpuprofile=$(Join-Path $run.StagePath 'profiles/pion.cpu')",
                    "-memprofile=$(Join-Path $run.StagePath 'profiles/pion.mem')"
                ) (Join-Path $run.StagePath 'go/pion-profile.txt')
            }
        }
    } finally {
        Pop-Location
    }
} catch {
    $failure = $_
}

$status = if ($null -eq $failure) { 'Success' } else { 'Failed' }
$errorMessage = if ($null -eq $failure) { '' } else { [string]$failure }
$published = Complete-D5EvidenceRun $run $status $errorMessage
Write-Output "D5 evidence published without overwrite: $($published.Path)"
if ($published.Status -ne 'Success' -and $null -eq $failure) {
    $failure = [Exception]::new([string]$published.Error)
}
if ($null -ne $failure) {
    throw $failure
}
