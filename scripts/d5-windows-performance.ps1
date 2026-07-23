[CmdletBinding()]
param(
    [ValidateSet('Audit', 'Build', 'OrdinaryTests', 'NetworkTests', 'BrowserTests', 'Baseline', 'Profile')]
    [string]$Mode = 'OrdinaryTests',

    [ValidatePattern('^[1-9][0-9]*(ns|us|ms|s|m|h)$')]
    [string]$BenchTime = '2s',

    [ValidateRange(1, 100)]
    [int]$Count = 5,

    [switch]$Race,

    [string]$CoverProfileRoot,

    [string]$EvidenceRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    throw 'The D5 stable Windows network runner is Windows-only.'
}
if ($Mode -eq 'Baseline' -and $Count -ne 5) {
    throw 'D5 Baseline requires exactly five independent samples; -Count must be 5.'
}
if (-not [string]::IsNullOrWhiteSpace($CoverProfileRoot)) {
    # Coverage measurement is meaningful only for the full fixed-path suite;
    # partial modes would understate every classified package.
    if ($Mode -ne 'NetworkTests') {
        throw 'Coverage profiles require the full fixed-path suite: use -Mode NetworkTests'
    }
    $CoverProfileRoot = [IO.Path]::GetFullPath($CoverProfileRoot)
    New-Item -ItemType Directory -Force -Path $CoverProfileRoot | Out-Null
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
# e2e carries the largest per-binary Go-level deadline (10m -test.timeout);
# the parallel join adds 2m grace so this watchdog only fires on a child that
# outlived its own deadline enforcement.
$script:D5NetworkJoinTimeout = [timespan]::FromMinutes(12)
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
. (Join-Path $PSScriptRoot 'go-benchmark-evidence.ps1')
. (Join-Path $PSScriptRoot 'd5-pion-performance-evidence.ps1')

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

function Get-D5RetiredProgramRuntimeStates {
    $states = [Collections.Generic.List[object]]::new()
    foreach ($program in @($script:firewallOwnershipPolicy.RetiredProgramSet)) {
        $expectedNative = ConvertTo-D5NativeImagePath $program
        $observations = [Collections.Generic.List[object]]::new()
        $processName = [IO.Path]::GetFileNameWithoutExtension($program)
        foreach ($process in @(Get-Process -Name $processName -ErrorAction SilentlyContinue)) {
            try {
                $observations.Add([pscustomobject]@{
                    ProcessID = [int]$process.Id
                    ImagePath = Get-D5ProcessNativeImagePath $process
                })
            } catch {
                $observations.Add([pscustomobject]@{
                    ProcessID = [int]$process.Id
                    ImagePath = ''
                })
            } finally {
                $process.Dispose()
            }
        }
        $processIDs = @(Select-D5ExactProcessIDs $expectedNative @($observations))
        $external = @($observations | Where-Object {
            [string]$_.ImagePath -ine $expectedNative
        })
        if ($external.Count -gt 0) {
            Write-Warning (
                "Ignoring same-name processes outside the retired D5 image identity; " +
                "resolved PIDs=$(@($external.ProcessID) -join ',')"
            )
        }
        $exists = Test-Path -LiteralPath $program -PathType Leaf
        $states.Add([pscustomobject][ordered]@{
            Program = [IO.Path]::GetFullPath($program)
            Exists = $exists
            SHA256 = if ($exists) {
                (Get-FileHash -LiteralPath $program -Algorithm SHA256).Hash.ToLowerInvariant()
            } else {
                ''
            }
            ProcessIDs = $processIDs
        })
    }
    return @($states)
}

function Test-D5RelevantFirewallFilter([string]$Program, [string]$InstanceID) {
    if (Test-D5RelevantProgram $Program) {
        return $true
    }
    return $null -ne $script:firewallOwnershipPolicy -and
        ($script:firewallOwnershipPolicy.RetiredRuleIdentitySet.Contains($InstanceID) -or
            (Test-D5ExternalStableProgramPath $Program $script:firewallOwnershipPolicy))
}

function Get-D5FirewallRules {
    $retiredStates = @(Get-D5RetiredProgramRuntimeStates)
    Assert-D5RetiredProgramRuntimeStates `
        $retiredStates `
        $script:firewallOwnershipPolicy `
        'Firewall observation'
    $retiredStatesByProgram = @{}
    foreach ($state in $retiredStates) {
        $retiredStatesByProgram[[IO.Path]::GetFullPath([string]$state.Program)] = $state
    }
    $snapshots = [Collections.Generic.List[object]]::new()
    $programStates = @{}
    foreach ($filter in @(Get-NetFirewallApplicationFilter -PolicyStore ActiveStore)) {
        $program = [Environment]::ExpandEnvironmentVariables([string]$filter.Program)
        if (-not (Test-D5RelevantFirewallFilter $program ([string]$filter.InstanceID))) {
            continue
        }
        $program = [IO.Path]::GetFullPath($program)
        if (-not $programStates.ContainsKey($program)) {
            $programStates[$program] = if ($retiredStatesByProgram.ContainsKey($program)) {
                $retiredStatesByProgram[$program]
            } elseif (Test-D5ProgramUnderRoot $program $script:firewallOwnershipPolicy.HarnessRoot) {
                $exists = Test-Path -LiteralPath $program -PathType Leaf
                [pscustomobject]@{
                    Exists = $exists
                    SHA256 = if ($exists) {
                        (Get-FileHash -LiteralPath $program -Algorithm SHA256).Hash.ToLowerInvariant()
                    } else {
                        ''
                    }
                    ProcessIDs = @()
                }
            } else {
                # External executable state cannot confer ownership, so hashing a
                # host-owned path would add a needless permission/freshness blocker.
                [pscustomobject]@{
                    Exists = $false
                    SHA256 = ''
                    ProcessIDs = @()
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
                DisplayName = [string]$rule.DisplayName
                Direction = ConvertTo-D5SemanticValue $rule.Direction
                Action = ConvertTo-D5SemanticValue $rule.Action
                Profile = @($rule.Profile | ForEach-Object { [string]$_ })
                Enabled = ConvertTo-D5SemanticValue $rule.Enabled
                PolicyStoreSourceType = ConvertTo-D5SemanticValue $rule.PolicyStoreSourceType
                Protocol = ConvertTo-D5SemanticValue @($ports.Protocol)
                LocalPort = ConvertTo-D5SemanticValue @($ports.LocalPort)
                RemotePort = ConvertTo-D5SemanticValue @($ports.RemotePort)
                LocalAddress = ConvertTo-D5SemanticValue @($addresses.LocalAddress)
                RemoteAddress = ConvertTo-D5SemanticValue @($addresses.RemoteAddress)
                ProgramExists = [bool]$programState.Exists
                ProgramSHA256 = [string]$programState.SHA256
                ProgramProcessIDs = @($programState.ProcessIDs)
            })
        }
    }
    return @($snapshots | Sort-Object Program, RuleID, InstanceID)
}

function Get-D5ActiveStoreFirewallRules {
    $snapshots = [Collections.Generic.List[object]]::new()
    foreach ($filter in @(Get-NetFirewallApplicationFilter -PolicyStore ActiveStore)) {
        $program = [Environment]::ExpandEnvironmentVariables([string]$filter.Program)
        foreach ($rule in @($filter | Get-NetFirewallRule -PolicyStore ActiveStore)) {
            $ports = @($rule | Get-NetFirewallPortFilter)
            $addresses = @($rule | Get-NetFirewallAddressFilter)
            $snapshots.Add([pscustomobject][ordered]@{
                RuleID = [string]$rule.Name
                InstanceID = [string]$filter.InstanceID
                Program = $program
                Direction = ConvertTo-D5SemanticValue $rule.Direction
                Action = ConvertTo-D5SemanticValue $rule.Action
                Profile = @($rule.Profile | ForEach-Object { [string]$_ })
                Enabled = ConvertTo-D5SemanticValue $rule.Enabled
                PolicyStoreSourceType = ConvertTo-D5SemanticValue $rule.PolicyStoreSourceType
                Protocol = ConvertTo-D5SemanticValue @($ports.Protocol)
                LocalPort = ConvertTo-D5SemanticValue @($ports.LocalPort)
                RemotePort = ConvertTo-D5SemanticValue @($ports.RemotePort)
                LocalAddress = ConvertTo-D5SemanticValue @($addresses.LocalAddress)
                RemoteAddress = ConvertTo-D5SemanticValue @($addresses.RemoteAddress)
            })
        }
    }
    return @($snapshots | Sort-Object Program, RuleID, InstanceID)
}

function Get-D5PlaywrightBrowserManifest {
    $output = @(
        & pnpm -C (Join-Path $repositoryRoot 'web') exec node scripts/verify-playwright-browsers.mjs --network-manifest-json 2>&1
    )
    if ($LASTEXITCODE -ne 0) {
        throw "Playwright firewall manifest probe failed: $($output -join [Environment]::NewLine)"
    }
    try {
        return @(($output -join [Environment]::NewLine) | ConvertFrom-Json)
    } catch {
        throw "Playwright firewall manifest is not valid JSON: $_"
    }
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
    $previousActiveStoreRules = @(Get-D5ActiveStoreFirewallRules)
    $previousRecordID = Get-D5LatestFirewallRecordID
    do {
        Start-Sleep -Milliseconds $firewallQuietMilliseconds
        $currentRules = @(Get-D5FirewallRules)
        $currentActiveStoreRules = @(Get-D5ActiveStoreFirewallRules)
        $currentRecordID = Get-D5LatestFirewallRecordID
        if ($currentRecordID -eq $previousRecordID -and
            (Test-D5RuleSetsEqual $previousRules $currentRules) -and
            (Test-D5RuleSetsEqual $previousActiveStoreRules $currentActiveStoreRules)) {
            return [pscustomobject]@{
                AfterRecordID = $currentRecordID
                AfterRules = $currentRules
                AfterActiveStoreRules = $currentActiveStoreRules
                NewRelevantEvents = @(
                    Get-D5RelevantFirewallEvents $AfterRecordID $currentRecordID
                )
            }
        }
        $previousRules = $currentRules
        $previousActiveStoreRules = $currentActiveStoreRules
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
        $state = $null
        try {
            $state = Get-Content -LiteralPath $registrationStatePath -Raw | ConvertFrom-Json
        } catch {
            # Registration state is a derived cache, never firewall authority.
            # Reconstructing an unreadable cache cannot broaden the observed
            # exact rule set that the ownership preflight already validated.
        }
        $schema = if ($null -eq $state -or
            $null -eq $state.PSObject.Properties['SchemaVersion']) {
            -1
        } else {
            [int]$state.SchemaVersion
        }
        if ($schema -eq $script:D5FirewallRegistrationSchemaVersion) {
            Assert-D5FirewallRegistrationState `
                $state `
                $Rules `
                $script:firewallOwnershipPolicy `
                $script:firewallOwnershipBaseline
            return $state
        }
    }
    $state = New-D5FirewallRegistrationState `
        $Rules `
        $script:firewallOwnershipPolicy `
        $script:firewallOwnershipBaseline
    Save-D5FirewallRegistrationState $state
    return $state
}

function Write-D5ForeignFirewallRuleWarning([object[]]$Rules) {
    # A filename collision outside the canonical namespace conveys no ownership;
    # report it for diagnosis without letting it weaken the exact stable-root
    # manifest. Nothing may still squat an unmanifested path inside that root.
    $foreign = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $retired = [Collections.Generic.List[object]]::new()
    foreach ($rule in @($Rules)) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        if ($script:firewallOwnershipPolicy.AuthorizedProgramSet.Contains($program)) {
            continue
        }
        if ($script:firewallOwnershipPolicy.RetiredProgramSet.Contains($program)) {
            Assert-D5RetiredProgramRule $rule $script:firewallOwnershipPolicy 'NetworkTests preflight'
            $retired.Add($rule)
            continue
        }
        if (Test-D5ProgramUnderRoot $program $script:firewallOwnershipPolicy.HarnessRoot) {
            throw "NetworkTests preflight found an unmanifested program under the stable harness root: $program"
        }
        if (Test-D5ExternalStableProgramNameRule $rule $script:firewallOwnershipPolicy) {
            [void]$foreign.Add($program)
        }
    }
    Assert-D5RetiredProgramRuleSet `
        @($retired) `
        $script:firewallOwnershipPolicy `
        'NetworkTests preflight'
    if ($retired.Count -gt 0) {
        Write-Warning (
            'The retired connectivity firewall tombstone remains as the exact inert Block TCP/UDP pair. ' +
            'It grants no launch authority and cleanup is optional host administration.'
        )
    }
    if ($foreign.Count -gt 0) {
        Write-Warning ('Ignoring same-name firewall rules outside the canonical D5 harness root: ' +
            (@($foreign | Sort-Object) -join ', '))
    }
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
    Assert-D5ProgramsExcludeRetiredTombstone `
        @([string]$binding.Plan.Executable.Path) `
        $script:firewallOwnershipPolicy `
        "Launch plan $requestID"
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
                $arguments = @('-test.v', '-test.count=1', "-test.timeout=$timeout")
                if (-not [string]::IsNullOrWhiteSpace($CoverProfileRoot)) {
                    # One batch execution with -test.count=1: each package's
                    # statements count from exactly one execution.
                    $arguments += "-test.coverprofile=$(
                        Join-Path $CoverProfileRoot "$name.cover.out"
                    )"
                }
                $definitions.Add([pscustomobject][ordered]@{
                    RequestID = "network-$name"
                    Name = $name
                    Arguments = $arguments
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
        }
    }
    return @($definitions)
}

function New-D5CompilerExecutionPlans(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]]$Definitions,
    [Parameter(Mandatory)] [string]$PlanRoot,
    [Parameter(Mandatory)] [object]$Source
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
        Assert-D5ProgramsExcludeRetiredTombstone `
            @($binary) `
            $script:firewallOwnershipPolicy `
            "Compiler execution-plan request $requestID"
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

    $requestPath = Join-Path $PlanRoot 'test-execution-plan-request.json'
    $outputPath = Join-Path $PlanRoot 'test-execution-plans.json'
    $compilerLog = Join-Path $PlanRoot 'test-execution-plan-compiler.txt'
    $boundSource = ConvertTo-D5BoundSourceIdentity $Source
    Write-D5JSON $requestPath ([ordered]@{
        SchemaVersion = $script:D5TestExecutionPlanSchemaVersion
        RunID = $script:ownerID
        Source = $boundSource
        Operations = @($requests)
    })

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
        $Source `
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
        Assert-D5ProgramsExcludeRetiredTombstone `
            @([string]$plan.Executable.Path) `
            $script:firewallOwnershipPolicy `
            "Compiler execution-plan output $requestID"
        $digest = ([string]$plan.PlanSHA256).ToLowerInvariant()
        [void](Assert-D5TestExecutionPlan `
            -Plan $plan `
            -ParentPrograms @($script:initialProgramEvidence) `
            -ExpectedSourceIdentity $Source `
            -ExpectedRunID $script:ownerID `
            -ExpectedRequestID $requestID `
            -ExpectedPlanSHA256 $digest)
        $script:executionPlans[$requestID] = [pscustomobject]@{
            Plan = $plan
            PlanSHA256 = $digest
        }
    }
}

function Build-D5AtomicTestBinary(
    [object]$Package,
    [Parameter(Mandatory)] [scriptblock]$SourceCheckpoint
) {
    Assert-D5HarnessLeaseHeld $script:harnessLease
    [void](& $SourceCheckpoint "before-build-$($Package.Name)")
    $binary = Join-Path $harnessRoot "$($Package.Name).test.exe"
    Assert-D5ProgramsExcludeRetiredTombstone `
        @($binary) `
        $script:firewallOwnershipPolicy `
        "Stable package build $($Package.Name)"
    $temporary = "$binary.building-$($script:ownerID)"
    $arguments = @('test', '-c', '-o', $temporary)
    if (-not [string]::IsNullOrWhiteSpace($CoverProfileRoot)) {
        # Coverage runs mirror the CI sweep (`-covermode=atomic`, no -race) so
        # the local verdict measures exactly what the ubuntu gate measures.
        $arguments += '-covermode=atomic'
    } elseif ($Mode -eq 'NetworkTests' -or $Race) {
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
    [void](& $SourceCheckpoint "after-build-$($Package.Name)")
    $script:binaries[$Package.Name] = $binary
    $packageDirectory = ([string]$Package.Path).TrimStart('.', '/', '\')
    $script:binaryWorkingDirectories[$Package.Name] = [IO.Path]::GetFullPath(
        (Join-Path $repositoryRoot $packageDirectory)
    )
}

function Build-D5StableChildren(
    [string]$ManifestPath,
    [Parameter(Mandatory)] [scriptblock]$SourceCheckpoint
) {
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
    Assert-D5ProgramsExcludeRetiredTombstone `
        @($targets.Path) `
        $script:firewallOwnershipPolicy `
        'Stable child build plan'
    $temporaryPaths = [Collections.Generic.List[string]]::new()
    try {
        foreach ($target in $targets) {
            [void](& $SourceCheckpoint "before-build-$([IO.Path]::GetFileName($target.Path))")
            $temporary = "$($target.Path).building-$($script:ownerID)"
            $temporaryPaths.Add($temporary)
            & go build -race -o $temporary $target.Package
            if ($LASTEXITCODE -ne 0) {
                throw "Failed to build stable child $($target.Package)"
            }
            [void](& $SourceCheckpoint "after-build-$([IO.Path]::GetFileName($target.Path))")
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
            $logPath = Join-Path $script:run.StagePath 'baseline/pion.txt'
            $baseline = Get-D5PionBaselineEvidence $logPath
            Write-D5JSON (Join-Path $script:run.StagePath 'baseline/pion-summary.json') $baseline
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
    $beforeActiveStoreRules = @($script:activeStoreRules)
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
    $activeStoreDelta = New-D5FirewallSemanticTransition `
        $beforeActiveStoreRules `
        @($settled.AfterActiveStoreRules) `
        $script:playwrightFirewallDebtPolicy `
        "$Phase ActiveStore evidence"
    $audit = [pscustomobject][ordered]@{
        Phase = $Phase
        AllowColdRegistration = $AllowColdRegistration
        ExpectedPrograms = @($ExpectedPrograms | ForEach-Object { [IO.Path]::GetFullPath($_) } | Sort-Object -Unique)
        BeforeRecordID = $beforeRecordID
        AfterRecordID = [long]$settled.AfterRecordID
        BeforeRules = $beforeRules
        AfterRules = @($settled.AfterRules)
        BeforeActiveStore = $activeStoreDelta.Before
        AfterActiveStore = $activeStoreDelta.After
        ActiveStoreDelta = $activeStoreDelta
        NewRelevantEvents = @($settled.NewRelevantEvents)
    }
    $script:phaseAudits.Add($audit)
    $script:auditRecordID = [long]$audit.AfterRecordID
    $script:auditRules = @($audit.AfterRules)
    $script:activeStoreRules = @($settled.AfterActiveStoreRules)
    Write-D5JSON (Join-Path $script:run.StagePath 'firewall-audit.json') ([ordered]@{
        Mode = $Mode
        InitialRecordID = $script:auditInitialRecordID
        InitialRules = @($script:auditInitialRules)
        InitialActiveStore = New-D5ActiveStoreFirewallSummary @($script:activeStoreInitialRules)
        InitialPlaywrightDebt = $script:playwrightFirewallDebtAtStart
        InitialExternalDebt = $script:externalFirewallDebtAtStart
        Phases = @($script:phaseAudits)
    })
    $policyError = $null
    try {
        $activeStoreTransition = Assert-D5ActiveStoreFirewallTransition `
            @($script:activeStoreInitialRules) `
            $beforeActiveStoreRules `
            @($script:activeStoreRules) `
            $harnessRoot `
            $script:playwrightFirewallDebtPolicy `
            $AllowColdRegistration `
            $ExpectedPrograms `
            $Phase
        $script:playwrightFirewallDebt = $activeStoreTransition.PlaywrightDebt
        $script:externalFirewallDebt = $activeStoreTransition.ExternalDebt
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

$networkManifest = Get-Content `
    -LiteralPath (Join-Path $PSScriptRoot 'd5-windows-network-packages.json') `
    -Raw |
    ConvertFrom-Json
$allPackages = @($networkManifest.Packages)
$packages = switch ($Mode) {
    'NetworkTests' { $allPackages }
    'Build' { $allPackages }
    'Baseline' { @($allPackages | Where-Object { $_.Name -eq 'webrtc' }) }
    'Profile' { @($allPackages | Where-Object { $_.Name -eq 'webrtc' }) }
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

# NetworkTests runs lean (no evidence run); audited modes create theirs before
# the lease so even a lease-acquisition failure publishes a Failed run.
$script:run = if ($Mode -eq 'NetworkTests') {
    $null
} else {
    $command = "scripts/d5-windows-performance.ps1 -Mode $Mode -BenchTime $BenchTime -Count $Count" + $(if ($Race) { ' -Race' } else { '' })
    New-D5EvidenceRun $repositoryRoot $EvidenceRoot $Mode $command
}
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
$script:activeStoreInitialRules = @()
$script:activeStoreRules = @()
$script:playwrightFirewallDebtPolicy = $null
$script:playwrightFirewallDebtAtStart = $null
$script:playwrightFirewallDebt = $null
$script:externalFirewallDebtAtStart = $null
$script:externalFirewallDebt = $null
$script:publishedResult = $null

function Invoke-D5AuditedRun {
    # The audited pipeline serves every mode except NetworkTests: evidence run,
    # firewall event-log audit chain, ownership baseline, and per-binary source
    # checkpoints behave exactly as before the 2026-07-14 flow split.
    $expectedPrograms = @()
    $networkCapablePrograms = @()
    $builtPrograms = @()
    # GetNewClosure binds the block to a fresh dynamic module that resolves
    # commands through the global scope, so script-scoped functions must be
    # captured as variables to survive CI's dot-sourcing pwsh step wrapper.
    $addSourceCheckpoint = ${function:Add-D5SourceCheckpoint}
    $auditedRun = $script:run
    $buildCheckpoint = {
        param([string]$Name)
        [void](& $addSourceCheckpoint $auditedRun $Name)
    }.GetNewClosure()
    try {
        if ($Mode -eq 'BrowserTests') {
            $browserManifest = @(Get-D5PlaywrightBrowserManifest)
            $cacheRoots = @($browserManifest.cacheRoot | Sort-Object -Unique)
            if ($cacheRoots.Count -ne 1) {
                throw 'Playwright browser manifest must resolve to one cache root'
            }
            $script:playwrightFirewallDebtPolicy = New-D5PlaywrightFirewallDebtPolicy `
                ([string]$cacheRoots[0]) `
                $browserManifest
        }

        # The cursor precedes both initial snapshots so enumeration cannot create a blind baseline.
        $script:auditInitialRecordID = Get-D5LatestFirewallRecordID
        $script:activeStoreInitialRules = @(Get-D5ActiveStoreFirewallRules)
        $script:auditInitialRules = @(Get-D5FirewallRules)
        $script:auditRecordID = [long]$script:auditInitialRecordID
        $script:auditRules = @($script:auditInitialRules)
        $script:activeStoreRules = @($script:activeStoreInitialRules)
        $script:externalFirewallDebtAtStart = New-D5ExternalFirewallDebtSnapshot `
            @($script:activeStoreInitialRules) `
            $harnessRoot `
            $script:playwrightFirewallDebtPolicy
        if ($null -ne $script:playwrightFirewallDebtPolicy) {
            $script:playwrightFirewallDebtAtStart = Get-D5PlaywrightFirewallDebtSnapshot `
                @($script:activeStoreInitialRules) `
                $script:playwrightFirewallDebtPolicy `
                'ActiveStore preflight'
            $script:playwrightFirewallDebt = $script:playwrightFirewallDebtAtStart
        }

        # Snapshots are pure evidence. Persist exact normalized entries, classes,
        # limits and violations before any policy assertion can terminate the run.
        Write-D5JSON (Join-Path $script:run.StagePath 'preflight-rules.json') $script:auditInitialRules
        Write-D5JSON `
            (Join-Path $script:run.StagePath 'active-store-before.json') `
            ([ordered]@{
                Summary = New-D5ActiveStoreFirewallSummary @($script:activeStoreInitialRules)
                PlaywrightDebt = $script:playwrightFirewallDebtAtStart
                ExternalDebt = $script:externalFirewallDebtAtStart
            })
        Assert-D5ExternalFirewallDebtSnapshot `
            $script:externalFirewallDebtAtStart `
            'ActiveStore preflight'
        if ($null -ne $script:playwrightFirewallDebtAtStart) {
            Assert-D5PlaywrightFirewallDebtSnapshot `
                $script:playwrightFirewallDebtAtStart `
                'ActiveStore preflight'
        }
        $script:externalFirewallDebt = New-D5ExternalFirewallTransition `
            @($script:activeStoreInitialRules) `
            @($script:activeStoreInitialRules) `
            $harnessRoot `
            $script:playwrightFirewallDebtPolicy `
            'ActiveStore preflight'
        Assert-D5ExternalFirewallTransition `
            $script:externalFirewallDebt `
            'ActiveStore preflight'
        Write-D5ForeignFirewallRuleWarning $script:auditInitialRules
        if ($null -ne $script:playwrightFirewallDebtAtStart -and
            [int]$script:playwrightFirewallDebtAtStart.PairCount -gt 0) {
            Write-Warning $script:playwrightFirewallDebtAtStart.CleanupAdvisory
        }
        $script:auditInitialized = $true
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
                Build-D5AtomicTestBinary $package $buildCheckpoint
            }
            if ($Mode -eq 'BrowserTests') {
                $env:WINDSHARE_D5_PLAYWRIGHT_OUTPUT_DIR = Join-Path $script:run.StagePath 'browser/test-results'
                $env:WINDSHARE_D5_CHILD_MANIFEST = Join-Path $script:run.StagePath 'browser/launched-binaries.json'
                $builtPrograms += @(Build-D5StableChildren $env:WINDSHARE_D5_CHILD_MANIFEST $buildCheckpoint)
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
        Assert-D5ProgramsExcludeRetiredTombstone `
            $builtPrograms `
            $script:firewallOwnershipPolicy `
            'Audited build plan'
        $script:firewallOwnershipPolicy = New-D5CurrentFirewallOwnershipPolicy

        $expectedPrograms = switch ($Mode) {
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
        if (@($script:operationDefinitions).Count -gt 0) {
            [void](Add-D5SourceCheckpoint $script:run 'before-compiler-execution-plan')
            New-D5CompilerExecutionPlans `
                -Definitions @($script:operationDefinitions) `
                -PlanRoot $script:run.StagePath `
                -Source $script:run.SourceAtStart
            [void](Add-D5SourceCheckpoint $script:run 'after-compiler-execution-plan')
        }

        $networkCapablePrograms = switch ($Mode) {
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
        Assert-D5ProgramsExcludeRetiredTombstone `
            @($expectedPrograms) `
            $script:firewallOwnershipPolicy `
            'Audited launch plan'
        Assert-D5ProgramsExcludeRetiredTombstone `
            @($networkCapablePrograms) `
            $script:firewallOwnershipPolicy `
            'Audited network launch plan'
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

        if ($Mode -in @('Baseline', 'Profile')) {
            $manifests = @(
                foreach ($name in $script:binaries.Keys | Sort-Object) {
                    Get-D5BinaryManifest $name
                }
            )
            Write-D5JSON (Join-Path $script:run.StagePath 'test-manifest.json') $manifests
        }

        switch ($Mode) {
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
            Write-D5JSON `
                (Join-Path $script:run.StagePath 'active-store-after.json') `
                ([ordered]@{
                    Summary = New-D5ActiveStoreFirewallSummary @($script:activeStoreRules)
                    PlaywrightDebt = $script:playwrightFirewallDebt
                    ExternalDebt = $script:externalFirewallDebt
                })
            if ($null -ne $script:playwrightFirewallDebt -and
                [int]$script:playwrightFirewallDebt.PairCount -gt
                    [int]$script:playwrightFirewallDebtAtStart.PairCount) {
                Write-Warning (
                    'Playwright firewall debt increased during this run. ' +
                    $script:playwrightFirewallDebt.CleanupAdvisory
                )
            }
            if ($null -ne $script:externalFirewallDebt -and
                ([int]$script:externalFirewallDebt.AddedRuleCount -gt 0 -or
                 [int]$script:externalFirewallDebt.RemovedRuleCount -gt 0 -or
                 [int]$script:externalFirewallDebt.ChangedRuleCount -gt 0)) {
                Write-Warning (
                    'External firewall telemetry changed during this run: ' +
                    "added=$($script:externalFirewallDebt.AddedRuleCount), " +
                    "removed=$($script:externalFirewallDebt.RemovedRuleCount), " +
                    "changed=$($script:externalFirewallDebt.ChangedRuleCount). " +
                    $script:externalFirewallDebt.CleanupAdvisory
                )
            }
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

function Invoke-D5NetworkTestsRun {
    # Owner decision 2026-07-14 (supersedes the 2026-07-13 per-package forensics
    # decision): NetworkTests is a lean deterministic-execution flow. It keeps
    # the exclusive harness lease, fixed-path builds, compiler execution plans,
    # the registration-pair check, and the one-use capability handshake; it
    # deliberately does NOT create an evidence run, source checkpoints, firewall
    # event-log audits or quiescence waits, zero-delta snapshot assertions, or
    # per-binary program-evidence re-hash sweeps, and foreign WindShare-
    # attributable rules outside the harness root demote to a one-line warning.
    $runRoot = Join-Path $harnessRoot 'network-run'
    $logRoot = Join-Path $runRoot 'logs'
    if (Test-Path -LiteralPath $runRoot) {
        Remove-Item -LiteralPath $runRoot -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path $runRoot, $logRoot | Out-Null
    # One identity read at flow start: the compiler plan schema embeds and
    # re-verifies it, replacing the per-binary checkpoint sweeps.
    $source = Get-D5SourceIdentity $repositoryRoot
    $noCheckpoint = { param([string]$Name) }

    Push-Location $repositoryRoot
    try {
        foreach ($package in $packages) {
            Build-D5AtomicTestBinary $package $noCheckpoint
        }
        $builtPrograms = @(Build-D5StableChildren `
            (Join-Path $runRoot 'e2e-child-manifest.json') `
            $noCheckpoint)
    } finally {
        Pop-Location
    }
    $builtPrograms += @($script:binaries.Values)
    $builtPrograms = @($builtPrograms | ForEach-Object { [IO.Path]::GetFullPath([string]$_) } | Sort-Object -Unique)
    Assert-D5ProgramsExcludeRetiredTombstone `
        $builtPrograms `
        $script:firewallOwnershipPolicy `
        'NetworkTests build and launch plan'
    # One hash per binary: this manifest is what every launch validation and
    # authorization handshake re-verifies against.
    $script:initialProgramEvidence = @(Get-D5BinaryEvidence $builtPrograms)
    # Post-build hashes feed rule attribution for the foreign-rule warning.
    $script:firewallOwnershipPolicy = New-D5CurrentFirewallOwnershipPolicy

    $script:operationDefinitions = @(Get-D5TestExecutionDefinitions)
    New-D5CompilerExecutionPlans `
        -Definitions @($script:operationDefinitions) `
        -PlanRoot $runRoot `
        -Source $source
    foreach ($definition in $script:operationDefinitions) {
        # The compiler's enumeration is authoritative; an empty plan would
        # otherwise pass validation silently as SelectionClass 'empty'.
        $binding = $script:executionPlans[[string]$definition.RequestID]
        if (@($binding.Plan.Entries).Count -eq 0) {
            throw "Stable binary $($script:binaries[[string]$definition.Name]) contains no tests"
        }
    }

    $rules = @(Get-D5FirewallRules)
    Write-D5ForeignFirewallRuleWarning $rules
    # The registration-pair state is what keeps inbound sockets deterministic
    # with firewall popups disabled: default-deny is silent otherwise.
    $state = if (Test-Path -LiteralPath $registrationStatePath -PathType Leaf) {
        Get-Content -LiteralPath $registrationStatePath -Raw | ConvertFrom-Json
    } else {
        $null
    }
    if ($null -ne $state) {
        Assert-D5FirewallRegistrationEntries $state $rules $script:firewallOwnershipPolicy
    } else {
        $state = New-D5FirewallRegistrationEntries $rules $script:firewallOwnershipPolicy
        Save-D5FirewallRegistrationState $state
    }
    $pending = @(Get-D5PendingRegistrationPrograms $state $builtPrograms)

    Assert-D5HarnessLeaseHeld $script:harnessLease
    $workers = [Collections.Generic.List[object]]::new()
    $results = @()
    try {
        foreach ($definition in $script:operationDefinitions) {
            $requestID = [string]$definition.RequestID
            $binding = $script:executionPlans[$requestID]
            $logPath = Join-Path $logRoot (([string]$definition.LogName).Replace('{phase}', 'network'))
            $workers.Add((Start-D5PlannedTestProcess `
                -Plan $binding.Plan `
                -ParentPrograms @($script:initialProgramEvidence) `
                -ExpectedSourceIdentity $source `
                -ExpectedRunID $script:ownerID `
                -ExpectedRequestID $requestID `
                -ExpectedPlanSHA256 $binding.PlanSHA256 `
                -Name ([string]$definition.Name) `
                -LogPath $logPath))
        }
        $results = @(Wait-D5PlannedTestProcesses `
            -Workers @($workers) `
            -Timeout $script:D5NetworkJoinTimeout)
    } catch {
        # Drain and log already-launched siblings before teardown so a
        # mid-batch launch failure still leaves their logs to inspect.
        foreach ($worker in @($workers)) {
            try {
                [void](Complete-D5PlannedTestWorker $worker 'sibling launch failed' 'LaunchAborted')
            } catch {
                # Best-effort: the teardown below still kills and releases.
            }
        }
        Stop-D5PlannedTestProcesses @($workers)
        throw
    }

    # Covers e2e-spawned grandchildren under the harness root: a process-
    # namespace drain, not a firewall event-log timer.
    Wait-D5HarnessNamespaceQuiescent $script:harnessLease $harnessRoot

    $failedResults = @($results | Where-Object {
        $_.ExitCode -ne 0 -or -not [string]::IsNullOrEmpty([string]$_.Failure)
    })
    $registrationError = $null
    if ($pending.Count -gt 0) {
        $misshapenError = $null
        try {
            # Rule minting is synchronous with a child's first bind and the
            # namespace is quiescent, so a fresh snapshot needs no settle-wait.
            $freshRules = @(Get-D5FirewallRules)
            # A failed batch must not persist 'NoRegistration' for a program
            # that may never have reached its first bind (e.g. a JoinTimeout
            # kill), and one misshapen mint must not block validly-minted
            # siblings: record shape-valid full pairs always, 'NoRegistration'
            # only after an all-clean join, and leave everything else pending
            # for the next run to retry.
            $recordablePrograms = [Collections.Generic.List[string]]::new()
            $unrecordedPrograms = [Collections.Generic.List[string]]::new()
            foreach ($program in $pending) {
                $programRules = @($freshRules | Where-Object {
                    [IO.Path]::GetFullPath([string]$_.Program).Equals(
                        $program,
                        [StringComparison]::OrdinalIgnoreCase
                    )
                })
                if ($programRules.Count -eq 0) {
                    if ($failedResults.Count -eq 0) {
                        $recordablePrograms.Add($program)
                    } else {
                        $unrecordedPrograms.Add($program)
                    }
                    continue
                }
                try {
                    Assert-D5FixedRegistrationRules $programRules $script:firewallOwnershipPolicy
                    Assert-D5ColdRegistrationRuleSet `
                        $programRules `
                        @($program) `
                        'NetworkTests cold registration'
                    $recordablePrograms.Add($program)
                } catch {
                    if ($null -eq $misshapenError) {
                        $misshapenError = $_
                    }
                    $unrecordedPrograms.Add($program)
                }
            }
            if ($recordablePrograms.Count -gt 0) {
                # Rules of unrecorded programs are filtered out so the final
                # whole-state validation inside Complete does not trip over
                # exactly what is being left pending.
                $unrecordedSet = New-D5ProgramSet @($unrecordedPrograms)
                $attemptRules = @($freshRules | Where-Object {
                    -not $unrecordedSet.Contains([IO.Path]::GetFullPath([string]$_.Program))
                })
                $state = Complete-D5FirewallRegistrationEntries `
                    $state `
                    $attemptRules `
                    @($recordablePrograms) `
                    $script:firewallOwnershipPolicy
                Save-D5FirewallRegistrationState $state
            }
            if ($unrecordedPrograms.Count -gt 0) {
                Write-Warning ('Bounded registration attempt left unrecorded for still-pending program(s): ' +
                    ($unrecordedPrograms -join ', ') + '; the next run retries it.')
            }
        } catch {
            $registrationError = $_
        }
        if ($null -eq $registrationError) {
            # A misshapen mint is recorded nowhere but must still fail the
            # run loudly — after the results table below, never instead of it.
            $registrationError = $misshapenError
        }
    }

    Write-Output ''
    Write-Output '== network results =='
    $nameWidth = (@($results | ForEach-Object { ([string]$_.Name).Length }) + @(4) |
        Measure-Object -Maximum).Maximum
    $dispositionWidth = (@($results | ForEach-Object { ([string]$_.Disposition).Length }) + @(11) |
        Measure-Object -Maximum).Maximum
    Write-Output ('{0}  {1,-5}  {2,-5}  {3}  {4}' -f
        'name'.PadRight($nameWidth), 'exit', 'time', 'disposition'.PadRight($dispositionWidth), 'log')
    foreach ($result in $results) {
        Write-Output ('{0}  {1,-5}  {2,-5}  {3}  {4}' -f
            ([string]$result.Name).PadRight($nameWidth),
            $result.ExitCode,
            ('{0:mm\:ss}' -f $result.Duration),
            ([string]$result.Disposition).PadRight($dispositionWidth),
            [string]$result.LogPath)
    }
    $problems = [Collections.Generic.List[string]]::new()
    if ($failedResults.Count -gt 0) {
        $summary = @($failedResults | ForEach-Object {
            if (-not [string]::IsNullOrEmpty([string]$_.Failure)) {
                "$($_.Name) ($($_.Failure))"
            } else {
                "$($_.Name) (exit $($_.ExitCode))"
            }
        }) -join ', '
        $problems.Add("$($failedResults.Count) network package(s) failed: $summary; see logs above")
    }
    if ($null -ne $registrationError) {
        $problems.Add("registration completion failed: $registrationError")
    }
    if ($problems.Count -gt 0) {
        throw ($problems -join '; ')
    }
}

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
    Assert-D5ProgramsExcludeRetiredTombstone `
        @($script:authorizedStablePrograms) `
        $script:firewallOwnershipPolicy `
        'Stable build manifest'
    # Two self-contained flows, dispatched once on $Mode: NetworkTests runs the
    # lean deterministic path (owner decision 2026-07-14); every other mode
    # keeps the audited pipeline unchanged.
    if ($Mode -eq 'NetworkTests') {
        Invoke-D5NetworkTestsRun
    } else {
        Invoke-D5AuditedRun
    }
} catch {
    Add-D5RunnerFailure $_
}

$restoreEnvironment = {
    $restoreErrors = [Collections.Generic.List[string]]::new()
    foreach ($name in $environmentNames) {
        try {
            $saved = $savedEnvironment[$name]
            if ($saved.Defined) {
                [Environment]::SetEnvironmentVariable($name, [string]$saved.Value, 'Process')
            } else {
                [Environment]::SetEnvironmentVariable($name, $null, 'Process')
            }
        } catch {
            $restoreErrors.Add("${name}: $_")
        }
    }
    if ($restoreErrors.Count -ne 0) {
        throw ($restoreErrors -join '; ')
    }
}.GetNewClosure()
$releaseRunnerGuard = ${function:Release-D5RunnerGuard}
$runnerGuardToRelease = $script:runnerGuard
$releaseRunnerGuardAction = {
    if ($null -ne $runnerGuardToRelease) {
        & $releaseRunnerGuard $runnerGuardToRelease
    }
}.GetNewClosure()
$releaseHarnessLease = ${function:Release-D5HarnessLease}
$harnessLeaseToRelease = $script:harnessLease
$releaseHarnessLeaseAction = {
    if ($null -ne $harnessLeaseToRelease) {
        & $releaseHarnessLease $harnessLeaseToRelease
    }
}.GetNewClosure()
$addFinalSourceCheckpoint = ${function:Add-D5SourceCheckpoint}
$runToFinalize = $script:run
$finalSourceAudit = {
    if ($null -ne $runToFinalize) {
        [void](& $addFinalSourceCheckpoint $runToFinalize 'after-environment-and-harness-cleanup')
    }
}.GetNewClosure()

# A content-addressed Success becomes visible only after every process-global
# restoration, audited teardown, and final source checkpoint has succeeded.
$requestedStatus = if ($null -eq $script:failure) { 'Success' } else { 'Failed' }
$initialError = if ($null -eq $script:failure) { '' } else { [string]$script:failure }
try {
    $transaction = Complete-D5EvidenceTransaction `
        $script:run `
        $requestedStatus `
        $initialError `
        @(
            [pscustomobject]@{ Name = 'environment restoration'; Action = $restoreEnvironment },
            [pscustomobject]@{ Name = 'runner guard cleanup'; Action = $releaseRunnerGuardAction },
            [pscustomobject]@{ Name = 'global harness lease cleanup'; Action = $releaseHarnessLeaseAction },
            [pscustomobject]@{ Name = 'final source audit'; Action = $finalSourceAudit }
        )
    $script:publishedResult = $transaction.Result
    if ($transaction.Status -ne 'Success') {
        $script:failure = [Exception]::new([string]$transaction.Error)
    }
} catch {
    Add-D5RunnerFailure $_ 'evidence publication transaction failed'
}

if ($null -ne $script:publishedResult) {
    Write-Output "D5 evidence published without overwrite: $($script:publishedResult.Path)"
}
if ($null -ne $script:failure) {
    throw $script:failure
}
