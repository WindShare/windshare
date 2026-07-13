Set-StrictMode -Version Latest

$script:D5TestEnumerationSchemaVersion = 1
$script:D5TestEnumerationOperation = 'go-test-enumeration'
$script:D5TestExecutionPlanSchemaVersion = 1
$script:D5TestExecutionPlanOperation = 'go-test-execution'
$script:D5TestExecutionPlanDigestDomain = 'd5-go-test-execution-plan-v1'
$script:D5TestEnumerationArguments = @('-test.list=^Test')
$script:D5NetworkAuthorityEnvironmentNames = @(
    'WINDSHARE_D5_AUTHORIZATION_PIPE',
    'WINDSHARE_WINDOWS_OS_NETWORK',
    'WINDSHARE_D5_HARNESS_CAPABILITY',
    'WINDSHARE_D5_AUTHORIZATION_MANIFEST',
    'WINDSHARE_D5_E2E_LEASE_TOKEN',
    'WINDSHARE_D5_RUNNER_PIPE',
    'WINDSHARE_D5_CHILD_MANIFEST'
)

function ConvertTo-D5BoundProgramEvidence([object]$Program) {
    if ($null -eq $Program) {
        throw 'A D5 test process requires parent-owned program evidence'
    }
    $path = [IO.Path]::GetFullPath([string]$Program.Path)
    $bytes = [long]$Program.Bytes
    $sha256 = ([string]$Program.SHA256).ToLowerInvariant()
    if (-not [IO.Path]::IsPathFullyQualified([string]$Program.Path) -or
        $bytes -le 0 -or
        $sha256 -notmatch '^[0-9a-f]{64}$') {
        throw 'D5 test process program evidence is invalid'
    }
    return [pscustomobject][ordered]@{
        Path = $path
        Bytes = $bytes
        SHA256 = $sha256
    }
}

function ConvertTo-D5BoundSourceIdentity([object]$Source) {
    if ($null -eq $Source) {
        throw 'A non-network enumeration requires an exact source identity'
    }
    $identityKind = [string]$Source.IdentityKind
    $commit = [string]$Source.Commit
    $sourceDigest = ([string]$Source.SourceDigest).ToLowerInvariant()
    if ($identityKind -notin @('git-commit', 'workspace-manifest') -or
        [string]::IsNullOrWhiteSpace($commit) -or
        $sourceDigest -notmatch '^[0-9a-f]{64}$') {
        throw 'Non-network enumeration source identity is invalid'
    }
    return [pscustomobject][ordered]@{
        IdentityKind = $identityKind
        Commit = $commit
        WorktreeClean = [bool]$Source.WorktreeClean
        SourceDigest = $sourceDigest
    }
}

function Get-D5TestProgramEvidence(
    [Parameter(Mandatory)] [object[]]$Programs,
    [Parameter(Mandatory)] [string]$Executable
) {
    $path = [IO.Path]::GetFullPath($Executable)
    $matches = @($Programs | Where-Object {
        [IO.Path]::GetFullPath([string]$_.Path).Equals(
            $path,
            [StringComparison]::OrdinalIgnoreCase
        )
    })
    if ($matches.Count -ne 1) {
        throw "The parent-owned process set does not contain exactly one record for $path"
    }
    return ConvertTo-D5BoundProgramEvidence $matches[0]
}

function Assert-D5ProgramEvidence(
    [Parameter(Mandatory)] [object]$Program,
    [Parameter(Mandatory)] [string]$Executable,
    [Parameter(Mandatory)] [string]$Context
) {
    $expected = ConvertTo-D5BoundProgramEvidence $Program
    $path = [IO.Path]::GetFullPath($Executable)
    if (-not $expected.Path.Equals($path, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Context executable path $path does not match parent-owned path $($expected.Path)"
    }
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "$Context executable is missing: $path"
    }
    $item = Get-Item -LiteralPath $path
    $sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ([long]$item.Length -ne $expected.Bytes -or $sha256 -ne $expected.SHA256) {
        throw "$Context executable differs from the parent-owned process set: $path"
    }
    return $expected
}

function Test-D5BoundSourceIdentityEqual([object]$Expected, [object]$Actual) {
    return [string]$Expected.IdentityKind -eq [string]$Actual.IdentityKind -and
        [string]$Expected.Commit -eq [string]$Actual.Commit -and
        [bool]$Expected.WorktreeClean -eq [bool]$Actual.WorktreeClean -and
        [string]$Expected.SourceDigest -eq [string]$Actual.SourceDigest
}

function Assert-D5EnumerationSourceIdentity(
    [Parameter(Mandatory)] [object]$Expected,
    [Parameter(Mandatory)] [object]$Actual,
    [Parameter(Mandatory)] [string]$Boundary
) {
    $boundExpected = ConvertTo-D5BoundSourceIdentity $Expected
    $boundActual = ConvertTo-D5BoundSourceIdentity $Actual
    if (-not (Test-D5BoundSourceIdentityEqual $boundExpected $boundActual)) {
        throw "Non-network enumeration source identity differs at $Boundary"
    }
}

function New-D5TestEnumerationOperation(
    [Parameter(Mandatory)] [string]$Executable,
    [Parameter(Mandatory)] [string]$WorkingDirectory,
    [Parameter(Mandatory)] [object]$ProgramEvidence,
    [Parameter(Mandatory)] [object]$SourceIdentity
) {
    $program = Assert-D5ProgramEvidence $ProgramEvidence $Executable 'Non-network enumeration'
    $workingPath = [IO.Path]::GetFullPath($WorkingDirectory)
    if (-not [IO.Path]::IsPathFullyQualified($WorkingDirectory) -or
        -not (Test-Path -LiteralPath $workingPath -PathType Container)) {
        throw "Non-network enumeration working directory is invalid: $WorkingDirectory"
    }
    return [pscustomobject][ordered]@{
        SchemaVersion = $script:D5TestEnumerationSchemaVersion
        Operation = $script:D5TestEnumerationOperation
        Authorization = 'none'
        Executable = $program
        WorkingDirectory = $workingPath
        Arguments = @($script:D5TestEnumerationArguments)
        Source = ConvertTo-D5BoundSourceIdentity $SourceIdentity
        DeniedEnvironmentNames = @($script:D5NetworkAuthorityEnvironmentNames)
    }
}

function Assert-D5TestEnumerationOperation([object]$Operation) {
    if ($null -eq $Operation -or
        [int]$Operation.SchemaVersion -ne $script:D5TestEnumerationSchemaVersion -or
        [string]$Operation.Operation -ne $script:D5TestEnumerationOperation -or
        [string]$Operation.Authorization -ne 'none') {
        throw 'Non-network enumeration operation identity is invalid'
    }
    $arguments = @($Operation.Arguments)
    if ($arguments.Count -ne $script:D5TestEnumerationArguments.Count) {
        throw 'Non-network enumeration only permits the exact Go test-list arguments'
    }
    for ($index = 0; $index -lt $arguments.Count; $index++) {
        if (-not ([string]$arguments[$index]).Equals(
            [string]$script:D5TestEnumerationArguments[$index],
            [StringComparison]::Ordinal
        )) {
            throw 'Non-network enumeration only permits the exact Go test-list arguments'
        }
    }
    $denied = @($Operation.DeniedEnvironmentNames)
    if ($denied.Count -ne $script:D5NetworkAuthorityEnvironmentNames.Count) {
        throw 'Non-network enumeration authority-denial contract is invalid'
    }
    for ($index = 0; $index -lt $denied.Count; $index++) {
        if (-not ([string]$denied[$index]).Equals(
            [string]$script:D5NetworkAuthorityEnvironmentNames[$index],
            [StringComparison]::Ordinal
        )) {
            throw 'Non-network enumeration authority-denial contract is invalid'
        }
    }
    $workingPath = [IO.Path]::GetFullPath([string]$Operation.WorkingDirectory)
    if (-not [IO.Path]::IsPathFullyQualified([string]$Operation.WorkingDirectory) -or
        -not $workingPath.Equals([string]$Operation.WorkingDirectory, [StringComparison]::OrdinalIgnoreCase) -or
        -not (Test-Path -LiteralPath $workingPath -PathType Container)) {
        throw 'Non-network enumeration working directory is invalid'
    }
    $program = Assert-D5ProgramEvidence `
        $Operation.Executable `
        ([string]$Operation.Executable.Path) `
        'Non-network enumeration'
    $source = ConvertTo-D5BoundSourceIdentity $Operation.Source
    return [pscustomobject][ordered]@{
        SchemaVersion = $script:D5TestEnumerationSchemaVersion
        Operation = $script:D5TestEnumerationOperation
        Authorization = 'none'
        Executable = $program
        WorkingDirectory = $workingPath
        Arguments = @($script:D5TestEnumerationArguments)
        Source = $source
        DeniedEnvironmentNames = @($script:D5NetworkAuthorityEnvironmentNames)
    }
}

function Get-D5TestExecutionPlanSHA256([object]$Plan) {
    $entries = @($Plan.Entries)
    $arguments = @($Plan.Arguments)
    $fields = [Collections.Generic.List[string]]::new()
    foreach ($field in @(
        $script:D5TestExecutionPlanDigestDomain,
        [string]$Plan.SchemaVersion,
        [string]$Plan.Operation,
        [string]$Plan.RequestID,
        [string]$Plan.RunID,
        [string]$Plan.PackagePath,
        [string]$Plan.NetworkAccess,
        [string]$Plan.SelectionClass,
        [string]$Plan.Source.IdentityKind,
        [string]$Plan.Source.Commit,
        ([bool]$Plan.Source.WorktreeClean).ToString().ToLowerInvariant(),
        [string]$Plan.Source.SourceDigest,
        [string]$Plan.Executable.Path,
        [string]$Plan.Executable.Bytes,
        [string]$Plan.Executable.SHA256,
        [string]$Plan.WorkingDirectory,
        [string]$arguments.Count
    )) {
        $fields.Add([string]$field)
    }
    foreach ($argument in $arguments) {
        $fields.Add([string]$argument)
    }
    $fields.Add(([bool]$Plan.LifecycleRequiresOSNetwork).ToString().ToLowerInvariant())
    $fields.Add([string]$entries.Count)
    foreach ($entry in $entries) {
        $fields.Add([string]$entry.Kind)
        $fields.Add([string]$entry.Name)
        $fields.Add(([bool]$entry.RequiresOSNetwork).ToString().ToLowerInvariant())
    }
    $encoding = [Text.UTF8Encoding]::new($false)
    $payload = [Text.StringBuilder]::new()
    foreach ($field in $fields) {
        [void]$payload.Append($encoding.GetByteCount($field))
        [void]$payload.Append(':')
        [void]$payload.Append($field)
        [void]$payload.Append("`n")
    }
    $digest = [Security.Cryptography.SHA256]::HashData(
        $encoding.GetBytes($payload.ToString())
    )
    return [Convert]::ToHexString($digest).ToLowerInvariant()
}

function Assert-D5TestExecutionPlan(
    [Parameter(Mandatory)] [object]$Plan,
    [Parameter(Mandatory)] [object[]]$ParentPrograms,
    [Parameter(Mandatory)] [object]$ExpectedSourceIdentity,
    [Parameter(Mandatory)] [string]$ExpectedRunID,
    [Parameter(Mandatory)] [string]$ExpectedRequestID,
    [Parameter(Mandatory)] [string]$ExpectedPlanSHA256
) {
    $expectedDigest = $ExpectedPlanSHA256.ToLowerInvariant()
    if ($null -eq $Plan -or
        [int]$Plan.SchemaVersion -ne $script:D5TestExecutionPlanSchemaVersion -or
        [string]$Plan.Operation -cne $script:D5TestExecutionPlanOperation -or
        [string]$Plan.RequestID -cne $ExpectedRequestID -or
        [string]$Plan.RunID -cne $ExpectedRunID -or
        $expectedDigest -notmatch '^[0-9a-f]{64}$' -or
        ([string]$Plan.PlanSHA256).ToLowerInvariant() -cne $expectedDigest) {
        throw 'Test execution-plan identity is invalid or stale'
    }
    $source = ConvertTo-D5BoundSourceIdentity $Plan.Source
    Assert-D5EnumerationSourceIdentity `
        $ExpectedSourceIdentity `
        $source `
        'test execution-plan validation'

    $program = Assert-D5ProgramEvidence `
        $Plan.Executable `
        ([string]$Plan.Executable.Path) `
        'Test execution-plan'
    $parentProgram = Get-D5TestProgramEvidence `
        $ParentPrograms `
        $program.Path
    if ($parentProgram.Bytes -ne $program.Bytes -or
        $parentProgram.SHA256 -cne $program.SHA256) {
        throw 'Test execution-plan executable differs from the parent-owned program set'
    }

    $workingPath = [IO.Path]::GetFullPath([string]$Plan.WorkingDirectory)
    if (-not [IO.Path]::IsPathFullyQualified([string]$Plan.WorkingDirectory) -or
        -not $workingPath.Equals(
            [string]$Plan.WorkingDirectory,
            [StringComparison]::OrdinalIgnoreCase
        ) -or
        -not (Test-Path -LiteralPath $workingPath -PathType Container)) {
        throw 'Test execution-plan working directory is invalid'
    }
    if ([string]::IsNullOrWhiteSpace([string]$Plan.PackagePath)) {
        throw 'Test execution-plan package identity is invalid'
    }

    $entries = @($Plan.Entries)
    $normalizedEntries = [Collections.Generic.List[object]]::new()
    $seenEntries = @{}
    $hasNetworkEntry = $false
    $hasPureEntry = $false
    foreach ($entry in $entries) {
        $kind = [string]$entry.Kind
        $name = [string]$entry.Name
        if ($kind -notin @('test', 'benchmark') -or
            [string]::IsNullOrWhiteSpace($name)) {
            throw 'Test execution-plan contains an invalid semantic entry'
        }
        $entryKey = $kind + [char]0 + $name
        if ($seenEntries.ContainsKey($entryKey)) {
            throw 'Test execution-plan repeats a semantic entry'
        }
        $seenEntries[$entryKey] = $true
        $requiresNetwork = [bool]$entry.RequiresOSNetwork
        $hasNetworkEntry = $hasNetworkEntry -or $requiresNetwork
        $hasPureEntry = $hasPureEntry -or -not $requiresNetwork
        $normalizedEntries.Add([pscustomobject][ordered]@{
            Kind = $kind
            Name = $name
            RequiresOSNetwork = $requiresNetwork
        })
    }
    $lifecycleNetwork = [bool]$Plan.LifecycleRequiresOSNetwork
    $hasNetwork = $lifecycleNetwork -or $hasNetworkEntry
    if ($hasNetwork) {
        $expectedAccess = 'parent-owned-one-use-pipe'
        $expectedClass = if ($lifecycleNetwork -and -not $hasNetworkEntry) {
            'network-lifecycle'
        } elseif ($hasPureEntry) {
            'mixed-network'
        } else {
            'network'
        }
    } else {
        $expectedAccess = 'none'
        $expectedClass = if ($entries.Count -eq 0) { 'empty' } else { 'non-network' }
    }
    if ([string]$Plan.NetworkAccess -cne $expectedAccess -or
        [string]$Plan.SelectionClass -cne $expectedClass) {
        throw 'Test execution-plan network selection does not match its compiler-derived entries'
    }

    $computedDigest = Get-D5TestExecutionPlanSHA256 $Plan
    if ($computedDigest -cne $expectedDigest) {
        throw 'Test execution-plan digest does not match its bound source, executable, or arguments'
    }
    return [pscustomobject][ordered]@{
        SchemaVersion = $script:D5TestExecutionPlanSchemaVersion
        Operation = $script:D5TestExecutionPlanOperation
        PlanSHA256 = $expectedDigest
        RequestID = [string]$Plan.RequestID
        RunID = [string]$Plan.RunID
        PackagePath = [string]$Plan.PackagePath
        NetworkAccess = $expectedAccess
        SelectionClass = $expectedClass
        LifecycleRequiresOSNetwork = $lifecycleNetwork
        Executable = $program
        WorkingDirectory = $workingPath
        Arguments = @($Plan.Arguments | ForEach-Object { [string]$_ })
        Source = $source
        Entries = @($normalizedEntries)
    }
}

function New-D5TestProcessStartInfo(
    [Parameter(Mandatory)] [string]$Executable,
    [Parameter(Mandatory)] [AllowEmptyCollection()] [string[]]$Arguments,
    [Parameter(Mandatory)] [string]$WorkingDirectory,
    [string[]]$DeniedEnvironmentNames = @(),
    [Collections.IDictionary]$EnvironmentOverrides = @{}
) {
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = [IO.Path]::GetFullPath($Executable)
    $start.WorkingDirectory = [IO.Path]::GetFullPath($WorkingDirectory)
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $start.StandardOutputEncoding = [Text.UTF8Encoding]::new($false)
    $start.StandardErrorEncoding = [Text.UTF8Encoding]::new($false)
    foreach ($name in $DeniedEnvironmentNames) {
        [void]$start.Environment.Remove($name)
    }
    foreach ($entry in $EnvironmentOverrides.GetEnumerator()) {
        $start.Environment[[string]$entry.Key] = [string]$entry.Value
    }
    foreach ($argument in $Arguments) {
        $start.ArgumentList.Add($argument)
    }
    return $start
}

function Write-D5CapturedProcessOutput(
    [string]$StandardOutput,
    [string]$StandardError,
    [string]$LogPath
) {
    $combined = $StandardOutput
    if (-not [string]::IsNullOrWhiteSpace($StandardError)) {
        if (-not [string]::IsNullOrEmpty($combined) -and -not $combined.EndsWith("`n")) {
            $combined += [Environment]::NewLine
        }
        $combined += $StandardError
    }
    if (-not [string]::IsNullOrWhiteSpace($LogPath)) {
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $LogPath) | Out-Null
        [IO.File]::WriteAllText($LogPath, $combined, [Text.UTF8Encoding]::new($false))
    }
    if (-not [string]::IsNullOrEmpty($combined)) {
        [Console]::Out.Write($combined)
    }
}

function Invoke-D5BoundNonNetworkProcess(
    [Parameter(Mandatory)] [object]$ExecutableEvidence,
    [Parameter(Mandatory)] [AllowEmptyCollection()] [string[]]$Arguments,
    [Parameter(Mandatory)] [string]$WorkingDirectory,
    [Parameter(Mandatory)] [string]$Context,
    [Parameter(Mandatory)] [string[]]$DeniedEnvironmentNames,
    [string]$LogPath = ''
) {
    $program = Assert-D5ProgramEvidence `
        $ExecutableEvidence `
        ([string]$ExecutableEvidence.Path) `
        $Context
    $start = New-D5TestProcessStartInfo `
        $program.Path `
        $Arguments `
        $WorkingDirectory `
        $DeniedEnvironmentNames
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $start
    $started = $false
    try {
        if (-not $process.Start()) {
            throw "Could not start $Context executable $($program.Path)"
        }
        $started = $true
        $childPID = $process.Id
        $actualPath = [IO.Path]::GetFullPath($process.MainModule.FileName)
        if (-not $actualPath.Equals($program.Path, [StringComparison]::OrdinalIgnoreCase)) {
            throw "$Context PID $childPID executable $actualPath does not match $($program.Path)"
        }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        $process.WaitForExit()
        $stdout = $stdoutTask.GetAwaiter().GetResult()
        $stderr = $stderrTask.GetAwaiter().GetResult()
        [void](Assert-D5ProgramEvidence $program $program.Path "Completed $Context")
        Write-D5CapturedProcessOutput $stdout $stderr $LogPath
        return [pscustomobject]@{
            ExitCode = $process.ExitCode
            StandardOutput = $stdout
            StandardError = $stderr
            ChildPID = $childPID
            Executable = $program
        }
    } catch {
        if ($started -and -not $process.HasExited) {
            $process.Kill($true)
            $process.WaitForExit()
        }
        throw
    } finally {
        $process.Dispose()
    }
}

function Invoke-D5TestEnumerationProcess(
    [Parameter(Mandatory)] [object]$Operation,
    [Parameter(Mandatory)] [object[]]$ParentPrograms,
    [Parameter(Mandatory)] [object]$ExpectedSourceIdentity,
    [Parameter(Mandatory)] [scriptblock]$SourceCheckpoint,
    [string]$LogPath = ''
) {
    $validated = Assert-D5TestEnumerationOperation $Operation
    $parentProgram = Get-D5TestProgramEvidence $ParentPrograms $validated.Executable.Path
    if ($parentProgram.Bytes -ne $validated.Executable.Bytes -or
        $parentProgram.SHA256 -ne $validated.Executable.SHA256) {
        throw 'Non-network enumeration operation differs from the parent-owned program set'
    }
    Assert-D5EnumerationSourceIdentity `
        $ExpectedSourceIdentity `
        $validated.Source `
        'operation construction'
    $sourceBefore = & $SourceCheckpoint 'before'
    Assert-D5EnumerationSourceIdentity $ExpectedSourceIdentity $sourceBefore 'before launch'
    $result = Invoke-D5BoundNonNetworkProcess `
        -ExecutableEvidence $validated.Executable `
        -Arguments @($validated.Arguments) `
        -WorkingDirectory $validated.WorkingDirectory `
        -Context 'non-network enumeration' `
        -DeniedEnvironmentNames @($validated.DeniedEnvironmentNames) `
        -LogPath $LogPath
    $sourceAfter = & $SourceCheckpoint 'after'
    Assert-D5EnumerationSourceIdentity $ExpectedSourceIdentity $sourceAfter 'after completion'
    return [pscustomobject]@{
        ExitCode = $result.ExitCode
        StandardOutput = $result.StandardOutput
        StandardError = $result.StandardError
        OperationRecord = [pscustomobject][ordered]@{
            SchemaVersion = $validated.SchemaVersion
            Operation = $validated.Operation
            Authorization = 'none'
            ParentPID = $PID
            ChildPID = $result.ChildPID
            Executable = $validated.Executable
            WorkingDirectory = $validated.WorkingDirectory
            Arguments = @($validated.Arguments)
            Source = $validated.Source
            DeniedEnvironmentNames = @($validated.DeniedEnvironmentNames)
        }
    }
}

function Complete-D5PlannedLaunchAuthorization(
    [Parameter(Mandatory)] [object]$Authorization,
    [Parameter(Mandatory)] [Diagnostics.Process]$Process,
    [Parameter(Mandatory)] [string]$ExpectedExecutable
) {
    if ([string]$Authorization.State -ne 'AwaitingConnection') {
        throw 'The planned one-use launch authorization is no longer awaiting its process'
    }
    $exitTask = $Process.WaitForExitAsync()
    $tasks = [Threading.Tasks.Task[]]@($Authorization.Connection, $exitTask)
    [void][Threading.Tasks.Task]::WhenAny($tasks).GetAwaiter().GetResult()
    if ($Authorization.Connection.IsCompleted) {
        Complete-D5LaunchAuthorization $Authorization $Process $ExpectedExecutable
        return $true
    }
    # A plan grants only the opportunity to cross the gate. A subtest filter or
    # runtime branch may finish without consuming that opportunity; the terminal
    # unused state cannot be reused by a later process.
    $Authorization.State = 'UnusedNoGate'
    return $false
}

function Invoke-D5NetworkAuthorizedTestProcess(
    [Parameter(Mandatory)] [string]$RunID,
    [Parameter(Mandatory)] [object[]]$Programs,
    [Parameter(Mandatory)] [string]$Executable,
    [Parameter(Mandatory)] [AllowEmptyCollection()] [string[]]$Arguments,
    [Parameter(Mandatory)] [string]$WorkingDirectory,
    [string]$LogPath = '',
    [switch]$AllowUnusedAuthorization
) {
    $program = Get-D5TestProgramEvidence $Programs $Executable
    [void](Assert-D5ProgramEvidence $program $Executable 'Network-authorized execution')
    $authorization = $null
    $process = $null
    $started = $false
    try {
        $authorization = New-D5LaunchAuthorization $RunID $Programs
        $start = New-D5TestProcessStartInfo `
            $Executable `
            $Arguments `
            $WorkingDirectory `
            @($script:D5NetworkAuthorityEnvironmentNames) `
            @{ 'WINDSHARE_D5_AUTHORIZATION_PIPE' = $authorization.Name }
        $process = [Diagnostics.Process]::new()
        $process.StartInfo = $start
        if (-not $process.Start()) {
            throw "Could not start network-authorized executable $Executable"
        }
        $started = $true
        $expectedPath = [IO.Path]::GetFullPath($Executable)
        $actualPath = $expectedPath
        try {
            if (-not $process.HasExited) {
                $mainModule = $process.MainModule
                if ($null -ne $mainModule) {
                    $actualPath = [IO.Path]::GetFullPath($mainModule.FileName)
                }
            }
        } catch {
            if (-not $process.HasExited) {
                throw
            }
        }
        if (-not $actualPath.Equals($expectedPath, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Network-capable PID $($process.Id) executable $actualPath does not match $expectedPath"
        }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        $authorizationConsumed = if ($AllowUnusedAuthorization) {
            Complete-D5PlannedLaunchAuthorization $authorization $process $Executable
        } else {
            Complete-D5LaunchAuthorization $authorization $process $Executable
            $true
        }
        $process.WaitForExit()
        $stdout = $stdoutTask.GetAwaiter().GetResult()
        $stderr = $stderrTask.GetAwaiter().GetResult()
        [void](Assert-D5ProgramEvidence $program $Executable 'Completed network-authorized execution')
        Write-D5CapturedProcessOutput $stdout $stderr $LogPath
        return [pscustomobject]@{
            ExitCode = $process.ExitCode
            StandardOutput = $stdout
            StandardError = $stderr
            AuthorizationRecord = [pscustomobject][ordered]@{
                RunID = $authorization.RunID
                Authorization = 'parent-owned-one-use-pipe'
                Disposition = if ($authorizationConsumed) { 'Consumed' } else { 'UnusedNoGate' }
                ParentPID = $PID
                ChildPID = $process.Id
                Executable = $actualPath
                Arguments = @($Arguments)
                SHA256 = $program.SHA256
            }
        }
    } catch {
        if ($started -and -not $process.HasExited) {
            $process.Kill($true)
            $process.WaitForExit()
        }
        throw
    } finally {
        Release-D5LaunchAuthorization $authorization
        if ($null -ne $process) {
            $process.Dispose()
        }
    }
}

function Invoke-D5PlannedTestProcess(
    [Parameter(Mandatory)] [object]$Plan,
    [Parameter(Mandatory)] [object[]]$ParentPrograms,
    [Parameter(Mandatory)] [object]$ExpectedSourceIdentity,
    [Parameter(Mandatory)] [string]$ExpectedRunID,
    [Parameter(Mandatory)] [string]$ExpectedRequestID,
    [Parameter(Mandatory)] [string]$ExpectedPlanSHA256,
    [Parameter(Mandatory)] [scriptblock]$SourceCheckpoint,
    [string]$LogPath = ''
) {
    $validated = Assert-D5TestExecutionPlan `
        -Plan $Plan `
        -ParentPrograms $ParentPrograms `
        -ExpectedSourceIdentity $ExpectedSourceIdentity `
        -ExpectedRunID $ExpectedRunID `
        -ExpectedRequestID $ExpectedRequestID `
        -ExpectedPlanSHA256 $ExpectedPlanSHA256
    $sourceBefore = & $SourceCheckpoint 'before'
    Assert-D5EnumerationSourceIdentity `
        $ExpectedSourceIdentity `
        $sourceBefore `
        'before planned test execution'

    $authorizationRecord = $null
    if ($validated.NetworkAccess -eq 'none') {
        $result = Invoke-D5BoundNonNetworkProcess `
            -ExecutableEvidence $validated.Executable `
            -Arguments @($validated.Arguments) `
            -WorkingDirectory $validated.WorkingDirectory `
            -Context 'compiler-planned non-network test execution' `
            -DeniedEnvironmentNames @($script:D5NetworkAuthorityEnvironmentNames) `
            -LogPath $LogPath
        $disposition = 'CompletedWithoutNetworkAuthority'
    } else {
        $result = Invoke-D5NetworkAuthorizedTestProcess `
            -RunID $validated.RunID `
            -Programs $ParentPrograms `
            -Executable $validated.Executable.Path `
            -Arguments @($validated.Arguments) `
            -WorkingDirectory $validated.WorkingDirectory `
            -LogPath $LogPath `
            -AllowUnusedAuthorization
        $authorizationRecord = $result.AuthorizationRecord
        $disposition = [string]$authorizationRecord.Disposition
    }

    $sourceAfter = & $SourceCheckpoint 'after'
    Assert-D5EnumerationSourceIdentity `
        $ExpectedSourceIdentity `
        $sourceAfter `
        'after planned test execution'
    $record = [pscustomobject][ordered]@{
        SchemaVersion = $validated.SchemaVersion
        Operation = $validated.Operation
        PlanSHA256 = $validated.PlanSHA256
        RequestID = $validated.RequestID
        RunID = $validated.RunID
        PackagePath = $validated.PackagePath
        NetworkAccess = $validated.NetworkAccess
        SelectionClass = $validated.SelectionClass
        Authorization = if ($validated.NetworkAccess -eq 'none') {
            'none'
        } else {
            'parent-owned-one-use-pipe'
        }
        Disposition = $disposition
        ParentPID = $PID
        ChildPID = if ($null -eq $authorizationRecord) {
            $result.ChildPID
        } else {
            $authorizationRecord.ChildPID
        }
        Executable = $validated.Executable
        WorkingDirectory = $validated.WorkingDirectory
        Arguments = @($validated.Arguments)
        Source = $validated.Source
        LifecycleRequiresOSNetwork = $validated.LifecycleRequiresOSNetwork
        Entries = @($validated.Entries)
    }
    return [pscustomobject]@{
        ExitCode = $result.ExitCode
        StandardOutput = $result.StandardOutput
        StandardError = $result.StandardError
        ExecutionRecord = $record
        AuthorizationRecord = $authorizationRecord
    }
}
