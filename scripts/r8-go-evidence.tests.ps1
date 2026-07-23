Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'r8-go-evidence.ps1')
. (Join-Path $PSScriptRoot 'r8-evidence-summary.ps1')

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

function New-R8TestSource([char]$DigestCharacter) {
    return [pscustomobject][ordered]@{
        IdentityKind = 'workspace-manifest'
        Commit = '0123456789abcdef0123456789abcdef01234567'
        CommitStatus = 'commit-pending-dirty-workspace'
        WorktreeClean = $false
        SourceDigest = ([string]$DigestCharacter) * 64
    }
}

function New-R8TestOperations([scriptblock]$GetSource, [scriptblock]$Execute) {
    return [pscustomobject]@{
        GetSourceIdentity = $GetSource
        TestSourceIdentityEqual = {
            param([object]$Expected, [object]$Actual)
            Test-R8GoSourceSummariesEqual $Expected $Actual
        }
        GetSourceSummary = {
            param([object]$Source)
            [pscustomobject][ordered]@{
                IdentityKind = [string]$Source.IdentityKind
                Commit = [string]$Source.Commit
                CommitStatus = [string]$Source.CommitStatus
                WorktreeClean = [bool]$Source.WorktreeClean
                SourceDigest = [string]$Source.SourceDigest
            }
        }
        Execute = $Execute
    }
}

function New-R8TestMetadata {
    return [pscustomobject][ordered]@{
        OSDescription = 'fixture OS'
        Architecture = 'X64'
        ProcessorCount = 8
        CPUModel = 'fixture CPU'
        PhysicalMemoryBytes = 1GB
        HardwareProbe = 'fixture'
        GoVersion = 'go version fixture'
        GOOS = 'windows'
        GOARCH = 'amd64'
        GOWORK = 'fixture.work'
        GitCommit = '0123456789abcdef0123456789abcdef01234567'
        GitStatus = [pscustomobject]@{ Dirty = $true; EntryCount = 1; SHA256 = ('c' * 64) }
    }
}

function New-R8TestSummary(
    [string]$Status,
    [string]$ErrorMessage,
    [object]$Source,
    [object[]]$Commands,
    [object[]]$Samples,
    [object[]]$Aggregates,
    [Collections.IDictionary]$BenchmarkContract
) {
    $policy = [ordered]@{
        SampleCount = 5
        BenchmarkCount = $BenchmarkContract.Count
        ExpectedSampleTotal = $BenchmarkContract.Count * 5
        TimingRole = 'fixture'
    }
    return New-R8GoSummaryDocument `
        $Status `
        $ErrorMessage `
        ([datetimeoffset]'2026-07-18T00:00:00Z') `
        (New-R8TestMetadata) `
        ([pscustomobject]@{ Stable = $true; Value = $Source; Error = '' }) `
        $Source `
        $policy `
        $Commands `
        $Samples `
        $Aggregates
}

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-r8-go-evidence-' + [guid]::NewGuid().ToString('N'))
$repositoryRoot = Join-Path $testRoot 'repository'
$runPath = Join-Path $testRoot 'run'
[void][IO.Directory]::CreateDirectory($repositoryRoot)
[void][IO.Directory]::CreateDirectory($runPath)
try {
    $source = New-R8TestSource 'a'
    $productionContract = Get-R8GoBenchmarkContract
    $productionDefinitions = @(
        New-R8GoCommandDefinitions `
            $repositoryRoot `
            (Join-Path $repositoryRoot 'core') `
            '1s' `
            5 `
            20 `
            20 `
            1
    )
    $productionNames = @($productionDefinitions.Name)
    $productionBenchmarks = @($productionDefinitions | Where-Object { $_.Kind -eq 'Benchmark' })
    if ($productionContract.Count -ne 18 -or
        $productionDefinitions.Count -ne 8 -or
        $productionBenchmarks.Count -ne 6 -or
        'semantic-core' -cnotin $productionNames -or
        'semantic-relay-registration' -cnotin $productionNames -or
        -not $productionContract.Contains('BenchmarkR8RelaySenderRegistration')) {
        throw 'Production R8 Go command/benchmark contract lost a required semantic or trend identity'
    }
    foreach ($definition in $productionBenchmarks) {
        if ('-count=5' -cnotin @($definition.Arguments)) {
            throw "Production benchmark $($definition.Name) does not request exactly five samples"
        }
    }
    Assert-Throws {
        New-R8GoCommandDefinitions $repositoryRoot $repositoryRoot '1s' 4 1 1 1
    } 'set "5"'

    $productionRunPath = Join-Path $testRoot 'production-schema-run'
    [void][IO.Directory]::CreateDirectory($productionRunPath)
    $productionOperations = New-R8TestOperations `
        { param([string]$Root) return $source }.GetNewClosure() `
        {
            param([object]$Definition, [string]$LogPath)
            [IO.File]::WriteAllText(
                $LogPath,
                "$($Definition.Name) fixture PASS$([Environment]::NewLine)",
                [Text.UTF8Encoding]::new($false)
            )
            return [pscustomobject]@{ ExitCode = 0; Error = '' }
        }
    $productionPlan = Invoke-R8GoCommandPlan `
        $productionDefinitions `
        $repositoryRoot `
        $productionRunPath `
        $source `
        $productionOperations
    if ($productionPlan.Status -ne 'Success' -or @($productionPlan.Commands).Count -ne 8) {
        throw 'Production R8 Go command fixture did not produce the exact successful transcript'
    }
    $productionSamples = @(
        foreach ($benchmarkName in $productionContract.Keys) {
            $owners = @(
                $productionDefinitions |
                    Where-Object { @($_.ExpectedBenchmarks) -ccontains [string]$benchmarkName }
            )
            if ($owners.Count -ne 1) {
                throw "Production benchmark ownership is not exact for $benchmarkName"
            }
            $rawLog = "$($owners[0].Name).txt"
            foreach ($sample in 1..5) {
                $metrics = [ordered]@{}
                foreach ($metricName in $productionContract[$benchmarkName]) {
                    $metrics[[string]$metricName] = [double]$sample
                }
                [pscustomobject]@{
                    Name = [string]$benchmarkName
                    Sample = $sample
                    Iterations = 1
                    Metrics = [pscustomobject]$metrics
                    RawLog = $rawLog
                }
            }
        }
    )
    $productionAggregates = @(
        foreach ($benchmarkName in $productionContract.Keys) {
            $metrics = [ordered]@{}
            foreach ($metricName in $productionContract[$benchmarkName]) {
                $metrics[[string]$metricName] = [pscustomobject]@{
                    Values = @([double]1, [double]2, [double]3, [double]4, [double]5)
                    P50 = [double]3
                    P95 = [double]5
                }
            }
            [pscustomobject]@{
                Name = [string]$benchmarkName
                SampleCount = 5
                Metrics = [pscustomobject]$metrics
            }
        }
    )
    $productionDocument = New-R8TestSummary `
        'Success' `
        '' `
        $source `
        @($productionPlan.Commands) `
        $productionSamples `
        $productionAggregates `
        $productionContract
    Assert-R8GoSummaryDocument `
        $productionDocument `
        $productionDefinitions `
        $productionContract `
        $repositoryRoot `
        $productionRunPath
    $swappedLogDocument = (($productionDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $swappedSample = @(
        $swappedLogDocument.Samples |
            Where-Object { [string]$_.Name -like 'BenchmarkR8V2ReadyScaling/*' } |
            Select-Object -First 1
    )[0]
    $swappedSample.RawLog = 'ready-real-disk.txt'
    Assert-Throws {
        Assert-R8GoSummaryDocument `
            $swappedLogDocument `
            $productionDefinitions `
            $productionContract `
            $repositoryRoot `
            $productionRunPath
    } 'bound to the wrong command log'

    $definitions = @(
        [pscustomobject]@{
            Name = 'semantic-fixture'
            Kind = 'Semantic'
            ExpectedBenchmarks = @()
            WorkingDirectory = $repositoryRoot
            Arguments = @('test', './fixture', '-run', '^TestR8$', '-count=1')
        },
        [pscustomobject]@{
            Name = 'benchmark-fixture'
            Kind = 'Benchmark'
            ExpectedBenchmarks = @('BenchmarkFixture')
            WorkingDirectory = $repositoryRoot
            Arguments = @('test', './fixture', '-run', '^$', '-bench', '^BenchmarkFixture$', '-count=5')
        }
    )
    $successOperations = New-R8TestOperations `
        { param([string]$Root) return $source }.GetNewClosure() `
        {
            param([object]$Definition, [string]$LogPath)
            [IO.File]::WriteAllText(
                $LogPath,
                "$($Definition.Name) PASS$([Environment]::NewLine)",
                [Text.UTF8Encoding]::new($false)
            )
            return [pscustomobject]@{ ExitCode = 0; Error = '' }
        }
    $successPlan = Invoke-R8GoCommandPlan `
        $definitions `
        $repositoryRoot `
        $runPath `
        $source `
        $successOperations
    if ($successPlan.Status -ne 'Success' -or
        @($successPlan.Commands).Count -ne 2 -or
        @($successPlan.BenchmarkLogs).Count -ne 1) {
        throw 'Successful command plan did not preserve the exact semantic/benchmark transcript'
    }
    foreach ($command in @($successPlan.Commands)) {
        if ($command.Status -ne 'Success' -or
            $command.ExitCode -ne 0 -or
            -not $command.ExecutionStarted -or
            -not $command.SourceBefore.Stable -or
            -not $command.SourceAfter.Stable -or
            -not $command.Log.Exists -or
            [string]$command.Log.SHA256 -notmatch '^[0-9a-f]{64}$') {
            throw "Successful command transcript is incomplete: $($command.Name)"
        }
    }

    $benchmarkContract = [ordered]@{ BenchmarkFixture = @('ns/op') }
    $samples = @(
        1..5 | ForEach-Object {
            [pscustomobject]@{
                Name = 'BenchmarkFixture'
                Sample = $_
                Iterations = 1
                Metrics = [pscustomobject]@{ 'ns/op' = [double]$_ }
                RawLog = 'benchmark-fixture.txt'
            }
        }
    )
    $aggregates = @(
        [pscustomobject]@{
            Name = 'BenchmarkFixture'
            SampleCount = 5
            Metrics = [pscustomobject]@{
                'ns/op' = [pscustomobject]@{
                    Values = @([double]1, [double]2, [double]3, [double]4, [double]5)
                    P50 = [double]3
                    P95 = [double]5
                }
            }
        }
    )
    $successDocument = New-R8TestSummary `
        'Success' `
        '' `
        $source `
        @($successPlan.Commands) `
        $samples `
        $aggregates `
        $benchmarkContract
    Assert-R8GoSummaryDocument $successDocument $definitions $benchmarkContract $repositoryRoot $runPath
    $roundTrip = ($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json
    Assert-R8GoSummaryDocument $roundTrip $definitions $benchmarkContract $repositoryRoot $runPath
    $summaryPath = Join-Path $runPath 'summary.json'
    $summaryWriteOperations = New-R8GoSummaryWriteOperations `
        $definitions `
        $benchmarkContract `
        $repositoryRoot `
        $runPath
    [void](Write-R8SummaryAtomically $summaryPath $successDocument 'Success' $summaryWriteOperations)
    if (-not (Test-Path -LiteralPath $summaryPath -PathType Leaf)) {
        throw 'Production R8 Go schema verifier did not atomically publish the valid fixture'
    }

    $missingLog = (($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $missingLog.Commands[0].Log = $null
    Assert-Throws {
        Assert-R8GoSummaryDocument $missingLog $definitions $benchmarkContract $repositoryRoot $runPath
    } 'missing its structured log evidence'

    $wrongSampleLog = (($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $wrongSampleLog.Samples[0].RawLog = 'semantic-fixture.txt'
    Assert-Throws {
        Assert-R8GoSummaryDocument $wrongSampleLog $definitions $benchmarkContract $repositoryRoot $runPath
    } 'bound to the wrong command log'

    $missingMetric = (($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $missingMetric.Samples[0].Metrics = [pscustomobject]@{}
    Assert-Throws {
        Assert-R8GoSummaryDocument $missingMetric $definitions $benchmarkContract $repositoryRoot $runPath
    } 'sample metrics count differs'

    $missingCheckpoint = (($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $missingCheckpoint.Commands[0].SourceBefore = $null
    Assert-Throws {
        Assert-R8GoSummaryDocument $missingCheckpoint $definitions $benchmarkContract $repositoryRoot $runPath
    } 'missing checkpoint SourceBefore'

    $driftedSuccess = (($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $driftedSuccess.Commands[1].SourceAfter.Stable = $false
    Assert-Throws {
        Assert-R8GoSummaryDocument $driftedSuccess $definitions $benchmarkContract $repositoryRoot $runPath
    } 'source drift at SourceAfter'

    $missingSemantic = (($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $missingSemantic.Commands = @($missingSemantic.Commands[1])
    Assert-Throws {
        Assert-R8GoSummaryDocument $missingSemantic $definitions $benchmarkContract $repositoryRoot $runPath
    } 'expected 2'

    $wrongSchema = (($successDocument | ConvertTo-Json -Depth 24) | ConvertFrom-Json)
    $wrongSchema.SchemaVersion = 2
    Assert-Throws {
        Assert-R8GoSummaryDocument $wrongSchema $definitions $benchmarkContract $repositoryRoot $runPath
    } 'schema version must be 3'

    $failedRunPath = Join-Path $testRoot 'failed-run'
    [void][IO.Directory]::CreateDirectory($failedRunPath)
    $failedOperations = New-R8TestOperations `
        { param([string]$Root) return $source }.GetNewClosure() `
        {
            param([object]$Definition, [string]$LogPath)
            [IO.File]::WriteAllText($LogPath, "fixture failure`n", [Text.UTF8Encoding]::new($false))
            return [pscustomobject]@{ ExitCode = 7; Error = '' }
        }
    $failedPlan = Invoke-R8GoCommandPlan `
        $definitions `
        $repositoryRoot `
        $failedRunPath `
        $source `
        $failedOperations
    if ($failedPlan.Status -ne 'Failed' -or
        @($failedPlan.Commands).Count -ne 1 -or
        $failedPlan.Commands[0].ExitCode -ne 7 -or
        -not $failedPlan.Commands[0].Log.Exists) {
        throw 'Failed command did not retain its exit status and hashed log transcript'
    }
    $failedDocument = New-R8TestSummary `
        'Failed' `
        ([string]$failedPlan.Error) `
        $source `
        @($failedPlan.Commands) `
        @() `
        @() `
        $benchmarkContract
    Assert-R8GoSummaryDocument $failedDocument $definitions $benchmarkContract $repositoryRoot $failedRunPath

    $preDriftRunPath = Join-Path $testRoot 'pre-drift-run'
    [void][IO.Directory]::CreateDirectory($preDriftRunPath)
    $changed = New-R8TestSource 'b'
    $executed = [ref]$false
    $preDriftOperations = New-R8TestOperations `
        { param([string]$Root) return $changed }.GetNewClosure() `
        {
            param([object]$Definition, [string]$LogPath)
            $executed.Value = $true
            return [pscustomobject]@{ ExitCode = 0; Error = '' }
        }.GetNewClosure()
    $preDriftPlan = Invoke-R8GoCommandPlan `
        @($definitions[0]) `
        $repositoryRoot `
        $preDriftRunPath `
        $source `
        $preDriftOperations
    $preDriftCommand = @($preDriftPlan.Commands)[0]
    if ($preDriftPlan.Status -ne 'Failed' -or
        $executed.Value -or
        $preDriftCommand.ExecutionStarted -or
        $preDriftCommand.SourceBefore.Stable -or
        $preDriftCommand.SourceAfter.Stable -or
        -not $preDriftCommand.Log.Exists) {
        throw 'Pre-command source drift did not fail closed with a complete two-boundary transcript'
    }

    $checkpointFailureRunPath = Join-Path $testRoot 'checkpoint-failure-run'
    [void][IO.Directory]::CreateDirectory($checkpointFailureRunPath)
    $checkpointExecuted = [ref]$false
    $checkpointFailureOperations = New-R8TestOperations `
        { param([string]$Root) throw 'injected source identity failure' } `
        {
            param([object]$Definition, [string]$LogPath)
            $checkpointExecuted.Value = $true
            return [pscustomobject]@{ ExitCode = 0; Error = '' }
        }.GetNewClosure()
    $checkpointFailurePlan = Invoke-R8GoCommandPlan `
        @($definitions[0]) `
        $repositoryRoot `
        $checkpointFailureRunPath `
        $source `
        $checkpointFailureOperations
    $checkpointFailureCommand = @($checkpointFailurePlan.Commands)[0]
    if ($checkpointFailurePlan.Status -ne 'Failed' -or
        $checkpointExecuted.Value -or
        $null -ne $checkpointFailureCommand.SourceBefore.Source -or
        $null -ne $checkpointFailureCommand.SourceAfter.Source -or
        [string]$checkpointFailureCommand.SourceBefore.Error -notmatch 'injected source identity failure' -or
        -not $checkpointFailureCommand.Log.Exists) {
        throw 'Source checkpoint collection failure did not remain structured failed evidence'
    }
    $checkpointFailureDocument = New-R8TestSummary `
        'Failed' `
        ([string]$checkpointFailurePlan.Error) `
        $source `
        @($checkpointFailurePlan.Commands) `
        @() `
        @() `
        ([ordered]@{})
    Assert-R8GoSummaryDocument `
        $checkpointFailureDocument `
        @($definitions[0]) `
        ([ordered]@{}) `
        $repositoryRoot `
        $checkpointFailureRunPath

    $postDriftRunPath = Join-Path $testRoot 'post-drift-run'
    [void][IO.Directory]::CreateDirectory($postDriftRunPath)
    $sourceCalls = [ref]0
    $postDriftOperations = New-R8TestOperations `
        {
            param([string]$Root)
            $sourceCalls.Value++
            if ($sourceCalls.Value -eq 1) { return $source }
            return $changed
        }.GetNewClosure() `
        {
            param([object]$Definition, [string]$LogPath)
            [IO.File]::WriteAllText($LogPath, "executed before drift`n", [Text.UTF8Encoding]::new($false))
            return [pscustomobject]@{ ExitCode = 0; Error = '' }
        }
    $postDriftPlan = Invoke-R8GoCommandPlan `
        @($definitions[0]) `
        $repositoryRoot `
        $postDriftRunPath `
        $source `
        $postDriftOperations
    $postDriftCommand = @($postDriftPlan.Commands)[0]
    if ($postDriftPlan.Status -ne 'Failed' -or
        -not $postDriftCommand.ExecutionStarted -or
        $postDriftCommand.ExitCode -ne 0 -or
        -not $postDriftCommand.SourceBefore.Stable -or
        $postDriftCommand.SourceAfter.Stable -or
        -not $postDriftCommand.Log.Exists) {
        throw 'Post-command source drift was not retained as failed evidence'
    }

    [IO.File]::AppendAllText(
        (Join-Path $runPath 'semantic-fixture.txt'),
        "tampered after capture`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-Throws {
        Assert-R8GoSummaryDocument $successDocument $definitions $benchmarkContract $repositoryRoot $runPath
    } 'log bytes or digest changed before publication'
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'R8 Go command transcript and schema tests PASS'
