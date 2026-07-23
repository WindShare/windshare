[CmdletBinding()]
param(
    [ValidatePattern('^[1-9][0-9]*(ns|us|ms|s|m|h)$')]
    [string]$BenchTime = '1s',

    [ValidateSet(5)]
    [int]$Count = 5,

    [ValidateRange(1, 10000)]
    [int]$ReadyDiskIterations = 20,

    [ValidateRange(1, 10000)]
    [int]$TransferIterations = 20,

    [ValidateRange(1, 100)]
    [int]$CatalogIterations = 1,

    [string]$EvidenceRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$coreRoot = Join-Path $repositoryRoot 'core'
. (Join-Path $PSScriptRoot 'd5-evidence.ps1')
. (Join-Path $PSScriptRoot 'go-benchmark-evidence.ps1')
. (Join-Path $PSScriptRoot 'r8-evidence-summary.ps1')
. (Join-Path $PSScriptRoot 'r8-go-evidence.ps1')
if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
    $EvidenceRoot = Join-Path $repositoryRoot 'tmp/r8-go-performance'
}
$timestamp = [datetimeoffset]::UtcNow.ToString('yyyyMMddTHHmmss.fffffffZ')
$runPath = Join-Path ([IO.Path]::GetFullPath($EvidenceRoot)) "$timestamp-$PID"
if (Test-Path -LiteralPath $runPath) {
    $runPath = "$runPath-$([guid]::NewGuid().ToString('N'))"
}
[void][IO.Directory]::CreateDirectory($runPath)

function Get-R8GitStatusDigest {
    $status = @(& git -C $repositoryRoot status --porcelain=v1 --untracked-files=all)
    if ($LASTEXITCODE -ne 0) {
        throw 'git status failed while recording the performance environment'
    }
    $bytes = [Text.Encoding]::UTF8.GetBytes(($status -join "`n"))
    $hash = [Security.Cryptography.SHA256]::HashData($bytes)
    return [pscustomobject][ordered]@{
        Dirty = $status.Count -ne 0
        EntryCount = $status.Count
        SHA256 = [Convert]::ToHexString($hash).ToLowerInvariant()
    }
}

function Get-R8HardwareEnvironment {
    $cpuModel = [string]$env:PROCESSOR_IDENTIFIER
    $physicalMemoryBytes = $null
    $probe = 'environment-fallback'
    if (-not $IsWindows -and (Test-Path -LiteralPath '/proc/meminfo' -PathType Leaf)) {
        try {
            $memoryLine = Get-Content -LiteralPath '/proc/meminfo' | Where-Object { $_ -match '^MemTotal:' } | Select-Object -First 1
            if ($memoryLine -match '^MemTotal:\s+([0-9]+)\s+kB$') {
                $physicalMemoryBytes = [long]$Matches[1] * 1024
                $probe = 'proc-meminfo'
            }
        } catch {
            # Keep the explicit null below when the host hides physical memory.
        }
    }
    return [pscustomobject][ordered]@{
        CPUModel = $cpuModel
        PhysicalMemoryBytes = $physicalMemoryBytes
        Probe = $probe
    }
}

$commands = @()
$benchmarkLogs = @()
$failure = $null
$startedAt = [datetimeoffset]::UtcNow
$sourceAtStart = Get-D5SourceIdentity $repositoryRoot
$getSourceIdentity = ${function:Get-D5SourceIdentity}
$testSourceIdentityEqual = ${function:Test-D5SourceIdentityEqual}
$getSourceSummary = ${function:Get-D5SourceIdentitySummary}
$commandOperations = [pscustomobject]@{
    GetSourceIdentity = $getSourceIdentity
    TestSourceIdentityEqual = $testSourceIdentityEqual
    GetSourceSummary = $getSourceSummary
}
$benchmarkContract = [ordered]@{}
$definitions = @()
$benchmarkDefinitions = @()
try {
    $benchmarkContract = Get-R8GoBenchmarkContract
    $definitions = @(
        New-R8GoCommandDefinitions `
            $repositoryRoot `
            $coreRoot `
            $BenchTime `
            $Count `
            $ReadyDiskIterations `
            $TransferIterations `
            $CatalogIterations
    )
    $benchmarkDefinitions = @($definitions | Where-Object { $_.Kind -eq 'Benchmark' })
    $commandPlan = Invoke-R8GoCommandPlan `
        $definitions `
        $repositoryRoot `
        $runPath `
        $sourceAtStart `
        $commandOperations
    $commands = @($commandPlan.Commands)
    $benchmarkLogs = @($commandPlan.BenchmarkLogs)
    if ($commandPlan.Status -ne 'Success') {
        $failure = [Exception]::new([string]$commandPlan.Error)
    }
} catch {
    $failure = $_
}

$samples = @()
$aggregates = @()
if ($null -eq $failure) {
    try {
        if ($benchmarkLogs.Count -ne $benchmarkDefinitions.Count) {
            throw "R8 produced $($benchmarkLogs.Count) benchmark logs; expected exactly $($benchmarkDefinitions.Count)"
        }
        $samples = @(
            foreach ($logPath in $benchmarkLogs) {
                ConvertFrom-GoBenchmarkLog $logPath
            }
        )
        Assert-GoBenchmarkEvidenceContract `
            -Samples $samples `
            -ExpectedMetrics $benchmarkContract `
            -SampleCount $Count
        $aggregates = @(Get-GoBenchmarkAggregates $samples)
        $expectedSamples = $benchmarkContract.Count * $Count
        if ($samples.Count -ne $expectedSamples -or
            $aggregates.Count -ne $benchmarkContract.Count) {
            throw (
                "R8 benchmark evidence shape is incomplete: samples=$($samples.Count)/$expectedSamples, " +
                "aggregates=$($aggregates.Count)/$($benchmarkContract.Count)"
            )
        }
    } catch {
        $failure = $_
    }
}
$summaryPath = Join-Path $runPath 'summary.json'
$getGitStatusDigest = ${function:Get-R8GitStatusDigest}
$getHardwareEnvironment = ${function:Get-R8HardwareEnvironment}
$metadataRepositoryRoot = $repositoryRoot
$metadataFactory = {
    $gitStatus = & $getGitStatusDigest
    $gitCommit = @(& git -C $metadataRepositoryRoot rev-parse HEAD)
    if ($LASTEXITCODE -ne 0 -or $gitCommit.Count -ne 1) {
        throw 'git rev-parse failed while recording the performance environment'
    }
    $goVersion = @(& go version)
    if ($LASTEXITCODE -ne 0 -or $goVersion.Count -eq 0) {
        throw 'go version failed while recording the performance environment'
    }
    $goEnvironment = @(& go env GOOS GOARCH GOWORK)
    if ($LASTEXITCODE -ne 0 -or $goEnvironment.Count -ne 3) {
        throw 'go env returned incomplete performance environment metadata'
    }
    $hardware = & $getHardwareEnvironment
    return [pscustomobject][ordered]@{
        OSDescription = [Runtime.InteropServices.RuntimeInformation]::OSDescription
        Architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
        ProcessorCount = [Environment]::ProcessorCount
        CPUModel = $hardware.CPUModel
        PhysicalMemoryBytes = $hardware.PhysicalMemoryBytes
        HardwareProbe = $hardware.Probe
        GoVersion = ($goVersion -join "`n")
        GOOS = $goEnvironment[0]
        GOARCH = $goEnvironment[1]
        GOWORK = $goEnvironment[2]
        GitCommit = ($gitCommit -join '').Trim()
        GitStatus = $gitStatus
    }
}.GetNewClosure()
$sourceRepositoryRoot = $repositoryRoot
$sourceAtMeasurementStart = $sourceAtStart
$finalSourceSummary = $getSourceSummary
$sourceCheckpoint = {
    $sourceAtEnd = & $getSourceIdentity $sourceRepositoryRoot
    $stable = & $testSourceIdentityEqual $sourceAtMeasurementStart $sourceAtEnd
    return [pscustomobject][ordered]@{
        Stable = $stable
        Value = & $finalSourceSummary $sourceAtEnd
        Error = if ($stable) {
            ''
        } else {
            "Workspace source changed during R8 measurement: start $($sourceAtMeasurementStart.SourceDigest), end $($sourceAtEnd.SourceDigest)"
        }
    }
}.GetNewClosure()
$summarySourceAtStart = & $getSourceSummary $sourceAtStart
$summaryStartedAt = $startedAt
$summaryCommands = @($commands)
$summarySamples = @($samples)
$summaryAggregates = @($aggregates)
$summaryBenchmarkContract = $benchmarkContract
$summaryDefinitions = @($definitions)
$summaryRepositoryRoot = $repositoryRoot
$summaryRunPath = $runPath
$newSummaryDocument = ${function:New-R8GoSummaryDocument}
$assertSummaryDocument = ${function:Assert-R8GoSummaryDocument}
$summaryPolicy = [ordered]@{
    SampleCount = $Count
    BenchmarkCount = $summaryBenchmarkContract.Count
    ExpectedSampleTotal = $summaryBenchmarkContract.Count * $Count
    ReadyAndContentBenchTime = $BenchTime
    ReadyDiskIterations = $ReadyDiskIterations
    TransferIterations = $TransferIterations
    CatalogIterations = $CatalogIterations
    TimingRole = 'descriptive trend only; semantic and budget invariants are the hard gate'
    Network = 'disabled: core benchmarks plus in-memory relayv2 BinarySocket; D5/Pion authority is not used'
    PercentileMethod = 'nearest rank over independent go test benchmark samples'
}
$summaryFactory = {
    param([string]$Status, [string]$ErrorMessage, [object]$Metadata, [object]$Source)
    $document = & $newSummaryDocument `
        $Status `
        $ErrorMessage `
        $summaryStartedAt `
        $Metadata `
        $Source `
        $summarySourceAtStart `
        $summaryPolicy `
        $summaryCommands `
        $summarySamples `
        $summaryAggregates
    & $assertSummaryDocument `
        $document `
        $summaryDefinitions `
        $summaryBenchmarkContract `
        $summaryRepositoryRoot `
        $summaryRunPath
    return $document
}.GetNewClosure()
$summaryWriteOperations = New-R8GoSummaryWriteOperations `
    $definitions `
    $benchmarkContract `
    $repositoryRoot `
    $runPath
$initialFailures = if ($null -eq $failure) { @() } else { @([string]$failure) }
$completion = Complete-R8EvidenceSummary `
    $summaryPath `
    $initialFailures `
    $metadataFactory `
    $sourceCheckpoint `
    $summaryFactory `
    $summaryWriteOperations
if ($completion.Status -ne 'Success') {
    $failure = [Exception]::new([string]$completion.Error)
}
Write-Output "R8 Go performance evidence: $runPath"
Write-Output "R8 Go performance summary: $summaryPath"
if ($null -ne $failure) {
    throw $failure
}
