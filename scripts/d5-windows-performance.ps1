[CmdletBinding()]
param(
    [ValidateSet('Audit', 'Build', 'OrdinaryTests', 'NetworkTests', 'BrowserTests', 'Baseline', 'Profile')]
    [string]$Mode = 'OrdinaryTests',

    [ValidatePattern('^[1-9][0-9]*(ns|us|ms|s|m|h)$')]
    [string]$BenchTime = '2s',

    [ValidateRange(1, 100)]
    [int]$Count = 5,

    [switch]$Race,

    [string]$EvidenceRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    throw 'The D5 stable Windows network runner is Windows-only.'
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$harnessRoot = Join-Path $repositoryRoot 'tmp\d5-harness'
$stableChildRoot = Join-Path $harnessRoot 'e2e-bin'
$registrationStatePath = Join-Path $harnessRoot '.firewall-registration-state.json'
$firewallOwnershipEvidencePath = Join-Path $PSScriptRoot 'd5-windows-firewall-ownership.json'
if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
    $EvidenceRoot = Join-Path $repositoryRoot 'tmp\d5-evidence'
}
$firewallLogName = 'Microsoft-Windows-Windows Firewall With Advanced Security/Firewall'
$firewallRuleEventIDs = @(2052, 2097, 2099)
$firewallQuietMilliseconds = 500
$firewallQuiescenceTimeout = [timespan]::FromSeconds(10)
$firewallObservedRoots = @(
    [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    [IO.Path]::GetFullPath((Join-Path $env:LOCALAPPDATA 'go-build'))
    [IO.Path]::GetFullPath($harnessRoot)
)
$browserNetworkContractName = 'WINDSHARE_WINDOWS_OS_NETWORK'
$browserNetworkContractValue = 'stable-harness-v3'
$browserLeaseTokenName = 'WINDSHARE_D5_E2E_LEASE_TOKEN'
$launchAuthorizationPipeName = 'WINDSHARE_D5_AUTHORIZATION_PIPE'
$runnerGuardName = 'WINDSHARE_D5_RUNNER_PIPE'

. (Join-Path $PSScriptRoot 'd5-windows-firewall-audit.ps1')
. (Join-Path $PSScriptRoot 'd5-evidence.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-stable-e2e-lease.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-runner-guard.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-test-process.ps1')

function Test-D5RelevantProgram([string]$Program) {
    if ([string]::IsNullOrWhiteSpace($Program) -or
        $null -eq $script:firewallOwnershipPolicy) {
        return $false
    }
    try {
        $expanded = [Environment]::ExpandEnvironmentVariables($Program)
        return Test-D5ProgramObserved $expanded $script:firewallOwnershipPolicy
    } catch {
        return $false
    }
}

function Get-D5FirewallRules {
    $snapshots = [Collections.Generic.List[object]]::new()
    $programStates = @{}
    foreach ($filter in @(Get-NetFirewallApplicationFilter -PolicyStore ActiveStore)) {
        $program = [Environment]::ExpandEnvironmentVariables([string]$filter.Program)
        if (-not (Test-D5RelevantProgram $program)) {
            continue
        }
        $program = [IO.Path]::GetFullPath($program)
        if (-not $programStates.ContainsKey($program)) {
            $exists = Test-Path -LiteralPath $program -PathType Leaf
            $programStates[$program] = [pscustomobject]@{
                Exists = $exists
                SHA256 = if ($exists) {
                    (Get-FileHash -LiteralPath $program -Algorithm SHA256).Hash.ToLowerInvariant()
                } else {
                    ''
                }
            }
        }
        $programState = $programStates[$program]
        foreach ($rule in @($filter | Get-NetFirewallRule -PolicyStore ActiveStore)) {
            $ports = @($rule | Get-NetFirewallPortFilter)
            $addresses = @($rule | Get-NetFirewallAddressFilter)
            $snapshots.Add([pscustomobject][ordered]@{
                RuleID = [string]$rule.Name
                InstanceID = [string]$filter.InstanceID
                Program = $program
                Direction = ConvertTo-D5SemanticValue $rule.Direction
                Action = ConvertTo-D5SemanticValue $rule.Action
                Profile = ConvertTo-D5SemanticValue $rule.Profile
                Enabled = ConvertTo-D5SemanticValue $rule.Enabled
                PolicyStoreSourceType = ConvertTo-D5SemanticValue $rule.PolicyStoreSourceType
                Protocol = ConvertTo-D5SemanticValue @($ports.Protocol)
                LocalPort = ConvertTo-D5SemanticValue @($ports.LocalPort)
                RemotePort = ConvertTo-D5SemanticValue @($ports.RemotePort)
                LocalAddress = ConvertTo-D5SemanticValue @($addresses.LocalAddress)
                RemoteAddress = ConvertTo-D5SemanticValue @($addresses.RemoteAddress)
                ProgramExists = [bool]$programState.Exists
                ProgramSHA256 = [string]$programState.SHA256
            })
        }
    }
    return @($snapshots | Sort-Object Program, RuleID, InstanceID)
}

function Get-D5LatestFirewallRecordID {
    $event = Get-WinEvent -LogName $firewallLogName -MaxEvents 1 -ErrorAction Stop
    return [long]$event.RecordId
}

function Get-D5RelevantFirewallEvents([long]$AfterRecordID, [long]$ThroughRecordID) {
    if ($ThroughRecordID -le $AfterRecordID) {
        return @()
    }
    $eventPredicate = @($firewallRuleEventIDs | ForEach-Object { "EventID=$_" }) -join ' or '
    $xpath = "*[System[(EventRecordID > $AfterRecordID) and (EventRecordID <= $ThroughRecordID) and ($eventPredicate)]]"
    $query = [Diagnostics.Eventing.Reader.EventLogQuery]::new(
        $firewallLogName,
        [Diagnostics.Eventing.Reader.PathType]::LogName,
        $xpath
    )
    $reader = [Diagnostics.Eventing.Reader.EventLogReader]::new($query)
    $relevant = [Collections.Generic.List[object]]::new()
    try {
        while ($null -ne ($event = $reader.ReadEvent())) {
            try {
                [xml]$document = $event.ToXml()
                $fields = [ordered]@{}
                $programs = [Collections.Generic.List[string]]::new()
                foreach ($data in $document.Event.EventData.Data) {
                    $name = [string]$data.Name
                    $value = [string]$data.InnerText
                    if (-not [string]::IsNullOrWhiteSpace($name)) {
                        $fields[$name] = $value
                    }
                    if (Test-D5RelevantProgram $value) {
                        $programs.Add([IO.Path]::GetFullPath(
                            [Environment]::ExpandEnvironmentVariables($value)
                        ))
                    }
                }
                if ($programs.Count -gt 0) {
                    $relevant.Add([pscustomobject][ordered]@{
                        RecordID = [long]$event.RecordId
                        EventID = [int]$event.Id
                        TimeCreated = $event.TimeCreated
                        Programs = @($programs | Sort-Object -Unique)
                        Fields = [pscustomobject]$fields
                    })
                }
            } finally {
                $event.Dispose()
            }
        }
    } finally {
        $reader.Dispose()
    }
    return @($relevant | Sort-Object RecordID)
}

function Wait-D5FirewallQuiescence([long]$AfterRecordID) {
    $deadline = [datetimeoffset]::Now.Add($firewallQuiescenceTimeout)
    $previousRules = @(Get-D5FirewallRules)
    $previousRecordID = Get-D5LatestFirewallRecordID
    do {
        Start-Sleep -Milliseconds $firewallQuietMilliseconds
        $currentRules = @(Get-D5FirewallRules)
        $currentRecordID = Get-D5LatestFirewallRecordID
        if ($currentRecordID -eq $previousRecordID -and
            (Test-D5RuleSetsEqual $previousRules $currentRules)) {
            return [pscustomobject]@{
                AfterRecordID = $currentRecordID
                AfterRules = $currentRules
                NewRelevantEvents = @(
                    Get-D5RelevantFirewallEvents $AfterRecordID $currentRecordID
                )
            }
        }
        $previousRules = $currentRules
        $previousRecordID = $currentRecordID
    } while ([datetimeoffset]::Now -lt $deadline)
    throw 'Windows Firewall did not reach the bounded D5 quiescence interval'
}

function Write-D5JSON([string]$Path, [object]$Value) {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Path) | Out-Null
    [IO.File]::WriteAllText(
        $Path,
        ($Value | ConvertTo-Json -Depth 16),
        [Text.UTF8Encoding]::new($false)
    )
}

function Save-D5FirewallRegistrationState([object]$State) {
    Assert-D5HarnessLeaseHeld $script:harnessLease
    $temporary = "$registrationStatePath.building-$($script:ownerID)"
    try {
        Write-D5JSON $temporary $State
        Assert-D5HarnessLeaseHeld $script:harnessLease
        [IO.File]::Move($temporary, $registrationStatePath, $true)
    } finally {
        [IO.File]::Delete($temporary)
    }
}

function Open-D5FirewallRegistrationState([object[]]$Rules) {
    if (Test-Path -LiteralPath $registrationStatePath -PathType Leaf) {
        $state = Get-Content -LiteralPath $registrationStatePath -Raw | ConvertFrom-Json
        Assert-D5FirewallRegistrationState `
            $state `
            $Rules `
            $script:firewallOwnershipPolicy `
            $script:firewallOwnershipBaseline
        return $state
    }
    $state = New-D5FirewallRegistrationState `
        $Rules `
        $script:firewallOwnershipPolicy `
        $script:firewallOwnershipBaseline
    Save-D5FirewallRegistrationState $state
    return $state
}

function New-D5CurrentFirewallOwnershipPolicy {
    $currentHashes = @(
        foreach ($program in $script:authorizedStablePrograms) {
            if (Test-Path -LiteralPath $program -PathType Leaf) {
                (Get-FileHash -LiteralPath $program -Algorithm SHA256).Hash.ToLowerInvariant()
            }
        }
    )
    return New-D5FirewallOwnershipPolicy `
        $harnessRoot `
        $script:authorizedStablePrograms `
        $script:firewallOwnershipEvidence `
        $currentHashes `
        $firewallObservedRoots
}

function Get-D5BinaryEvidence([string[]]$Programs) {
    return @(
        foreach ($program in $Programs | Sort-Object -Unique) {
            $path = [IO.Path]::GetFullPath($program)
            if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
                throw "Expected stable binary is missing: $path"
            }
            $item = Get-Item -LiteralPath $path
            [ordered]@{
                Path = $path
                Bytes = [long]$item.Length
                SHA256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
            }
        }
    )
}

function Assert-D5BinaryEvidenceEqual(
    [object[]]$Expected,
    [object[]]$Actual,
    [string]$Phase
) {
    $expectedByPath = @{}
    foreach ($entry in $Expected) {
        $expectedByPath[[IO.Path]::GetFullPath([string]$entry.Path)] = $entry
    }
    $actualByPath = @{}
    foreach ($entry in $Actual) {
        $actualByPath[[IO.Path]::GetFullPath([string]$entry.Path)] = $entry
    }
    if ($expectedByPath.Count -ne $actualByPath.Count -or
        @(Compare-Object -ReferenceObject @($expectedByPath.Keys | Sort-Object) -DifferenceObject @($actualByPath.Keys | Sort-Object)).Count -ne 0) {
        throw "$Phase binary program set differs from the runner-owned manifest"
    }
    foreach ($path in $expectedByPath.Keys) {
        $expectedEntry = $expectedByPath[$path]
        $actualEntry = $actualByPath[$path]
        if ([long]$expectedEntry.Bytes -ne [long]$actualEntry.Bytes -or
            [string]$expectedEntry.SHA256 -ne [string]$actualEntry.SHA256) {
            throw "$Phase binary differs from the runner-owned manifest: $path"
        }
    }
}

function Assert-D5RunProgramsUnchanged([string]$Phase) {
    if (@($script:initialProgramEvidence).Count -eq 0) {
        return
    }
    $actual = @(Get-D5BinaryEvidence @($script:initialProgramEvidence.Path))
    Assert-D5BinaryEvidenceEqual @($script:initialProgramEvidence) $actual $Phase
}

function Invoke-D5PlannedBinary([object]$Definition, [string]$Phase) {
    $name = [string]$Definition.Name
    $requestID = [string]$Definition.RequestID
    $binding = $script:executionPlans[$requestID]
    if ($null -eq $binding) {
        throw "Compiler execution plan is missing for $requestID"
    }
    Assert-D5HarnessLeaseHeld $script:harnessLease
    Assert-D5RunProgramsUnchanged "before launch of $name"
    [void](Add-D5SourceCheckpoint $script:run "before-launch-$name")
    $relativeLog = ([string]$Definition.LogName).Replace('{phase}', $Phase)
    $logPath = Join-Path $script:run.StagePath $relativeLog
    $executionRun = $script:run
    $executionID = $requestID
    # GetNewClosure binds the block to a fresh dynamic module that resolves
    # commands through the global scope, so script-scoped functions must be
    # captured as variables to survive CI's dot-sourcing pwsh step wrapper.
    $addSourceCheckpoint = ${function:Add-D5SourceCheckpoint}
    $checkpoint = {
        param([string]$Boundary)
        & $addSourceCheckpoint $executionRun "$Boundary-execution-$executionID"
    }.GetNewClosure()
    $result = Invoke-D5PlannedTestProcess `
        -Plan $binding.Plan `
        -ParentPrograms @($script:initialProgramEvidence) `
        -ExpectedSourceIdentity $script:run.SourceAtStart `
        -ExpectedRunID $script:ownerID `
        -ExpectedRequestID $requestID `
        -ExpectedPlanSHA256 $binding.PlanSHA256 `
        -SourceCheckpoint $checkpoint `
        -LogPath $logPath
    $script:executionRecords.Add($result.ExecutionRecord)
    if ($null -ne $result.AuthorizationRecord) {
        $script:authorizationRecords.Add($result.AuthorizationRecord)
    }
    [void](Add-D5SourceCheckpoint $script:run "after-launch-$name")
    Assert-D5RunProgramsUnchanged "after launch of $name"
    if ($result.ExitCode -ne 0) {
        throw "$name exited with code $($result.ExitCode); see $logPath"
    }
}

function Get-D5BinaryManifest([string]$Name) {
    $binary = [string]$script:binaries[$Name]
    Assert-D5HarnessLeaseHeld $script:harnessLease
    Assert-D5RunProgramsUnchanged "before test enumeration of $Name"
    # Go listing executes no test and therefore cannot consume the lazy network
    # gate. Its separate source-bound operation intentionally carries no grant.
    $program = Get-D5TestProgramEvidence `
        -Programs @($script:initialProgramEvidence) `
        -Executable $binary
    $operation = New-D5TestEnumerationOperation `
        -Executable $binary `
        -WorkingDirectory ([string]$script:binaryWorkingDirectories[$Name]) `
        -ProgramEvidence $program `
        -SourceIdentity $script:run.SourceAtStart
    $enumerationRun = $script:run
    $enumerationName = $Name
    # Same closure-module constraint as Invoke-D5PlannedBinary: capture the
    # script-scoped function as a variable so the closure can invoke it.
    $addSourceCheckpoint = ${function:Add-D5SourceCheckpoint}
    $checkpoint = {
        param([string]$Boundary)
        & $addSourceCheckpoint $enumerationRun "$Boundary-enumeration-$enumerationName"
    }.GetNewClosure()
    $result = Invoke-D5TestEnumerationProcess `
        -Operation $operation `
        -ParentPrograms @($script:initialProgramEvidence) `
        -ExpectedSourceIdentity $script:run.SourceAtStart `
        -SourceCheckpoint $checkpoint
    $script:enumerationRecords.Add($result.OperationRecord)
    Assert-D5RunProgramsUnchanged "after test enumeration of $Name"
    if ($result.ExitCode -ne 0) {
        throw "Unable to enumerate tests in $binary"
    }
    $lines = @($result.StandardOutput -split '\r?\n')
    $tests = @($lines | Where-Object { $_ -match '^Test' } | Sort-Object -Unique)
    if ($tests.Count -eq 0) {
        throw "Stable binary $binary contains no tests"
    }
    return [ordered]@{
        Package = $Name
        Binary = $binary
        WorkingDirectory = [string]$script:binaryWorkingDirectories[$Name]
        Tests = $tests
    }
}

function Get-D5TestExecutionDefinitions {
    $definitions = [Collections.Generic.List[object]]::new()
    switch ($Mode) {
        'NetworkTests' {
            foreach ($name in $script:binaries.Keys | Sort-Object) {
                $timeout = if ($name -eq 'e2e') { '10m' } else { '5m' }
                $definitions.Add([pscustomobject][ordered]@{
                    RequestID = "network-$name"
                    Name = $name
                    Arguments = @('-test.v', '-test.count=1', "-test.timeout=$timeout")
                    LogName = "{phase}/$name.txt"
                })
            }
        }
        'Baseline' {
            $definitions.Add([pscustomobject][ordered]@{
                RequestID = 'baseline-pion'
                Name = 'webrtc'
                Arguments = @(
                    '-test.run=^$',
                    '-test.bench=^BenchmarkPionChunkTransfer$',
                    '-test.benchmem',
                    "-test.benchtime=$BenchTime",
                    "-test.count=$Count"
                )
                LogName = 'baseline/pion.txt'
            })
            $definitions.Add([pscustomobject][ordered]@{
                RequestID = 'baseline-relay-queue'
                Name = 'relay'
                Arguments = @(
                    '-test.run=^TestSharedForwardQueueChunkPolicy$',
                    '-test.count=1',
                    '-test.v'
                )
                LogName = 'baseline/relay-queue.txt'
            })
            $definitions.Add([pscustomobject][ordered]@{
                RequestID = 'baseline-relay'
                Name = 'relay'
                Arguments = @(
                    '-test.run=^$',
                    '-test.bench=^BenchmarkRelayChunkTransfer$',
                    '-test.benchmem',
                    "-test.benchtime=$BenchTime",
                    "-test.count=$Count"
                )
                LogName = 'baseline/relay.txt'
            })
            $definitions.Add([pscustomobject][ordered]@{
                RequestID = 'baseline-connectivity-zero-pressure'
                Name = 'connectivity'
                Arguments = @(
                    '-test.run=^$',
                    '-test.bench=^BenchmarkSenderRequestWindowZeroPressure$',
                    '-test.benchmem',
                    "-test.benchtime=$BenchTime",
                    "-test.count=$Count"
                )
                LogName = 'baseline/connectivity-zero-pressure.txt'
            })
        }
        'Profile' {
            $profileRoot = Join-Path $script:run.StagePath 'profiles'
            $definitions.Add([pscustomobject][ordered]@{
                RequestID = 'profile-pion'
                Name = 'webrtc'
                Arguments = @(
                    '-test.run=^$',
                    '-test.bench=^BenchmarkPionChunkTransfer/chunk_1024KiB$',
                    "-test.benchtime=$BenchTime",
                    "-test.cpuprofile=$(Join-Path $profileRoot 'pion.cpu')",
                    "-test.memprofile=$(Join-Path $profileRoot 'pion.mem')"
                )
                LogName = 'profiles/pion.txt'
            })
            $definitions.Add([pscustomobject][ordered]@{
                RequestID = 'profile-relay'
                Name = 'relay'
                Arguments = @(
                    '-test.run=^$',
                    '-test.bench=^BenchmarkRelayChunkTransfer/chunk_1024KiB$',
                    "-test.benchtime=$BenchTime",
                    "-test.cpuprofile=$(Join-Path $profileRoot 'relay.cpu')",
                    "-test.memprofile=$(Join-Path $profileRoot 'relay.mem')"
                )
                LogName = 'profiles/relay.txt'
            })
        }
    }
    return @($definitions)
}

function New-D5CompilerExecutionPlans(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]]$Definitions
) {
    if ($Definitions.Count -eq 0) {
        return
    }
    $requests = [Collections.Generic.List[object]]::new()
    $seenRequestIDs = @{}
    foreach ($definition in $Definitions) {
        $requestID = [string]$definition.RequestID
        $name = [string]$definition.Name
        if ($seenRequestIDs.ContainsKey($requestID)) {
            throw "Duplicate test execution request ID: $requestID"
        }
        $seenRequestIDs[$requestID] = $true
        $packagesForName = @($allPackages | Where-Object { [string]$_.Name -ceq $name })
        if ($packagesForName.Count -ne 1) {
            throw "Test execution request $requestID has no exact package manifest record"
        }
        $binary = [string]$script:binaries[$name]
        $program = Get-D5TestProgramEvidence `
            -Programs @($script:initialProgramEvidence) `
            -Executable $binary
        $requests.Add([ordered]@{
            RequestID = $requestID
            PackagePath = ([string]$packagesForName[0].Path).TrimStart('.', '/', '\')
            Executable = $program
            WorkingDirectory = [string]$script:binaryWorkingDirectories[$name]
            Arguments = @($definition.Arguments)
        })
    }

    $requestPath = Join-Path $script:run.StagePath 'test-execution-plan-request.json'
    $outputPath = Join-Path $script:run.StagePath 'test-execution-plans.json'
    $compilerLog = Join-Path $script:run.StagePath 'test-execution-plan-compiler.txt'
    $source = ConvertTo-D5BoundSourceIdentity $script:run.SourceAtStart
    Write-D5JSON $requestPath ([ordered]@{
        SchemaVersion = $script:D5TestExecutionPlanSchemaVersion
        RunID = $script:ownerID
        Source = $source
        Operations = @($requests)
    })

    [void](Add-D5SourceCheckpoint $script:run 'before-compiler-execution-plan')
    Push-Location $repositoryRoot
    try {
        & go run ./scripts/internal/d5networkpolicy `
            -root $repositoryRoot `
            -manifest scripts/d5-windows-network-packages.json `
            -execution-plan-request $requestPath `
            -execution-plan-output $outputPath 2>&1 |
            Tee-Object -FilePath $compilerLog
        $exitCode = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    [void](Add-D5SourceCheckpoint $script:run 'after-compiler-execution-plan')
    if ($exitCode -ne 0) {
        throw "Compiler execution-plan construction exited with code $exitCode"
    }
    if (-not (Test-Path -LiteralPath $outputPath -PathType Leaf)) {
        throw 'Compiler execution-plan output is missing'
    }
    $document = Get-Content -LiteralPath $outputPath -Raw | ConvertFrom-Json
    if ([int]$document.SchemaVersion -ne $script:D5TestExecutionPlanSchemaVersion -or
        [string]$document.RunID -cne $script:ownerID) {
        throw 'Compiler execution-plan document identity is invalid'
    }
    Assert-D5EnumerationSourceIdentity `
        $script:run.SourceAtStart `
        $document.Source `
        'compiler execution-plan document'
    $plans = @($document.Plans)
    if ($plans.Count -ne $Definitions.Count) {
        throw 'Compiler execution-plan count differs from the exact request set'
    }
    foreach ($definition in $Definitions) {
        $requestID = [string]$definition.RequestID
        $matches = @($plans | Where-Object { [string]$_.RequestID -ceq $requestID })
        if ($matches.Count -ne 1) {
            throw "Compiler execution-plan output does not contain exactly one $requestID plan"
        }
        $plan = $matches[0]
        $digest = ([string]$plan.PlanSHA256).ToLowerInvariant()
        [void](Assert-D5TestExecutionPlan `
            -Plan $plan `
            -ParentPrograms @($script:initialProgramEvidence) `
            -ExpectedSourceIdentity $script:run.SourceAtStart `
            -ExpectedRunID $script:ownerID `
            -ExpectedRequestID $requestID `
            -ExpectedPlanSHA256 $digest)
        $script:executionPlans[$requestID] = [pscustomobject]@{
            Plan = $plan
            PlanSHA256 = $digest
        }
    }
}

function Build-D5AtomicTestBinary([object]$Package) {
    Assert-D5HarnessLeaseHeld $script:harnessLease
    [void](Add-D5SourceCheckpoint $script:run "before-build-$($Package.Name)")
    $binary = Join-Path $harnessRoot "$($Package.Name).test.exe"
    $temporary = "$binary.building-$($script:ownerID)"
    $arguments = @('test', '-c', '-o', $temporary)
    if ($Mode -eq 'NetworkTests' -or $Race) {
        $arguments += '-race'
    }
    $arguments += [string]$Package.Path
    try {
        & go @arguments
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to build $($Package.Path)"
        }
        Assert-D5HarnessLeaseHeld $script:harnessLease
        [IO.File]::Move($temporary, $binary, $true)
    } finally {
        [IO.File]::Delete($temporary)
    }
    [void](Add-D5SourceCheckpoint $script:run "after-build-$($Package.Name)")
    $script:binaries[$Package.Name] = $binary
    $packageDirectory = ([string]$Package.Path).TrimStart('.', '/', '\')
    $script:binaryWorkingDirectories[$Package.Name] = [IO.Path]::GetFullPath(
        (Join-Path $repositoryRoot $packageDirectory)
    )
}

function Build-D5StableChildren([bool]$IncludeHostileSender, [string]$ManifestPath) {
    Assert-D5HarnessLeaseHeld $script:harnessLease
    New-Item -ItemType Directory -Force -Path $stableChildRoot | Out-Null
    $targets = [Collections.Generic.List[object]]::new()
    $targets.Add([pscustomobject]@{
        Path = Join-Path $stableChildRoot 'windshare.exe'
        Package = './cmd/windshare'
    })
    $targets.Add([pscustomobject]@{
        Path = Join-Path $stableChildRoot 'wsrelay.exe'
        Package = './relay/cmd/wsrelay'
    })
    if ($IncludeHostileSender) {
        $targets.Add([pscustomobject]@{
            Path = Join-Path $stableChildRoot 'hostile-sender.exe'
            Package = './web/e2e/fixtures/hostile-sender'
        })
    }
    $temporaryPaths = [Collections.Generic.List[string]]::new()
    try {
        foreach ($target in $targets) {
            [void](Add-D5SourceCheckpoint $script:run "before-build-$([IO.Path]::GetFileName($target.Path))")
            $temporary = "$($target.Path).building-$($script:ownerID)"
            $temporaryPaths.Add($temporary)
            & go build -race -o $temporary $target.Package
            if ($LASTEXITCODE -ne 0) {
                throw "Failed to build stable child $($target.Package)"
            }
            [void](Add-D5SourceCheckpoint $script:run "after-build-$([IO.Path]::GetFileName($target.Path))")
        }
        Assert-D5HarnessLeaseHeld $script:harnessLease
        foreach ($target in $targets) {
            $temporary = "$($target.Path).building-$($script:ownerID)"
            [IO.File]::Move($temporary, $target.Path, $true)
        }
        $evidence = @(Get-D5BinaryEvidence @($targets.Path))
        Write-D5JSON $ManifestPath ([ordered]@{
            recordedAt = [datetimeoffset]::UtcNow.ToString('o')
            binaries = @($evidence | ForEach-Object {
                [ordered]@{
                    path = [string]$_.Path
                    bytes = [long]$_.Bytes
                    sha256 = [string]$_.SHA256
                }
            })
        })
        return @($targets.Path | ForEach-Object { [IO.Path]::GetFullPath($_) })
    } finally {
        foreach ($temporary in $temporaryPaths) {
            [IO.File]::Delete($temporary)
        }
    }
}

function Invoke-D5SelectedMode([string]$Phase) {
    switch ($Mode) {
        'OrdinaryTests' {
            [void](Add-D5SourceCheckpoint $script:run 'before-ordinary-go-test')
            $arguments = @('test', '-count=1', '-timeout=600s')
            if ($Race) {
                $arguments += '-race'
            }
            $arguments += './...'
            Push-Location $repositoryRoot
            try {
                & go @arguments 2>&1 | Tee-Object -FilePath (Join-Path $script:run.StagePath 'windows-ordinary-tests.txt')
                $exitCode = $LASTEXITCODE
            } finally {
                Pop-Location
            }
            [void](Add-D5SourceCheckpoint $script:run 'after-ordinary-go-test')
            if ($exitCode -ne 0) {
                throw "Ordinary Windows tests exited with code $exitCode"
            }
        }
        'NetworkTests' {
            foreach ($definition in $script:operationDefinitions) {
                Invoke-D5PlannedBinary $definition $Phase
            }
        }
        'BrowserTests' {
            $phaseRoot = Join-Path $script:run.StagePath "browser/$Phase"
            $listLog = Join-Path $phaseRoot 'test-list.txt'
            $runLog = Join-Path $phaseRoot 'playwright.txt'
            $env:WINDSHARE_D5_PLAYWRIGHT_OUTPUT_DIR = Join-Path $phaseRoot 'test-results'
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $listLog) | Out-Null
            [void](Add-D5SourceCheckpoint $script:run 'before-browser-test-enumeration')
            Push-Location $repositoryRoot
            try {
                $listed = @(& pnpm -C web exec playwright test --list 2>&1 | Tee-Object -FilePath $listLog)
                $listExitCode = $LASTEXITCODE
                if ($listExitCode -eq 0) {
                    $tests = @($listed | Where-Object { $_ -match '\s›\s' })
                    if ($tests.Count -eq 0) {
                        throw 'Playwright test enumeration returned no scenarios'
                    }
                    Write-D5JSON (Join-Path $phaseRoot 'test-manifest.json') ([ordered]@{
                        Count = $tests.Count
                        Tests = $tests
                    })
                    [void](Add-D5SourceCheckpoint $script:run 'before-browser-test-run')
                    & pnpm -C web exec playwright test 2>&1 | Tee-Object -FilePath $runLog
                    $runExitCode = $LASTEXITCODE
                }
            } finally {
                Pop-Location
            }
            [void](Add-D5SourceCheckpoint $script:run 'after-browser-test-run')
            if ($listExitCode -ne 0) {
                throw "Playwright test enumeration exited with code $listExitCode"
            }
            if ($runExitCode -ne 0) {
                throw "Playwright exited with code $runExitCode"
            }
        }
        'Baseline' {
            foreach ($definition in $script:operationDefinitions) {
                Invoke-D5PlannedBinary $definition $Phase
            }
        }
        'Profile' {
            New-Item -ItemType Directory -Force -Path (
                Join-Path $script:run.StagePath 'profiles'
            ) | Out-Null
            foreach ($definition in $script:operationDefinitions) {
                Invoke-D5PlannedBinary $definition $Phase
            }
        }
    }
}

function Invoke-D5AuditedPhase(
    [string]$Phase,
    [bool]$AllowColdRegistration,
    [string[]]$ExpectedPrograms,
    [scriptblock]$Action
) {
    Assert-D5HarnessLeaseHeld $script:harnessLease
    $beforeRecordID = [long]$script:auditRecordID
    $beforeRules = @($script:auditRules)
    $runError = $null
    try {
        & $Action
    } catch {
        $runError = $_
    }
    $settled = $null
    $settleError = $null
    try {
        $settled = Wait-D5FirewallQuiescence $beforeRecordID
    } catch {
        $settleError = $_
    }
    if ($null -eq $settled) {
        if ($null -ne $runError) {
            throw "$runError; firewall quiescence failed: $settleError"
        }
        throw $settleError
    }
    $audit = [pscustomobject][ordered]@{
        Phase = $Phase
        AllowColdRegistration = $AllowColdRegistration
        ExpectedPrograms = @($ExpectedPrograms | ForEach-Object { [IO.Path]::GetFullPath($_) } | Sort-Object -Unique)
        BeforeRecordID = $beforeRecordID
        AfterRecordID = [long]$settled.AfterRecordID
        BeforeRules = $beforeRules
        AfterRules = @($settled.AfterRules)
        NewRelevantEvents = @($settled.NewRelevantEvents)
    }
    $script:phaseAudits.Add($audit)
    $script:auditRecordID = [long]$audit.AfterRecordID
    $script:auditRules = @($audit.AfterRules)
    Write-D5JSON (Join-Path $script:run.StagePath 'firewall-audit.json') ([ordered]@{
        Mode = $Mode
        InitialRecordID = $script:auditInitialRecordID
        InitialRules = @($script:auditInitialRules)
        Phases = @($script:phaseAudits)
    })
    $policyError = $null
    try {
        Assert-D5FirewallAuditChain @($script:phaseAudits) $script:auditInitialRecordID @($script:auditInitialRules)
        if ($AllowColdRegistration) {
            Assert-D5ColdFirewallRegistration `
                $audit `
                $ExpectedPrograms `
                $script:firewallOwnershipPolicy `
                $script:firewallOwnershipBaseline
        } else {
            Assert-D5FirewallUnchanged `
                $audit `
                $script:firewallOwnershipPolicy `
                $script:firewallOwnershipBaseline
        }
    } catch {
        $policyError = $_
    }
    if ($null -ne $runError -and $null -ne $policyError) {
        throw "$runError; firewall policy failed: $policyError"
    }
    if ($null -ne $runError) { throw $runError }
    if ($null -ne $policyError) { throw $policyError }
    $script:lastAudit = $audit
}

function Add-D5RunnerFailure([object]$ErrorRecord, [string]$Context = '') {
    if ($null -eq $ErrorRecord) {
        return
    }
    $message = if ([string]::IsNullOrWhiteSpace($Context)) {
        [string]$ErrorRecord
    } else {
        "${Context}: $ErrorRecord"
    }
    if ($null -eq $script:failure) {
        $script:failure = [Exception]::new($message)
    } else {
        $script:failure = [Exception]::new("$($script:failure); $message")
    }
}

$allPackages = @(
    Get-Content -LiteralPath (Join-Path $PSScriptRoot 'd5-windows-network-packages.json') -Raw |
        ConvertFrom-Json
)
$packages = switch ($Mode) {
    'NetworkTests' { $allPackages }
    'Build' { $allPackages }
    'Baseline' { @($allPackages | Where-Object { $_.Name -in @('webrtc', 'relay', 'connectivity') }) }
    'Profile' { @($allPackages | Where-Object { $_.Name -in @('webrtc', 'relay') }) }
    default { @() }
}
$script:firewallOwnershipEvidence = Import-D5FirewallOwnershipEvidence `
    $repositoryRoot `
    $firewallOwnershipEvidencePath
$script:authorizedStablePrograms = @(
    $script:firewallOwnershipEvidence.StableRelativePrograms | ForEach-Object {
        $relative = ([string]$_).Replace('/', [IO.Path]::DirectorySeparatorChar)
        [IO.Path]::GetFullPath((Join-Path $harnessRoot $relative))
    }
)

$command = "scripts/d5-windows-performance.ps1 -Mode $Mode -BenchTime $BenchTime -Count $Count" + $(if ($Race) { ' -Race' } else { '' })
$script:run = New-D5EvidenceRun $repositoryRoot $EvidenceRoot $Mode $command
$script:ownerID = "$PID-$([guid]::NewGuid().ToString('N'))"
$script:binaries = @{}
$script:binaryWorkingDirectories = @{}
$script:phaseAudits = [Collections.Generic.List[object]]::new()
$script:failure = $null
$script:harnessLease = $null
$script:runnerGuard = $null
$script:initialProgramEvidence = @()
$script:operationDefinitions = @()
$script:executionPlans = @{}
$script:executionRecords = [Collections.Generic.List[object]]::new()
$script:authorizationRecords = [Collections.Generic.List[object]]::new()
$script:enumerationRecords = [Collections.Generic.List[object]]::new()
$script:auditInitialized = $false
$script:lastAudit = $null
$script:registrationState = $null
$script:firewallOwnershipPolicy = $null
$script:firewallOwnershipBaseline = $null
$publishedResult = $null
$expectedPrograms = @()
$networkCapablePrograms = @()
$builtPrograms = @()

$environmentNames = @(
    $browserNetworkContractName,
    $browserLeaseTokenName,
    $launchAuthorizationPipeName,
    $runnerGuardName,
    'WINDSHARE_D5_PLAYWRIGHT_OUTPUT_DIR',
    'WINDSHARE_D5_CHILD_MANIFEST'
)
$savedEnvironment = @{}
foreach ($name in $environmentNames) {
    $savedEnvironment[$name] = [pscustomobject]@{
        Defined = Test-Path "Env:$name"
        Value = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    [Environment]::SetEnvironmentVariable($name, $null, 'Process')
}

try {
    New-Item -ItemType Directory -Force -Path $harnessRoot, $stableChildRoot | Out-Null
    $script:harnessLease = Acquire-D5HarnessLease $stableChildRoot $browserNetworkContractValue
    Wait-D5HarnessNamespaceQuiescent $script:harnessLease $harnessRoot
    $script:runnerGuard = New-D5RunnerGuard
    [Environment]::SetEnvironmentVariable($runnerGuardName, $script:runnerGuard.Name, 'Process')
    $script:firewallOwnershipPolicy = New-D5CurrentFirewallOwnershipPolicy

    # The cursor precedes the initial snapshot so a rule change during enumeration
    # is attributed to the first audited interval instead of entering a blind baseline.
    $script:auditInitialRecordID = Get-D5LatestFirewallRecordID
    $script:auditInitialRules = @(Get-D5FirewallRules)
    $script:auditRecordID = [long]$script:auditInitialRecordID
    $script:auditRules = @($script:auditInitialRules)
    $script:auditInitialized = $true
    Write-D5JSON (Join-Path $script:run.StagePath 'preflight-rules.json') $script:auditInitialRules
    $script:firewallOwnershipBaseline = New-D5FirewallOwnershipBaseline `
        $script:auditInitialRules `
        $script:firewallOwnershipPolicy
    Write-D5JSON `
        (Join-Path $script:run.StagePath 'firewall-ownership-before.json') `
        ([ordered]@{
            EvidenceSources = @($script:firewallOwnershipPolicy.EvidenceSources)
            StablePrograms = @($script:authorizedStablePrograms)
            D5ExecutableNames = @($script:firewallOwnershipPolicy.D5ProgramNames)
            D5ExecutableSHA256 = @($script:firewallOwnershipPolicy.D5ProgramSHA256)
            CleanupOwned = [ordered]@{
                RuleCount = $script:firewallOwnershipPolicy.CleanupOwnedRuleCount
                ProgramCount = $script:firewallOwnershipPolicy.CleanupOwnedProgramCount
                ProgramRootCount = @($script:firewallOwnershipPolicy.CleanupOwnedProgramRoots).Count
                SemanticPayloadSHA256 = $script:firewallOwnershipPolicy.CleanupOwnedSemanticPayloadSHA256
            }
            Baseline = $script:firewallOwnershipBaseline
        })
    Assert-D5FirewallPreflight `
        $script:auditInitialRules `
        $script:firewallOwnershipPolicy `
        $script:firewallOwnershipBaseline
    # The no-action phase closes the cursor-before-snapshot interval before any build.
    # A concurrent temp identity cannot be absorbed into the excluded baseline silently.
    Invoke-D5AuditedPhase 'preflight-quiescence' $false @() { }
    $script:registrationState = Open-D5FirewallRegistrationState @($script:auditRules)
    Write-D5JSON `
        (Join-Path $script:run.StagePath 'firewall-registration-before.json') `
        $script:registrationState
    [void](Add-D5SourceCheckpoint $script:run 'after-lease-before-builds')

    Push-Location $repositoryRoot
    try {
        foreach ($package in $packages) {
            Build-D5AtomicTestBinary $package
        }
        if ($Mode -eq 'NetworkTests') {
            $builtPrograms += @(Build-D5StableChildren $false (Join-Path $script:run.StagePath 'e2e-child-manifest.json'))
        } elseif ($Mode -eq 'BrowserTests') {
            $env:WINDSHARE_D5_PLAYWRIGHT_OUTPUT_DIR = Join-Path $script:run.StagePath 'browser/test-results'
            $env:WINDSHARE_D5_CHILD_MANIFEST = Join-Path $script:run.StagePath 'browser/launched-binaries.json'
            $builtPrograms += @(Build-D5StableChildren $true $env:WINDSHARE_D5_CHILD_MANIFEST)
            [Environment]::SetEnvironmentVariable(
                $browserNetworkContractName,
                $browserNetworkContractValue,
                'Process'
            )
            [Environment]::SetEnvironmentVariable(
                $browserLeaseTokenName,
                $script:harnessLease.Token,
                'Process'
            )
        }
    } finally {
        Pop-Location
    }
    $builtPrograms += @($script:binaries.Values)
    $builtPrograms = @($builtPrograms | ForEach-Object { [IO.Path]::GetFullPath([string]$_) } | Sort-Object -Unique)
    $script:firewallOwnershipPolicy = New-D5CurrentFirewallOwnershipPolicy

    $expectedPrograms = switch ($Mode) {
        'NetworkTests' { $builtPrograms }
        'BrowserTests' { $builtPrograms }
        'Baseline' { @($script:binaries.Values) }
        'Profile' { @($script:binaries.Values) }
        default { @() }
    }
    $expectedPrograms = @(
        $expectedPrograms |
            ForEach-Object { [IO.Path]::GetFullPath([string]$_) } |
            Sort-Object -Unique
    )
    $script:initialProgramEvidence = @(Get-D5BinaryEvidence $builtPrograms)
    $script:operationDefinitions = @(Get-D5TestExecutionDefinitions)
    New-D5CompilerExecutionPlans @($script:operationDefinitions)

    $networkCapablePrograms = switch ($Mode) {
        'NetworkTests' { $builtPrograms }
        'BrowserTests' { $builtPrograms }
        'Baseline' {
            @($script:executionPlans.Values | Where-Object {
                [string]$_.Plan.NetworkAccess -eq 'parent-owned-one-use-pipe'
            } | ForEach-Object { [string]$_.Plan.Executable.Path })
        }
        'Profile' {
            @($script:executionPlans.Values | Where-Object {
                [string]$_.Plan.NetworkAccess -eq 'parent-owned-one-use-pipe'
            } | ForEach-Object { [string]$_.Plan.Executable.Path })
        }
        default { @() }
    }
    $networkCapablePrograms = @(
        $networkCapablePrograms |
            ForEach-Object { [IO.Path]::GetFullPath([string]$_) } |
            Sort-Object -Unique
    )
    $pendingRegistrationPrograms = @(
        Get-D5PendingRegistrationPrograms $script:registrationState $networkCapablePrograms
    )
    if ($Mode -in @('Baseline', 'Profile') -and $pendingRegistrationPrograms.Count -ne 0) {
        throw "$Mode requires prior bounded registration disposition for: $($pendingRegistrationPrograms -join ', ')"
    }
    Write-D5JSON (Join-Path $script:run.StagePath 'binary-manifest.json') ([ordered]@{
        Mode = $Mode
        ExpectedLaunchedPrograms = $expectedPrograms
        NetworkCapablePrograms = $networkCapablePrograms
        PendingFirstRegistrationPrograms = $pendingRegistrationPrograms
        Binaries = @($script:initialProgramEvidence)
    })

    if ($Mode -in @('NetworkTests', 'Baseline', 'Profile')) {
        $manifests = @(
            foreach ($name in $script:binaries.Keys | Sort-Object) {
                Get-D5BinaryManifest $name
            }
        )
        Write-D5JSON (Join-Path $script:run.StagePath 'test-manifest.json') $manifests
    }

    switch ($Mode) {
        'NetworkTests' {
            if ($pendingRegistrationPrograms.Count -gt 0) {
                Invoke-D5AuditedPhase 'cold' $true $pendingRegistrationPrograms { Invoke-D5SelectedMode 'cold' }
                $script:registrationState = Complete-D5FirewallRegistrationAttempt `
                    $script:registrationState `
                    @($script:lastAudit.AfterRules) `
                    $pendingRegistrationPrograms `
                    $script:firewallOwnershipPolicy `
                    $script:firewallOwnershipBaseline
                Save-D5FirewallRegistrationState $script:registrationState
                Invoke-D5AuditedPhase 'repeat' $false $networkCapablePrograms { Invoke-D5SelectedMode 'repeat' }
            } else {
                Invoke-D5AuditedPhase 'network' $false $networkCapablePrograms { Invoke-D5SelectedMode 'network' }
            }
        }
        'BrowserTests' {
            if ($pendingRegistrationPrograms.Count -gt 0) {
                Invoke-D5AuditedPhase 'browser-cold' $true $pendingRegistrationPrograms { Invoke-D5SelectedMode 'browser-cold' }
                $script:registrationState = Complete-D5FirewallRegistrationAttempt `
                    $script:registrationState `
                    @($script:lastAudit.AfterRules) `
                    $pendingRegistrationPrograms `
                    $script:firewallOwnershipPolicy `
                    $script:firewallOwnershipBaseline
                Save-D5FirewallRegistrationState $script:registrationState
                Invoke-D5AuditedPhase 'browser-repeat' $false $expectedPrograms { Invoke-D5SelectedMode 'browser-repeat' }
            } else {
                Invoke-D5AuditedPhase 'browser' $false $expectedPrograms { Invoke-D5SelectedMode 'browser' }
            }
            $launched = Get-Content -LiteralPath $env:WINDSHARE_D5_CHILD_MANIFEST -Raw | ConvertFrom-Json
            $manifestPrograms = @($launched.binaries.Path | ForEach-Object { [IO.Path]::GetFullPath([string]$_) } | Sort-Object)
            if ($manifestPrograms.Count -ne $expectedPrograms.Count -or
                @(Compare-Object -ReferenceObject @($expectedPrograms | Sort-Object) -DifferenceObject $manifestPrograms).Count -ne 0) {
                throw 'Browser runner manifest does not contain its exact per-mode program set'
            }
        }
        'Baseline' {
            Invoke-D5AuditedPhase 'measurement' $false $networkCapablePrograms { Invoke-D5SelectedMode 'measurement' }
        }
        'Profile' {
            Invoke-D5AuditedPhase 'measurement' $false $networkCapablePrograms { Invoke-D5SelectedMode 'measurement' }
        }
        'OrdinaryTests' {
            Invoke-D5AuditedPhase 'ordinary' $false @() { Invoke-D5SelectedMode 'ordinary' }
        }
        'Build' {
            Invoke-D5AuditedPhase 'build' $false @() { }
        }
        'Audit' {
            Invoke-D5AuditedPhase 'audit' $false @() { }
        }
    }
} catch {
    Add-D5RunnerFailure $_
}

if ($null -ne $script:harnessLease) {
    try {
        Wait-D5HarnessNamespaceQuiescent $script:harnessLease $harnessRoot
        Assert-D5RunProgramsUnchanged 'post-run'
        if (@($script:initialProgramEvidence).Count -gt 0) {
            Write-D5JSON (Join-Path $script:run.StagePath 'post-run-binaries.json') ([ordered]@{
                Binaries = @(Get-D5BinaryEvidence @($script:initialProgramEvidence.Path))
            })
        }
        if ($script:authorizationRecords.Count -gt 0) {
            Write-D5JSON (Join-Path $script:run.StagePath 'parent-authorizations.json') ([ordered]@{
                RunID = $script:ownerID
                Launches = @($script:authorizationRecords)
            })
        }
        if ($script:executionRecords.Count -gt 0) {
            Write-D5JSON (Join-Path $script:run.StagePath 'test-executions.json') ([ordered]@{
                RunID = $script:ownerID
                Executions = @($script:executionRecords)
            })
        }
        if ($script:enumerationRecords.Count -gt 0) {
            Write-D5JSON (Join-Path $script:run.StagePath 'non-network-enumerations.json') ([ordered]@{
                RunID = $script:ownerID
                Authorization = 'none'
                Enumerations = @($script:enumerationRecords)
            })
        }
        [void](Add-D5SourceCheckpoint $script:run 'after-execution-before-final-audit')
        if ($script:auditInitialized) {
            Invoke-D5AuditedPhase 'final-quiescence' $false $networkCapablePrograms { }
        }
        if ($null -ne $script:firewallOwnershipBaseline) {
            Assert-D5FirewallOwnershipBaseline `
                $script:firewallOwnershipBaseline `
                @($script:auditRules) `
                $script:firewallOwnershipPolicy `
                'Final ownership verification'
            $finalOwnership = New-D5FirewallOwnershipBaseline `
                @($script:auditRules) `
                $script:firewallOwnershipPolicy
            Write-D5JSON `
                (Join-Path $script:run.StagePath 'firewall-ownership-after.json') `
                ([ordered]@{
                    D5ExecutableSHA256 = @($script:firewallOwnershipPolicy.D5ProgramSHA256)
                    Baseline = $finalOwnership
                })
        }
        if ($null -ne $script:registrationState) {
            Assert-D5FirewallRegistrationState `
                $script:registrationState `
                @($script:auditRules) `
                $script:firewallOwnershipPolicy `
                $script:firewallOwnershipBaseline
            Write-D5JSON `
                (Join-Path $script:run.StagePath 'firewall-registration-after.json') `
                $script:registrationState
        }
        $connected = @($script:runnerGuard.Connections | Where-Object IsCompletedSuccessfully).Count
        Write-D5JSON (Join-Path $script:run.StagePath 'runner-guard.json') ([ordered]@{
            ConnectedProcesses = $connected
            Capacity = $script:D5RunnerGuardCapacity
        })
        if ($Mode -eq 'BrowserTests') {
            Assert-D5RunnerGuardConnected $script:runnerGuard
        }
    } catch {
        Add-D5RunnerFailure $_ 'post-run ownership verification failed'
    }
}

try {
    $status = if ($null -eq $script:failure) { 'Success' } else { 'Failed' }
    $errorMessage = if ($null -eq $script:failure) { '' } else { [string]$script:failure }
    $publishedResult = Complete-D5EvidenceRun $script:run $status $errorMessage
    if ($publishedResult.Status -ne 'Success' -and $null -eq $script:failure) {
        Add-D5RunnerFailure $publishedResult.Error 'evidence completion failed'
    }
} catch {
    Add-D5RunnerFailure $_ 'evidence publication failed'
}

foreach ($name in $environmentNames) {
    $saved = $savedEnvironment[$name]
    if ($saved.Defined) {
        [Environment]::SetEnvironmentVariable($name, [string]$saved.Value, 'Process')
    } else {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
}
try {
    Release-D5RunnerGuard $script:runnerGuard
} catch {
    Add-D5RunnerFailure $_ 'runner guard cleanup failed'
}
try {
    Release-D5HarnessLease $script:harnessLease
} catch {
    Add-D5RunnerFailure $_ 'global harness lease cleanup failed'
}

if ($null -ne $publishedResult) {
    Write-Output "D5 evidence published without overwrite: $($publishedResult.Path)"
}
if ($null -ne $script:failure) {
    throw $script:failure
}
