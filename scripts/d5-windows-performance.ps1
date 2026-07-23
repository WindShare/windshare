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
if ([string]::IsNullOrWhiteSpace($EvidenceRoot)) {
    $EvidenceRoot = Join-Path $repositoryRoot 'tmp\d5-evidence'
}
# e2e carries the largest per-binary Go-level deadline (10m -test.timeout);
# the parallel join adds 2m grace so this watchdog only fires on a child that
# outlived its own deadline enforcement.
$script:D5NetworkJoinTimeout = [timespan]::FromMinutes(12)
$browserNetworkContractName = 'WINDSHARE_WINDOWS_OS_NETWORK'
$browserNetworkContractValue = 'stable-harness-v3'
$browserLeaseTokenName = 'WINDSHARE_D5_E2E_LEASE_TOKEN'
$launchAuthorizationPipeName = 'WINDSHARE_D5_AUTHORIZATION_PIPE'
$runnerGuardName = 'WINDSHARE_D5_RUNNER_PIPE'

. (Join-Path $PSScriptRoot 'd5-evidence.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-stable-e2e-lease.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-runner-guard.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-test-process.ps1')
. (Join-Path $PSScriptRoot 'go-benchmark-evidence.ps1')
. (Join-Path $PSScriptRoot 'd5-pion-performance-evidence.ps1')

function Write-D5JSON([string]$Path, [object]$Value) {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Path) | Out-Null
    [IO.File]::WriteAllText(
        $Path,
        ($Value | ConvertTo-Json -Depth 16),
        [Text.UTF8Encoding]::new($false)
    )
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

function Invoke-D5Phase([string]$Phase, [scriptblock]$Action) {
    Assert-D5HarnessLeaseHeld $script:harnessLease
    Write-Output "-- D5 phase: $Phase"
    & $Action
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
$script:failure = $null
$script:harnessLease = $null
$script:runnerGuard = $null
$script:initialProgramEvidence = @()
$script:operationDefinitions = @()
$script:executionPlans = @{}
$script:executionRecords = [Collections.Generic.List[object]]::new()
$script:authorizationRecords = [Collections.Generic.List[object]]::new()
$script:enumerationRecords = [Collections.Generic.List[object]]::new()
$script:publishedResult = $null

function Invoke-D5AuditedRun {
    # Evidence-bearing modes preserve source identity, binary identity, process
    # ownership and exact launch records. Host firewall state is intentionally
    # outside this authority: prompts and rule changes are Windows-owned input,
    # not a reason to prevent the product tests from running.
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
        Write-D5JSON (Join-Path $script:run.StagePath 'binary-manifest.json') ([ordered]@{
            Mode = $Mode
            ExpectedLaunchedPrograms = $expectedPrograms
            NetworkCapablePrograms = $networkCapablePrograms
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
                Invoke-D5Phase 'browser' { Invoke-D5SelectedMode 'browser' }
                $launched = Get-Content -LiteralPath $env:WINDSHARE_D5_CHILD_MANIFEST -Raw | ConvertFrom-Json
                $manifestPrograms = @($launched.binaries.Path | ForEach-Object { [IO.Path]::GetFullPath([string]$_) } | Sort-Object)
                if ($manifestPrograms.Count -ne $expectedPrograms.Count -or
                    @(Compare-Object -ReferenceObject @($expectedPrograms | Sort-Object) -DifferenceObject $manifestPrograms).Count -ne 0) {
                    throw 'Browser runner manifest does not contain its exact per-mode program set'
                }
            }
            'Baseline' {
                Invoke-D5Phase 'measurement' { Invoke-D5SelectedMode 'measurement' }
            }
            'Profile' {
                Invoke-D5Phase 'measurement' { Invoke-D5SelectedMode 'measurement' }
            }
            'OrdinaryTests' {
                Invoke-D5Phase 'ordinary' { Invoke-D5SelectedMode 'ordinary' }
            }
            'Build' {
                Invoke-D5Phase 'build' { }
            }
            'Audit' {
                Invoke-D5Phase 'audit' { }
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
        [void](Add-D5SourceCheckpoint $script:run 'after-execution-before-final-verification')
        $connected = @($script:runnerGuard.Connections | Where-Object IsCompletedSuccessfully).Count
        Write-D5JSON (Join-Path $script:run.StagePath 'runner-guard.json') ([ordered]@{
            ConnectedProcesses = $connected
            Capacity = $script:D5RunnerGuardCapacity
        })
        if ($Mode -eq 'BrowserTests') {
            Assert-D5RunnerGuardConnected $script:runnerGuard
        }
    } catch {
        Add-D5RunnerFailure $_ 'post-run verification failed'
    }

}

function Invoke-D5NetworkTestsRun {
    # The network flow derives its authority from the exclusive harness lease,
    # fixed-path hashes, compiler execution plans and one-use capability. Windows
    # Firewall may prompt or change host-owned rules without changing that test
    # contract, so the runner never queries or classifies firewall state.
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
    # One hash per binary: this manifest is what every launch validation and
    # authorization handshake re-verifies against.
    $script:initialProgramEvidence = @(Get-D5BinaryEvidence $builtPrograms)

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
    # Two self-contained flows, dispatched once on $Mode: NetworkTests runs the
    # lean deterministic path; every other mode additionally publishes evidence.
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
