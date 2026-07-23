Set-StrictMode -Version Latest

$script:R8GoEvidenceSchemaVersion = 3
$script:R8GoRequiredSampleCount = 5

function Get-R8GoBenchmarkGroups {
    return [ordered]@{
        'ready-scaling' = [pscustomobject]@{
            Names = @(
                @(0, 1000, 10000, 100000, 1000000) |
                    ForEach-Object { "BenchmarkR8V2ReadyScaling/descendants={0:D7}" -f $_ }
            )
            Metrics = @(
                'ns/op', 'B/op', 'allocs/op', 'virtual-descendants',
                'descendant-fs-ops/op', 'registration-material-bytes/op', 'descriptor-bytes/op'
            )
        }
        'ready-real-disk' = [pscustomobject]@{
            Names = @(
                @('fresh', 'reused') |
                    ForEach-Object { "BenchmarkR8V2ReadyRealDisk/path_state=$_" }
            )
            Metrics = @('ns/op', 'B/op', 'allocs/op', 'registration-material-bytes/op')
        }
        'content-file-local' = [pscustomobject]@{
            Names = @(
                @(1024, (64 * 1024), (1024 * 1024), (4 * 1024 * 1024)) |
                    ForEach-Object { "BenchmarkR8ContentFileLocalBlock/block_bytes={0:D7}" -f $_ }
            )
            Metrics = @(
                'ns/op', 'MB/s', 'B/op', 'allocs/op', 'file-local-blocks/op',
                'sealed-bytes/op', 'record-overhead-bytes/op'
            )
        }
        'multi-lane' = [pscustomobject]@{
            Names = @(
                @(1, 2, 4, 8) |
                    ForEach-Object { "BenchmarkR8FileLocalMultiLane/lanes={0:D2}/window={0:D2}/block_bytes=0065536" -f $_ }
            )
            Metrics = @(
                'ns/op', 'MB/s', 'B/op', 'allocs/op', 'lane-fetches/op',
                'duplicate-fetches/op', 'window-blocks'
            )
        }
        'extreme-width-catalog' = [pscustomobject]@{
            Names = @(
                @(10000, 100000) |
                    ForEach-Object { "BenchmarkR8ExtremeWidthCatalogSpill/entries={0:D7}/run_bytes=1048576" -f $_ }
            )
            Metrics = @(
                'ns/op', 'B/op', 'allocs/op', 'entries/op', 'pages/op',
                'sort-spill-written-bytes/op', 'sort-object-commits/op',
                'peak-sort-objects', 'scan-peak-session-bytes', 'retained-catalog-bytes/op'
            )
        }
        'relay-registration-wire' = [pscustomobject]@{
            Names = @('BenchmarkR8RelaySenderRegistration')
            Metrics = @(
                'ns/op', 'B/op', 'allocs/op', 'registration-wire-sent-B/op',
                'registration-wire-received-B/op', 'descriptor-bytes/op',
                'registration-writes/op', 'registration-reads/op'
            )
        }
    }
}

function Get-R8GoBenchmarkContract {
    $contract = [ordered]@{}
    foreach ($group in (Get-R8GoBenchmarkGroups).Values) {
        foreach ($name in $group.Names) {
            $contract[[string]$name] = @($group.Metrics)
        }
    }
    return $contract
}

function New-R8GoCommandDefinitions(
    [string]$RepositoryRoot,
    [string]$CoreRoot,
    [string]$BenchTime,
    [ValidateSet(5)] [int]$Count,
    [int]$ReadyDiskIterations,
    [int]$TransferIterations,
    [int]$CatalogIterations
) {
    $groups = Get-R8GoBenchmarkGroups
    return @(
        [pscustomobject]@{
            Name = 'semantic-core'
            Kind = 'Semantic'
            ExpectedBenchmarks = @()
            WorkingDirectory = $CoreRoot
            Arguments = @('test', './liveshare', './catalog', './transfer', '-run', '^TestR8', '-count=1')
        },
        [pscustomobject]@{
            Name = 'semantic-relay-registration'
            Kind = 'Semantic'
            ExpectedBenchmarks = @()
            WorkingDirectory = $RepositoryRoot
            Arguments = @('test', './transport/relayv2', '-run', '^TestR8', '-count=1')
        },
        [pscustomobject]@{
            Name = 'ready-scaling'
            Kind = 'Benchmark'
            ExpectedBenchmarks = @($groups['ready-scaling'].Names)
            WorkingDirectory = $CoreRoot
            Arguments = @('test', './liveshare', '-run', '^$', '-bench', '^BenchmarkR8V2ReadyScaling$', '-benchmem', "-benchtime=$BenchTime", "-count=$Count")
        },
        [pscustomobject]@{
            Name = 'ready-real-disk'
            Kind = 'Benchmark'
            ExpectedBenchmarks = @($groups['ready-real-disk'].Names)
            WorkingDirectory = $CoreRoot
            Arguments = @('test', './liveshare', '-run', '^$', '-bench', '^BenchmarkR8V2ReadyRealDisk$', '-benchmem', "-benchtime=${ReadyDiskIterations}x", "-count=$Count")
        },
        [pscustomobject]@{
            Name = 'content-file-local'
            Kind = 'Benchmark'
            ExpectedBenchmarks = @($groups['content-file-local'].Names)
            WorkingDirectory = $CoreRoot
            Arguments = @('test', './content/records', '-run', '^$', '-bench', '^BenchmarkR8ContentFileLocalBlock$', '-benchmem', "-benchtime=$BenchTime", "-count=$Count")
        },
        [pscustomobject]@{
            Name = 'multi-lane'
            Kind = 'Benchmark'
            ExpectedBenchmarks = @($groups['multi-lane'].Names)
            WorkingDirectory = $CoreRoot
            Arguments = @('test', './transfer', '-run', '^$', '-bench', '^BenchmarkR8FileLocalMultiLane$', '-benchmem', "-benchtime=${TransferIterations}x", "-count=$Count")
        },
        [pscustomobject]@{
            Name = 'extreme-width-catalog'
            Kind = 'Benchmark'
            ExpectedBenchmarks = @($groups['extreme-width-catalog'].Names)
            WorkingDirectory = $CoreRoot
            Arguments = @('test', './catalog', '-run', '^$', '-bench', '^BenchmarkR8ExtremeWidthCatalogSpill$', '-benchmem', "-benchtime=${CatalogIterations}x", "-count=$Count")
        },
        [pscustomobject]@{
            Name = 'relay-registration-wire'
            Kind = 'Benchmark'
            ExpectedBenchmarks = @($groups['relay-registration-wire'].Names)
            WorkingDirectory = $RepositoryRoot
            Arguments = @('test', './transport/relayv2', '-run', '^$', '-bench', '^BenchmarkR8RelaySenderRegistration$', '-benchmem', "-benchtime=$BenchTime", "-count=$Count")
        }
    )
}

function Get-R8GoEvidenceOperation(
    [object]$Operations,
    [string]$Name,
    [scriptblock]$Default
) {
    if ($null -eq $Operations) {
        return $Default
    }
    if ($Operations -is [Collections.IDictionary] -and $Operations.Contains($Name)) {
        $value = $Operations[$Name]
        if ($null -eq $value) {
            return $Default
        }
        if ($value -isnot [scriptblock]) {
            throw "R8 Go evidence operation $Name is not a script block"
        }
        return [scriptblock]$value
    }
    $property = $Operations.PSObject.Properties[$Name]
    if ($null -eq $property -or $null -eq $property.Value) {
        return $Default
    }
    if ($property.Value -isnot [scriptblock]) {
        throw "R8 Go evidence operation $Name is not a script block"
    }
    return [scriptblock]$property.Value
}

function Get-R8GoObjectProperty([object]$Value, [string]$Name) {
    if ($null -eq $Value) {
        return $null
    }
    if ($Value -is [Collections.IDictionary]) {
        if ($Value.Contains($Name)) {
            return $Value[$Name]
        }
        return $null
    }
    $property = $Value.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }
    return $property.Value
}

function Get-R8GoSourceCheckpoint(
    [string]$Name,
    [string]$RepositoryRoot,
    [object]$SourceAtStart,
    [scriptblock]$GetSourceIdentity,
    [scriptblock]$TestSourceIdentityEqual,
    [scriptblock]$GetSourceSummary
) {
    $source = $null
    $summary = $null
    $stable = $false
    $errorMessage = ''
    try {
        $source = & $GetSourceIdentity $RepositoryRoot
        $summary = & $GetSourceSummary $source
        $stable = [bool](& $TestSourceIdentityEqual $SourceAtStart $source)
        if (-not $stable) {
            $errorMessage = (
                "Workspace source changed at R8 checkpoint '$Name': " +
                "start $($SourceAtStart.SourceDigest), observed $($source.SourceDigest)"
            )
        }
    } catch {
        $stable = $false
        $errorMessage = "R8 source checkpoint '$Name' failed: $_"
    }
    return [pscustomobject][ordered]@{
        Name = $Name
        Stable = $stable
        Error = $errorMessage
        Source = $summary
    }
}

function Get-R8GoLogEvidence([string]$Path, [string]$RunPath) {
    $relativePath = [IO.Path]::GetRelativePath(
        [IO.Path]::GetFullPath($RunPath),
        [IO.Path]::GetFullPath($Path)
    ).Replace('\', '/')
    try {
        if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
            return [pscustomobject][ordered]@{
                Path = $relativePath
                Exists = $false
                Bytes = $null
                SHA256 = ''
                Error = 'command log is missing'
            }
        }
        $item = Get-Item -LiteralPath $Path
        return [pscustomobject][ordered]@{
            Path = $relativePath
            Exists = $true
            Bytes = [long]$item.Length
            SHA256 = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
            Error = ''
        }
    } catch {
        return [pscustomobject][ordered]@{
            Path = $relativePath
            Exists = $false
            Bytes = $null
            SHA256 = ''
            Error = "command log evidence failed: $_"
        }
    }
}

function Add-R8GoLogDiagnostic([string]$Path, [string]$Message) {
    try {
        [IO.File]::AppendAllText(
            $Path,
            "R8 runner: $Message$([Environment]::NewLine)",
            [Text.UTF8Encoding]::new($false)
        )
        return ''
    } catch {
        return "could not append the runner diagnostic to the command log: $_"
    }
}

function Invoke-R8GoProcess([object]$Definition, [string]$LogPath) {
    $exitCode = $null
    $errorMessage = ''
    Push-Location ([string]$Definition.WorkingDirectory)
    try {
        & go @($Definition.Arguments) 2>&1 |
            Tee-Object -FilePath $LogPath |
            Out-Host
        $exitCode = $LASTEXITCODE
    } catch {
        $errorMessage = "go command execution failed: $_"
    } finally {
        Pop-Location
    }
    return [pscustomobject][ordered]@{
        ExitCode = $exitCode
        Error = $errorMessage
    }
}

function Invoke-R8GoCommandPlan(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]]$Definitions,
    [Parameter(Mandatory)] [string]$RepositoryRoot,
    [Parameter(Mandatory)] [string]$RunPath,
    [Parameter(Mandatory)] [object]$SourceAtStart,
    [Parameter(Mandatory)] [object]$Operations
) {
    $getSourceIdentity = Get-R8GoEvidenceOperation $Operations 'GetSourceIdentity' $null
    $testSourceIdentityEqual = Get-R8GoEvidenceOperation $Operations 'TestSourceIdentityEqual' $null
    $getSourceSummary = Get-R8GoEvidenceOperation $Operations 'GetSourceSummary' $null
    foreach ($operation in @(
        [pscustomobject]@{ Name = 'GetSourceIdentity'; Value = $getSourceIdentity },
        [pscustomobject]@{ Name = 'TestSourceIdentityEqual'; Value = $testSourceIdentityEqual },
        [pscustomobject]@{ Name = 'GetSourceSummary'; Value = $getSourceSummary }
    )) {
        if ($null -eq $operation.Value) {
            throw "R8 Go command plan requires operation $($operation.Name)"
        }
    }
    $execute = Get-R8GoEvidenceOperation $Operations 'Execute' ${function:Invoke-R8GoProcess}

    $commands = [Collections.Generic.List[object]]::new()
    $benchmarkLogs = [Collections.Generic.List[string]]::new()
    $planError = ''
    foreach ($definition in $Definitions) {
        $name = [string](Get-R8GoObjectProperty $definition 'Name')
        $kind = [string](Get-R8GoObjectProperty $definition 'Kind')
        $expectedBenchmarks = @(
            (Get-R8GoObjectProperty $definition 'ExpectedBenchmarks') |
                ForEach-Object { [string]$_ }
        )
        if ([string]::IsNullOrWhiteSpace($name) -or
            [IO.Path]::GetFileName($name) -cne $name -or
            $kind -notin @('Semantic', 'Benchmark') -or
            ($kind -eq 'Semantic' -and $expectedBenchmarks.Count -ne 0) -or
            ($kind -eq 'Benchmark' -and $expectedBenchmarks.Count -eq 0)) {
            throw "Invalid R8 Go command definition: name='$name', kind='$kind'"
        }
        $logPath = Join-Path $RunPath "$name.txt"
        [IO.File]::WriteAllText($logPath, '', [Text.UTF8Encoding]::new($false))
        $startedAt = [datetimeoffset]::UtcNow
        $errors = [Collections.Generic.List[string]]::new()
        $before = Get-R8GoSourceCheckpoint `
            "before-$name" `
            $RepositoryRoot `
            $SourceAtStart `
            $getSourceIdentity `
            $testSourceIdentityEqual `
            $getSourceSummary
        $executionStarted = $false
        $exitCode = $null
        if (-not $before.Stable) {
            $errors.Add([string]$before.Error)
        } else {
            $executionStarted = $true
            try {
                $execution = & $execute $definition $logPath
                if ($null -eq $execution) {
                    $errors.Add('R8 Go command executor returned no result')
                } else {
                    $exitCode = Get-R8GoObjectProperty $execution 'ExitCode'
                    $executionError = [string](Get-R8GoObjectProperty $execution 'Error')
                    if (-not [string]::IsNullOrWhiteSpace($executionError)) {
                        $errors.Add($executionError)
                    }
                    if ($null -eq $exitCode) {
                        $errors.Add('R8 Go command executor returned no exit code')
                    } elseif ([int]$exitCode -ne 0) {
                        $errors.Add(
                            "go $(@($definition.Arguments) -join ' ') exited with code $exitCode"
                        )
                    }
                }
            } catch {
                $errors.Add("R8 Go command executor failed: $_")
            }
        }
        # The post-check is unconditional. A command failure must not hide a
        # concurrent source change, and a pre-check failure still needs a closed
        # two-boundary transcript rather than an ambiguous half-record.
        $after = Get-R8GoSourceCheckpoint `
            "after-$name" `
            $RepositoryRoot `
            $SourceAtStart `
            $getSourceIdentity `
            $testSourceIdentityEqual `
            $getSourceSummary
        if (-not $after.Stable) {
            $errors.Add([string]$after.Error)
        }

        if ($errors.Count -gt 0) {
            $diagnosticError = Add-R8GoLogDiagnostic $logPath ($errors -join '; ')
            if (-not [string]::IsNullOrWhiteSpace($diagnosticError)) {
                $errors.Add($diagnosticError)
            }
        }
        $log = Get-R8GoLogEvidence $logPath $RunPath
        if (-not $log.Exists) {
            $errors.Add([string]$log.Error)
        }
        $status = if ($errors.Count -eq 0 -and $executionStarted -and [int]$exitCode -eq 0) {
            'Success'
        } else {
            'Failed'
        }
        $errorMessage = if ($errors.Count -eq 0) { '' } else { $errors -join '; ' }
        $relativeWorkingDirectory = [IO.Path]::GetRelativePath(
            [IO.Path]::GetFullPath($RepositoryRoot),
            [IO.Path]::GetFullPath([string]$definition.WorkingDirectory)
        ).Replace('\', '/')
        $commands.Add([pscustomobject][ordered]@{
            Name = $name
            Kind = $kind
            ExpectedBenchmarks = @($expectedBenchmarks)
            WorkingDirectory = $relativeWorkingDirectory
            Arguments = @($definition.Arguments)
            Status = $status
            ExecutionStarted = $executionStarted
            ExitCode = $exitCode
            StartedAtUTC = $startedAt.ToString('o')
            FinishedAtUTC = [datetimeoffset]::UtcNow.ToString('o')
            Error = $errorMessage
            SourceBefore = $before
            SourceAfter = $after
            Log = $log
        })
        if ($status -eq 'Success' -and $kind -eq 'Benchmark') {
            $benchmarkLogs.Add([IO.Path]::GetFullPath($logPath))
        }
        if ($status -ne 'Success') {
            $planError = "R8 Go command $name failed: $errorMessage"
            break
        }
    }
    return [pscustomobject][ordered]@{
        Status = if ([string]::IsNullOrWhiteSpace($planError)) { 'Success' } else { 'Failed' }
        Error = $planError
        Commands = @($commands)
        BenchmarkLogs = @($benchmarkLogs)
    }
}

function New-R8GoSummaryDocument(
    [ValidateSet('Success', 'Failed')] [string]$Status,
    [string]$ErrorMessage,
    [datetimeoffset]$StartedAt,
    [object]$Metadata,
    [object]$SourceCompletion,
    [object]$SourceAtStartSummary,
    [object]$Policy,
    [AllowEmptyCollection()] [object[]]$Commands,
    [AllowEmptyCollection()] [object[]]$Samples,
    [AllowEmptyCollection()] [object[]]$Aggregates
) {
    $sourceAtEnd = if ($null -eq $SourceCompletion) { $null } else { $SourceCompletion.Value }
    $environment = [ordered]@{
        MetadataAvailable = $null -ne $Metadata
        OSDescription = if ($null -eq $Metadata) { '' } else { $Metadata.OSDescription }
        Architecture = if ($null -eq $Metadata) { '' } else { $Metadata.Architecture }
        ProcessorCount = if ($null -eq $Metadata) { $null } else { $Metadata.ProcessorCount }
        CPUModel = if ($null -eq $Metadata) { '' } else { $Metadata.CPUModel }
        PhysicalMemoryBytes = if ($null -eq $Metadata) { $null } else { $Metadata.PhysicalMemoryBytes }
        HardwareProbe = if ($null -eq $Metadata) { '' } else { $Metadata.HardwareProbe }
        GoVersion = if ($null -eq $Metadata) { '' } else { $Metadata.GoVersion }
        GOOS = if ($null -eq $Metadata) { '' } else { $Metadata.GOOS }
        GOARCH = if ($null -eq $Metadata) { '' } else { $Metadata.GOARCH }
        GOWORK = if ($null -eq $Metadata) { '' } else { $Metadata.GOWORK }
        GitCommit = if ($null -eq $Metadata) { '' } else { $Metadata.GitCommit }
        GitStatus = if ($null -eq $Metadata) { $null } else { $Metadata.GitStatus }
        SourceAtStart = $SourceAtStartSummary
        SourceAtEnd = $sourceAtEnd
        SourceStable = $null -ne $SourceCompletion -and [bool]$SourceCompletion.Stable
    }
    return [ordered]@{
        SchemaVersion = $script:R8GoEvidenceSchemaVersion
        Status = $Status
        StartedAtUTC = $StartedAt.ToString('o')
        FinishedAtUTC = [datetimeoffset]::UtcNow.ToString('o')
        Error = $ErrorMessage
        Policy = $Policy
        Environment = $environment
        Commands = @($Commands)
        Samples = @($Samples)
        Aggregates = @($Aggregates)
    }
}

function Assert-R8GoSHA256([string]$Value, [string]$Field) {
    if ($Value -cnotmatch '^[0-9a-f]{64}$') {
        throw "R8 Go summary $Field is not a lowercase SHA-256 digest"
    }
}

function Test-R8GoSourceSummariesEqual([object]$Expected, [object]$Actual) {
    if ($null -eq $Expected -or $null -eq $Actual) {
        return $false
    }
    return [string]$Expected.IdentityKind -ceq [string]$Actual.IdentityKind -and
        [string]$Expected.Commit -ceq [string]$Actual.Commit -and
        [bool]$Expected.WorktreeClean -eq [bool]$Actual.WorktreeClean -and
        [string]$Expected.SourceDigest -ceq [string]$Actual.SourceDigest
}

function Assert-R8GoStringArrayEqual(
    [AllowEmptyCollection()] [string[]]$Expected,
    [AllowEmptyCollection()] [string[]]$Actual,
    [string]$Field
) {
    if ($Expected.Count -ne $Actual.Count) {
        throw "R8 Go summary $Field count differs: $($Actual.Count)/$($Expected.Count)"
    }
    for ($index = 0; $index -lt $Expected.Count; $index++) {
        if ($Expected[$index] -cne $Actual[$index]) {
            throw "R8 Go summary $Field differs at index $index"
        }
    }
}

function Get-R8GoMetricNames([object]$Metrics) {
    if ($null -eq $Metrics) {
        return @()
    }
    if ($Metrics -is [Collections.IDictionary]) {
        return @($Metrics.Keys | ForEach-Object { [string]$_ })
    }
    return @($Metrics.PSObject.Properties | ForEach-Object { [string]$_.Name })
}

function Assert-R8GoSummaryDocument(
    [object]$Document,
    [Parameter(Mandatory)] [object[]]$ExpectedCommands,
    [Parameter(Mandatory)] [Collections.IDictionary]$BenchmarkContract,
    [Parameter(Mandatory)] [string]$RepositoryRoot,
    [Parameter(Mandatory)] [string]$RunPath
) {
    if ([int](Get-R8GoObjectProperty $Document 'SchemaVersion') -ne $script:R8GoEvidenceSchemaVersion) {
        throw "R8 Go summary schema version must be $script:R8GoEvidenceSchemaVersion"
    }
    $status = [string](Get-R8GoObjectProperty $Document 'Status')
    if ($status -notin @('Success', 'Failed')) {
        throw "R8 Go summary has invalid status '$status'"
    }
    $errorMessage = [string](Get-R8GoObjectProperty $Document 'Error')
    if (($status -eq 'Success' -and -not [string]::IsNullOrEmpty($errorMessage)) -or
        ($status -eq 'Failed' -and [string]::IsNullOrWhiteSpace($errorMessage))) {
        throw 'R8 Go summary status and error ledger disagree'
    }

    $policy = Get-R8GoObjectProperty $Document 'Policy'
    if ($null -eq $policy -or
        [int](Get-R8GoObjectProperty $policy 'SampleCount') -ne $script:R8GoRequiredSampleCount -or
        [int](Get-R8GoObjectProperty $policy 'BenchmarkCount') -ne $BenchmarkContract.Count -or
        [int](Get-R8GoObjectProperty $policy 'ExpectedSampleTotal') -ne
            $BenchmarkContract.Count * $script:R8GoRequiredSampleCount) {
        throw 'R8 Go summary policy differs from the exact benchmark contract'
    }

    $commands = @((Get-R8GoObjectProperty $Document 'Commands'))
    if ($status -eq 'Success' -and $commands.Count -ne $ExpectedCommands.Count) {
        throw "R8 Go Success summary has $($commands.Count) commands; expected $($ExpectedCommands.Count)"
    }
    if ($commands.Count -gt $ExpectedCommands.Count) {
        throw 'R8 Go summary has commands outside the exact execution plan'
    }
    $environment = Get-R8GoObjectProperty $Document 'Environment'
    $sourceAtStart = Get-R8GoObjectProperty $environment 'SourceAtStart'
    if ($null -eq $sourceAtStart) {
        throw 'R8 Go summary is missing its starting source identity'
    }
    Assert-R8GoSHA256 ([string]$sourceAtStart.SourceDigest) 'Environment.SourceAtStart.SourceDigest'

    $seenFailure = $false
    for ($index = 0; $index -lt $commands.Count; $index++) {
        $command = $commands[$index]
        $expected = $ExpectedCommands[$index]
        $name = [string](Get-R8GoObjectProperty $command 'Name')
        if ($name -cne [string]$expected.Name -or
            [string](Get-R8GoObjectProperty $command 'Kind') -cne [string]$expected.Kind) {
            throw "R8 Go summary command $index differs from the exact execution plan"
        }
        Assert-R8GoStringArrayEqual `
            @($expected.ExpectedBenchmarks | ForEach-Object { [string]$_ }) `
            @((Get-R8GoObjectProperty $command 'ExpectedBenchmarks') | ForEach-Object { [string]$_ }) `
            "command $name benchmark ownership"
        $expectedWorkingDirectory = [IO.Path]::GetRelativePath(
            [IO.Path]::GetFullPath($RepositoryRoot),
            [IO.Path]::GetFullPath([string]$expected.WorkingDirectory)
        ).Replace('\', '/')
        if ([string](Get-R8GoObjectProperty $command 'WorkingDirectory') -cne $expectedWorkingDirectory) {
            throw "R8 Go summary command $name has the wrong working directory"
        }
        Assert-R8GoStringArrayEqual `
            @($expected.Arguments | ForEach-Object { [string]$_ }) `
            @((Get-R8GoObjectProperty $command 'Arguments') | ForEach-Object { [string]$_ }) `
            "command $name arguments"

        $commandStatus = [string](Get-R8GoObjectProperty $command 'Status')
        if ($commandStatus -notin @('Success', 'Failed') -or ($seenFailure -and $commandStatus -eq 'Success')) {
            throw "R8 Go summary command $name has an invalid ordered status"
        }
        if ($commandStatus -eq 'Failed') {
            $seenFailure = $true
            if ($index -ne ($commands.Count - 1)) {
                throw "R8 Go failed command $name is not the terminal command record"
            }
            if ([string]::IsNullOrWhiteSpace([string](Get-R8GoObjectProperty $command 'Error'))) {
                throw "R8 Go failed command $name has no error"
            }
        } elseif ($null -eq (Get-R8GoObjectProperty $command 'ExitCode') -or
            [int](Get-R8GoObjectProperty $command 'ExitCode') -ne 0 -or
            -not [bool](Get-R8GoObjectProperty $command 'ExecutionStarted') -or
            -not [string]::IsNullOrEmpty([string](Get-R8GoObjectProperty $command 'Error'))) {
            throw "R8 Go successful command $name has inconsistent execution results"
        }

        foreach ($checkpointField in @('SourceBefore', 'SourceAfter')) {
            $checkpoint = Get-R8GoObjectProperty $command $checkpointField
            if ($null -eq $checkpoint) {
                throw "R8 Go command $name is missing checkpoint $checkpointField"
            }
            $checkpointPrefix = if ($checkpointField -eq 'SourceBefore') { 'before' } else { 'after' }
            if ([string](Get-R8GoObjectProperty $checkpoint 'Name') -cne "$checkpointPrefix-$name") {
                throw "R8 Go command $name has a misnamed checkpoint $checkpointField"
            }
            $checkpointStable = [bool](Get-R8GoObjectProperty $checkpoint 'Stable')
            $checkpointSource = Get-R8GoObjectProperty $checkpoint 'Source'
            $checkpointError = [string](Get-R8GoObjectProperty $checkpoint 'Error')
            if ($commandStatus -eq 'Success' -and
                (-not $checkpointStable -or $null -eq $checkpointSource -or
                    -not [string]::IsNullOrEmpty($checkpointError) -or
                    -not (Test-R8GoSourceSummariesEqual $sourceAtStart $checkpointSource))) {
                throw "R8 Go successful command $name has source drift at $checkpointField"
            }
            if ($commandStatus -eq 'Failed') {
                if ($checkpointStable -and
                    ($null -eq $checkpointSource -or
                        -not [string]::IsNullOrEmpty($checkpointError) -or
                        -not (Test-R8GoSourceSummariesEqual $sourceAtStart $checkpointSource))) {
                    throw "R8 Go failed command $name has an inconsistent stable checkpoint $checkpointField"
                }
                if (-not $checkpointStable -and [string]::IsNullOrWhiteSpace($checkpointError)) {
                    throw "R8 Go failed command $name has an unexplained checkpoint failure at $checkpointField"
                }
            }
        }

        $log = Get-R8GoObjectProperty $command 'Log'
        if ($null -eq $log) {
            throw "R8 Go command $name is missing its structured log evidence"
        }
        $relativeLog = [string](Get-R8GoObjectProperty $log 'Path')
        if ($relativeLog -cne "$name.txt" -or [IO.Path]::IsPathRooted($relativeLog)) {
            throw "R8 Go command $name has an invalid log path"
        }
        if (-not [bool](Get-R8GoObjectProperty $log 'Exists')) {
            if ($commandStatus -eq 'Success' -or
                [string]::IsNullOrWhiteSpace([string](Get-R8GoObjectProperty $log 'Error'))) {
                throw "R8 Go command $name is missing its structured log evidence"
            }
            continue
        }
        if ($commandStatus -eq 'Success' -and [long](Get-R8GoObjectProperty $log 'Bytes') -le 0) {
            throw "R8 Go successful command $name has an empty log transcript"
        }
        $fullLog = [IO.Path]::GetFullPath((Join-Path $RunPath $relativeLog))
        $relativeToRun = [IO.Path]::GetRelativePath([IO.Path]::GetFullPath($RunPath), $fullLog)
        if ($relativeToRun.StartsWith('..', [StringComparison]::Ordinal) -or
            -not (Test-Path -LiteralPath $fullLog -PathType Leaf)) {
            throw "R8 Go command $name log escapes or is absent from the run root"
        }
        $item = Get-Item -LiteralPath $fullLog
        $digest = (Get-FileHash -LiteralPath $fullLog -Algorithm SHA256).Hash.ToLowerInvariant()
        Assert-R8GoSHA256 ([string](Get-R8GoObjectProperty $log 'SHA256')) "command $name log"
        if ([long](Get-R8GoObjectProperty $log 'Bytes') -ne [long]$item.Length -or
            [string](Get-R8GoObjectProperty $log 'SHA256') -cne $digest) {
            throw "R8 Go command $name log bytes or digest changed before publication"
        }
    }
    if ($status -eq 'Success' -and $seenFailure) {
        throw 'R8 Go Success summary contains a failed command'
    }

    if ($status -eq 'Success') {
        if (-not [bool](Get-R8GoObjectProperty $environment 'MetadataAvailable') -or
            -not [bool](Get-R8GoObjectProperty $environment 'SourceStable')) {
            throw 'R8 Go Success summary lacks stable source or environment metadata'
        }
        $sourceAtEnd = Get-R8GoObjectProperty $environment 'SourceAtEnd'
        if (-not (Test-R8GoSourceSummariesEqual $sourceAtStart $sourceAtEnd)) {
            throw 'R8 Go Success summary source identities differ at finalization'
        }
        $samples = @((Get-R8GoObjectProperty $Document 'Samples'))
        $aggregates = @((Get-R8GoObjectProperty $Document 'Aggregates'))
        $expectedSamples = $BenchmarkContract.Count * $script:R8GoRequiredSampleCount
        if ($samples.Count -ne $expectedSamples -or $aggregates.Count -ne $BenchmarkContract.Count) {
            throw 'R8 Go Success summary sample or aggregate shape is incomplete'
        }
        $expectedLogByBenchmark = [Collections.Generic.Dictionary[string, string]]::new(
            [StringComparer]::Ordinal
        )
        foreach ($command in $commands | Where-Object { [string]$_.Kind -ceq 'Benchmark' }) {
            foreach ($benchmarkName in @($command.ExpectedBenchmarks)) {
                $benchmarkName = [string]$benchmarkName
                if (-not $BenchmarkContract.Contains($benchmarkName) -or
                    $expectedLogByBenchmark.ContainsKey($benchmarkName)) {
                    throw "R8 Go benchmark ownership is missing, duplicated, or outside the exact contract: $benchmarkName"
                }
                $expectedLogByBenchmark.Add($benchmarkName, [string]$command.Log.Path)
            }
        }
        if ($expectedLogByBenchmark.Count -ne $BenchmarkContract.Count) {
            throw 'R8 Go benchmark ownership does not cover the exact trend contract'
        }
        foreach ($sample in $samples) {
            $sampleName = [string]$sample.Name
            if (-not $expectedLogByBenchmark.ContainsKey($sampleName) -or
                [string]$sample.RawLog -cne $expectedLogByBenchmark[$sampleName]) {
                throw "R8 Go benchmark $sampleName is bound to the wrong command log"
            }
        }
        $benchmarkLogNames = @(
            $commands |
                Where-Object { [string]$_.Kind -ceq 'Benchmark' } |
                ForEach-Object { [string]$_.Log.Path } |
                Sort-Object -CaseSensitive -Unique
        )
        $sampleLogNames = @(
            $samples |
                ForEach-Object { [string]$_.RawLog } |
                Sort-Object -CaseSensitive -Unique
        )
        Assert-R8GoStringArrayEqual `
            $benchmarkLogNames `
            $sampleLogNames `
            'benchmark sample raw-log identities'
        foreach ($benchmarkName in @($BenchmarkContract.Keys)) {
            $group = @(
                $samples |
                    Where-Object { [string]$_.Name -ceq [string]$benchmarkName } |
                    Sort-Object Sample
            )
            if ($group.Count -ne $script:R8GoRequiredSampleCount) {
                throw "R8 Go Success summary benchmark $benchmarkName does not have exactly five samples"
            }
            $requiredMetrics = @(
                $BenchmarkContract[$benchmarkName] |
                    ForEach-Object { [string]$_ } |
                    Sort-Object -CaseSensitive
            )
            for ($sampleIndex = 0; $sampleIndex -lt $group.Count; $sampleIndex++) {
                $sample = $group[$sampleIndex]
                if ([int]$sample.Sample -ne $sampleIndex + 1 -or [long]$sample.Iterations -le 0) {
                    throw "R8 Go Success summary benchmark $benchmarkName has invalid sample ordinals or iterations"
                }
                $actualMetrics = @(Get-R8GoMetricNames $sample.Metrics | Sort-Object -CaseSensitive)
                Assert-R8GoStringArrayEqual $requiredMetrics $actualMetrics "benchmark $benchmarkName sample metrics"
                foreach ($metricName in $requiredMetrics) {
                    $value = [double](Get-R8GoObjectProperty $sample.Metrics $metricName)
                    if ([double]::IsNaN($value) -or [double]::IsInfinity($value)) {
                        throw "R8 Go Success summary benchmark $benchmarkName metric $metricName is not finite"
                    }
                }
            }
            $aggregate = @($aggregates | Where-Object { [string]$_.Name -ceq [string]$benchmarkName })
            if ($aggregate.Count -ne 1 -or [int]$aggregate[0].SampleCount -ne $script:R8GoRequiredSampleCount) {
                throw "R8 Go Success summary benchmark $benchmarkName has incomplete aggregates"
            }
            $aggregateMetrics = @(Get-R8GoMetricNames $aggregate[0].Metrics | Sort-Object -CaseSensitive)
            Assert-R8GoStringArrayEqual $requiredMetrics $aggregateMetrics "benchmark $benchmarkName aggregate metrics"
            foreach ($metricName in $requiredMetrics) {
                $expectedValues = [double[]]@(
                    $group | ForEach-Object { [double](Get-R8GoObjectProperty $_.Metrics $metricName) }
                )
                $metricAggregate = Get-R8GoObjectProperty $aggregate[0].Metrics $metricName
                $actualValues = [double[]]@((Get-R8GoObjectProperty $metricAggregate 'Values'))
                if ($actualValues.Count -ne $script:R8GoRequiredSampleCount) {
                    throw "R8 Go Success summary benchmark $benchmarkName metric $metricName has incomplete raw values"
                }
                for ($valueIndex = 0; $valueIndex -lt $expectedValues.Count; $valueIndex++) {
                    if ($actualValues[$valueIndex] -ne $expectedValues[$valueIndex]) {
                        throw "R8 Go Success summary benchmark $benchmarkName metric $metricName raw values differ"
                    }
                }
                $orderedValues = @($expectedValues | Sort-Object)
                $p50 = Get-R8GoObjectProperty $metricAggregate 'P50'
                $p95 = Get-R8GoObjectProperty $metricAggregate 'P95'
                if ($null -eq $p50 -or $null -eq $p95 -or
                    [double]$p50 -ne [double]$orderedValues[2] -or
                    [double]$p95 -ne [double]$orderedValues[4]) {
                    throw "R8 Go Success summary benchmark $benchmarkName metric $metricName percentiles differ"
                }
            }
        }
    }
}

function New-R8GoSummaryWriteOperations(
    [object[]]$ExpectedCommands,
    [Collections.IDictionary]$BenchmarkContract,
    [string]$RepositoryRoot,
    [string]$RunPath
) {
    $expected = @($ExpectedCommands)
    $contract = $BenchmarkContract
    $root = $RepositoryRoot
    $run = $RunPath
    $assertSummary = ${function:Assert-R8GoSummaryDocument}
    return [pscustomobject]@{
        Verify = {
            param([string]$Target, [string]$ExpectedStatus)
            if (-not (Test-Path -LiteralPath $Target -PathType Leaf)) {
                throw "R8 Go temporary summary is missing: $Target"
            }
            $raw = [IO.File]::ReadAllText($Target)
            if ([string]::IsNullOrWhiteSpace($raw)) {
                throw 'R8 Go temporary summary is empty'
            }
            $document = $raw | ConvertFrom-Json
            if ([string]$document.Status -cne $ExpectedStatus) {
                throw "R8 Go temporary summary status is $($document.Status), want $ExpectedStatus"
            }
            & $assertSummary $document $expected $contract $root $run
        }.GetNewClosure()
    }
}
