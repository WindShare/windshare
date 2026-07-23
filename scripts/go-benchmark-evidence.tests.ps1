Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'go-benchmark-evidence.ps1')
. (Join-Path $PSScriptRoot 'd5-pion-performance-evidence.ps1')

function Assert-Throws([scriptblock]$Action, [string]$Pattern) {
    try {
        & $Action
    } catch {
        if ([string]$_ -notmatch $Pattern) {
            throw "Unexpected error: $_"
        }
        return
    }
    throw "Expected an error matching: $Pattern"
}

function Write-BenchmarkFixture([string]$Path, [string[]]$Lines) {
    [IO.File]::WriteAllLines($Path, $Lines, [Text.UTF8Encoding]::new($false))
}

function New-PionBenchmarkFixture(
    [int]$PeakBufferedBytes = 1114111,
    [int]$LowWaterBytes = 262144,
    [int]$HighWaterBytes = 1048576,
    [int]$SendAdmissionHighWaterBytes = 1048575,
    [int]$FrameBytes = 65536
) {
    $lines = [Collections.Generic.List[string]]::new()
    foreach ($entry in $script:D5PionChunkBytes.GetEnumerator()) {
        $name = [string]$entry.Key
        $chunkBytes = [int]$entry.Value
        $frames = [math]::Ceiling($chunkBytes / $script:D5PionFrameBytes)
        foreach ($sample in 1..5) {
            $lines.Add(
                "$name-28  1  10 ns/op  20 MB/s  30 B/op  4 allocs/op  $frames frames/chunk  " +
                "$chunkBytes wire-B/chunk  $PeakBufferedBytes peak-buffered-B  $LowWaterBytes low-water-B  " +
                "$HighWaterBytes high-water-B  $SendAdmissionHighWaterBytes send-admission-high-water-B  " +
                "$FrameBytes max-frame-B"
            )
        }
    }
    return @($lines)
}

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-go-benchmark-evidence-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($testRoot)
try {
    $contract = [ordered]@{
        BenchmarkAlpha = @('ns/op', 'B/op', 'allocs/op')
    }
    $validPath = Join-Path $testRoot 'valid.txt'
    Write-BenchmarkFixture $validPath @(
        1..5 | ForEach-Object { "BenchmarkAlpha-28  1  $($_ * 10) ns/op  20 B/op  3 allocs/op" }
    )
    $valid = @(ConvertFrom-GoBenchmarkLog $validPath)
    Assert-GoBenchmarkEvidenceContract $valid $contract 5
    $aggregate = @(Get-GoBenchmarkAggregates $valid)
    if ($aggregate.Count -ne 1 -or
        $aggregate[0].SampleCount -ne 5 -or
        $aggregate[0].Metrics.'ns/op'.P50 -ne 30 -or
        $aggregate[0].Metrics.'ns/op'.P95 -ne 50) {
        throw 'Valid benchmark evidence did not preserve nearest-rank aggregates'
    }

    $missingPath = Join-Path $testRoot 'missing.txt'
    Write-BenchmarkFixture $missingPath @('PASS')
    Assert-Throws { ConvertFrom-GoBenchmarkLog $missingPath } 'contains no samples'

    $skippedPath = Join-Path $testRoot 'skipped.txt'
    Write-BenchmarkFixture $skippedPath @('--- SKIP: BenchmarkAlpha', 'PASS')
    Assert-Throws { ConvertFrom-GoBenchmarkLog $skippedPath } 'contains no samples'

    $renamedPath = Join-Path $testRoot 'renamed.txt'
    Write-BenchmarkFixture $renamedPath @(
        1..5 | ForEach-Object { 'BenchmarkRenamed-28  1  10 ns/op  20 B/op  3 allocs/op' }
    )
    $renamed = @(ConvertFrom-GoBenchmarkLog $renamedPath)
    Assert-Throws { Assert-GoBenchmarkEvidenceContract $renamed $contract 5 } 'identity set differs'

    $partialPath = Join-Path $testRoot 'partial.txt'
    Write-BenchmarkFixture $partialPath @(
        1..4 | ForEach-Object { 'BenchmarkAlpha-28  1  10 ns/op  20 B/op  3 allocs/op' }
    )
    $partial = @(ConvertFrom-GoBenchmarkLog $partialPath)
    Assert-Throws { Assert-GoBenchmarkEvidenceContract $partial $contract 5 } 'expected exactly 5'

    $malformedPath = Join-Path $testRoot 'malformed.txt'
    Write-BenchmarkFixture $malformedPath @('BenchmarkAlpha-28  1  NaN ns/op  20 B/op  3 allocs/op')
    Assert-Throws { ConvertFrom-GoBenchmarkLog $malformedPath } 'Non-finite or invalid'

    $missingMetricPath = Join-Path $testRoot 'missing-metric.txt'
    Write-BenchmarkFixture $missingMetricPath @(
        1..5 | ForEach-Object { 'BenchmarkAlpha-28  1  10 ns/op  20 B/op' }
    )
    $missingMetric = @(ConvertFrom-GoBenchmarkLog $missingMetricPath)
    Assert-Throws {
        Assert-GoBenchmarkEvidenceContract $missingMetric $contract 5
    } 'metric set differs from the exact contract'

    $unexpectedMetricPath = Join-Path $testRoot 'unexpected-metric.txt'
    Write-BenchmarkFixture $unexpectedMetricPath @(
        1..5 | ForEach-Object { 'BenchmarkAlpha-28  1  10 ns/op  20 B/op  3 allocs/op  7 typo/op' }
    )
    $unexpectedMetric = @(ConvertFrom-GoBenchmarkLog $unexpectedMetricPath)
    Assert-Throws {
        Assert-GoBenchmarkEvidenceContract $unexpectedMetric $contract 5
    } 'metric set differs from the exact contract'

    $pionPath = Join-Path $testRoot 'pion-valid.txt'
    $pionLines = @(New-PionBenchmarkFixture)
    Write-BenchmarkFixture $pionPath @($pionLines)
    $pionEvidence = Get-D5PionBaselineEvidence $pionPath
    if ($pionEvidence.SchemaVersion -ne 3 -or
        $pionEvidence.Status -ne 'Success' -or
        @($pionEvidence.Samples).Count -ne 20 -or
        @($pionEvidence.Aggregates).Count -ne 4 -or
        $pionEvidence.Policy.PeakBufferedExclusiveUpperBoundBytes -ne 1114112 -or
        $pionEvidence.Policy.MaximumAdmittedPeakBufferedBytes -ne 1114111 -or
        $pionEvidence.Policy.ProductionSendAdmissionHighWaterBytes -ne 1048575 -or
        $pionEvidence.Policy.MaximumObservedPeakBufferedBytes -ne 1114111) {
        throw 'Valid Pion evidence did not preserve the exact baseline contract'
    }

    $equalLimitPath = Join-Path $testRoot 'pion-equal-limit.txt'
    Write-BenchmarkFixture $equalLimitPath @(New-PionBenchmarkFixture -PeakBufferedBytes 1114112)
    Assert-Throws {
        Get-D5PionBaselineEvidence $equalLimitPath
    } 'must be below the independent exclusive limit 1114112'

    $inflatedHighWaterPath = Join-Path $testRoot 'pion-inflated-high-water.txt'
    Write-BenchmarkFixture $inflatedHighWaterPath @(
        New-PionBenchmarkFixture -HighWaterBytes 2097152
    )
    Assert-Throws {
        Get-D5PionBaselineEvidence $inflatedHighWaterPath
    } 'bufferedAmount constants differ from production'

    $wrongLowWaterPath = Join-Path $testRoot 'pion-wrong-low-water.txt'
    Write-BenchmarkFixture $wrongLowWaterPath @(
        New-PionBenchmarkFixture -LowWaterBytes 524288
    )
    Assert-Throws {
        Get-D5PionBaselineEvidence $wrongLowWaterPath
    } 'bufferedAmount constants differ from production'

    $wrongAdmissionPath = Join-Path $testRoot 'pion-wrong-send-admission.txt'
    Write-BenchmarkFixture $wrongAdmissionPath @(
        New-PionBenchmarkFixture -SendAdmissionHighWaterBytes 1048576
    )
    Assert-Throws {
        Get-D5PionBaselineEvidence $wrongAdmissionPath
    } 'bufferedAmount constants differ from production'
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'Go benchmark evidence contract tests PASS'
