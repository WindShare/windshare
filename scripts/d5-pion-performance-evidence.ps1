Set-StrictMode -Version Latest

$script:D5PionBaselineSampleCount = 5
$script:D5PionFrameBytes = 64 * 1024
$script:D5PionLowWaterBytes = 256 * 1024
$script:D5PionHighWaterBytes = 1024 * 1024
$script:D5PionSendAdmissionHighWaterBytes = $script:D5PionHighWaterBytes - 1
$script:D5PionMaximumAdmittedPeakBytes = (
    $script:D5PionSendAdmissionHighWaterBytes + $script:D5PionFrameBytes
)
$script:D5PionPeakExclusiveLimitBytes = $script:D5PionHighWaterBytes + $script:D5PionFrameBytes
$script:D5PionChunkBytes = [ordered]@{
    'BenchmarkPionChunkTransfer/chunk_1KiB' = 1024
    'BenchmarkPionChunkTransfer/chunk_64KiB' = 64 * 1024
    'BenchmarkPionChunkTransfer/chunk_1024KiB' = 1024 * 1024
    'BenchmarkPionChunkTransfer/chunk_4096KiB' = 4 * 1024 * 1024
}

function Get-D5PionBaselineContract {
    $metrics = @(
        'ns/op', 'MB/s', 'B/op', 'allocs/op',
        'frames/chunk', 'wire-B/chunk', 'peak-buffered-B',
        'low-water-B', 'high-water-B', 'send-admission-high-water-B',
        'max-frame-B'
    )
    $contract = [ordered]@{}
    foreach ($name in $script:D5PionChunkBytes.Keys) {
        $contract[$name] = @($metrics)
    }
    return $contract
}

function Get-D5PionBaselineEvidence([Parameter(Mandatory)] [string]$Path) {
    $samples = @(ConvertFrom-GoBenchmarkLog $Path)
    $contract = Get-D5PionBaselineContract
    Assert-GoBenchmarkEvidenceContract `
        -Samples $samples `
        -ExpectedMetrics $contract `
        -SampleCount $script:D5PionBaselineSampleCount

    $maximumObservedPeak = 0.0
    foreach ($sample in $samples) {
        $name = [string]$sample.Name
        $chunkBytes = [double]$script:D5PionChunkBytes[$name]
        $wireBytes = [double]$sample.Metrics.'wire-B/chunk'
        $frameBytes = [double]$sample.Metrics.'max-frame-B'
        $frames = [double]$sample.Metrics.'frames/chunk'
        $lowWater = [double]$sample.Metrics.'low-water-B'
        $highWater = [double]$sample.Metrics.'high-water-B'
        $sendAdmissionHighWater = [double]$sample.Metrics.'send-admission-high-water-B'
        $peakBuffered = [double]$sample.Metrics.'peak-buffered-B'
        $nanoseconds = [double]$sample.Metrics.'ns/op'
        $throughput = [double]$sample.Metrics.'MB/s'
        $allocatedBytes = [double]$sample.Metrics.'B/op'
        $allocations = [double]$sample.Metrics.'allocs/op'
        $expectedFrames = [math]::Ceiling($chunkBytes / $script:D5PionFrameBytes)
        if ($wireBytes -ne $chunkBytes -or $frames -ne $expectedFrames) {
            throw "Pion benchmark $name sample $($sample.Sample) does not preserve the exact chunk/frame geometry"
        }
        if ($frameBytes -ne $script:D5PionFrameBytes -or
            $lowWater -ne $script:D5PionLowWaterBytes -or
            $highWater -ne $script:D5PionHighWaterBytes -or
            $sendAdmissionHighWater -ne $script:D5PionSendAdmissionHighWaterBytes) {
            throw (
                "Pion benchmark $name sample $($sample.Sample) bufferedAmount constants differ from production: " +
                "low=$lowWater/$($script:D5PionLowWaterBytes), " +
                "high=$highWater/$($script:D5PionHighWaterBytes), " +
                "admission=$sendAdmissionHighWater/$($script:D5PionSendAdmissionHighWaterBytes), " +
                "frame=$frameBytes/$($script:D5PionFrameBytes)"
            )
        }
        if ($nanoseconds -le 0 -or $throughput -lt 0 -or
            $allocatedBytes -lt 0 -or $allocations -lt 0 -or $peakBuffered -lt 0) {
            throw "Pion benchmark $name sample $($sample.Sample) reports an invalid required metric value"
        }
        $maximumObservedPeak = [math]::Max($maximumObservedPeak, $peakBuffered)
        # A send already admitted below high-water may add one complete frame;
        # a larger overshoot means the production hysteresis stopped bounding memory.
        if ($peakBuffered -ge $script:D5PionPeakExclusiveLimitBytes) {
            throw (
                "Pion benchmark $name sample $($sample.Sample) peak-buffered-B=$peakBuffered " +
                "must be below the independent exclusive limit $($script:D5PionPeakExclusiveLimitBytes)"
            )
        }
        if ($peakBuffered -gt $script:D5PionMaximumAdmittedPeakBytes) {
            throw (
                "Pion benchmark $name sample $($sample.Sample) peak-buffered-B=$peakBuffered " +
                "exceeds admission high-water plus one maximum frame " +
                "$($script:D5PionMaximumAdmittedPeakBytes)"
            )
        }
    }

    $aggregates = @(Get-GoBenchmarkAggregates $samples)
    if ($samples.Count -ne $contract.Count * $script:D5PionBaselineSampleCount -or
        $aggregates.Count -ne $contract.Count) {
        throw 'Pion benchmark aggregate shape differs from the exact baseline contract'
    }
    return [pscustomobject][ordered]@{
        SchemaVersion = 3
        Status = 'Success'
        Policy = [ordered]@{
            SampleCount = $script:D5PionBaselineSampleCount
            BenchmarkCount = $contract.Count
            ExpectedSampleTotal = $contract.Count * $script:D5PionBaselineSampleCount
            PeakBufferedExclusiveUpperBoundBytes = $script:D5PionPeakExclusiveLimitBytes
            MaximumAdmittedPeakBufferedBytes = $script:D5PionMaximumAdmittedPeakBytes
            ProductionLowWaterBytes = $script:D5PionLowWaterBytes
            ProductionHighWaterBytes = $script:D5PionHighWaterBytes
            ProductionSendAdmissionHighWaterBytes = $script:D5PionSendAdmissionHighWaterBytes
            ProductionMaxFrameBytes = $script:D5PionFrameBytes
            MaximumObservedPeakBufferedBytes = $maximumObservedPeak
            PeakBufferedFormula = 'peak-buffered-B <= send-admission-high-water-B + max-frame-B < production high-water + max-frame-B'
            TimingRole = 'descriptive trend only; exact sample/metric shape and bufferedAmount budget are hard gates'
            PercentileMethod = 'nearest rank over independent go test benchmark samples'
        }
        Samples = @($samples)
        Aggregates = @($aggregates)
        RawLog = [IO.Path]::GetFileName($Path)
    }
}
