[CmdletBinding()]
param(
    [ValidateSet('Baseline', 'Profile', 'Scheduler', 'Browser')]
    [string]$Mode = 'Baseline',

    [ValidatePattern('^[1-9][0-9]*(ns|us|ms|s|m|h)$')]
    [string]$BenchTime = '2s',

    [ValidateRange(1, 100)]
    [int]$Count = 5,

    [string]$EvidenceRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($IsWindows -and $Mode -ne 'Scheduler') {
    throw 'D5 real OS-network measurements are disabled on Windows; use an isolated non-Windows host.'
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
    $EvidenceRoot = Join-Path $repositoryRoot 'tmp/d5-evidence'
}
. (Join-Path $PSScriptRoot 'd5-evidence.ps1')

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
$browserOutputWasDefined = Test-Path Env:WINDSHARE_D5_BROWSER_OUTPUT_DIR
$previousBrowserOutput = $env:WINDSHARE_D5_BROWSER_OUTPUT_DIR
try {
    Push-Location $repositoryRoot
    try {
        switch ($Mode) {
            'Baseline' {
                Invoke-D5LoggedCommand 'go' @(
                    'test', './transport/webrtc', '-run', '^$',
                    '-bench', '^BenchmarkPionChunkTransfer$', '-benchmem',
                    "-benchtime=$BenchTime", "-count=$Count"
                ) (Join-Path $run.StagePath 'go/pion-baseline.txt')
                Invoke-D5LoggedCommand 'go' @(
                    'test', './transport/relay',
                    '-run', '^TestSharedForwardQueueChunkPolicy$', '-count=1', '-v'
                ) (Join-Path $run.StagePath 'go/relay-queue.txt')
                Invoke-D5LoggedCommand 'go' @(
                    'test', './transport/relay', '-run', '^$',
                    '-bench', '^BenchmarkRelayChunkTransfer$', '-benchmem',
                    "-benchtime=$BenchTime", "-count=$Count"
                ) (Join-Path $run.StagePath 'go/relay-baseline.txt')
                Invoke-D5LoggedCommand 'go' @(
                    'test', './connectivity', '-run', '^$',
                    '-bench', '^BenchmarkSenderRequestWindowZeroPressure$', '-benchmem',
                    "-benchtime=$BenchTime", "-count=$Count"
                ) (Join-Path $run.StagePath 'go/connectivity-zero-pressure.txt')
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
                Invoke-D5LoggedCommand 'go' @(
                    'test', './transport/relay', '-run', '^$',
                    '-bench', '^BenchmarkRelayChunkTransfer/chunk_1024KiB$',
                    "-benchtime=$BenchTime",
                    "-cpuprofile=$(Join-Path $run.StagePath 'profiles/relay.cpu')",
                    "-memprofile=$(Join-Path $run.StagePath 'profiles/relay.mem')"
                ) (Join-Path $run.StagePath 'go/relay-profile.txt')
            }
            'Scheduler' {
                Invoke-D5LoggedCommand 'pnpm' @(
                    '-C', 'web', 'exec', 'vitest', 'bench',
                    'test/session/scheduler.bench.ts', '--run'
                ) (Join-Path $run.StagePath 'web/scheduler.txt')
            }
            'Browser' {
                $env:WINDSHARE_D5_BROWSER_OUTPUT_DIR = Join-Path $run.StagePath 'web/playwright'
                Invoke-D5LoggedCommand 'pnpm' @(
                    '-C', 'web', 'exec', 'playwright', 'test',
                    '--config', 'test/session/performance.playwright.config.ts'
                ) (Join-Path $run.StagePath 'web/browser.txt')
            }
        }
    } finally {
        Pop-Location
    }
} catch {
    $failure = $_
} finally {
    if ($browserOutputWasDefined) {
        $env:WINDSHARE_D5_BROWSER_OUTPUT_DIR = $previousBrowserOutput
    } elseif (Test-Path Env:WINDSHARE_D5_BROWSER_OUTPUT_DIR) {
        Remove-Item Env:WINDSHARE_D5_BROWSER_OUTPUT_DIR
    }
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
