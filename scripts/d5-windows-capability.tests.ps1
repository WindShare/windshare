Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    Write-Output 'D5 Windows capability tests SKIP: Windows-only named-pipe semantics'
    return
}

. (Join-Path $PSScriptRoot 'd5-windows-runner-guard.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-test-process.ps1')

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

function Copy-TestObject([object]$Value) {
    return $Value | ConvertTo-Json -Depth 24 | ConvertFrom-Json
}

function New-ProgramEvidence([string]$Path, [string]$Hash = '') {
    $item = Get-Item -LiteralPath $Path
    if ([string]::IsNullOrWhiteSpace($Hash)) {
        $Hash = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    return [pscustomobject][ordered]@{
        Path = [IO.Path]::GetFullPath($Path)
        Bytes = [long]$item.Length
        SHA256 = $Hash
    }
}

function New-ProbeProcess(
    [string]$Path,
    [string]$PipeName = '',
    [string[]]$Arguments = @()
) {
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = [IO.Path]::GetFullPath($Path)
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    if (-not [string]::IsNullOrWhiteSpace($PipeName)) {
        $start.Environment['WINDSHARE_D5_AUTHORIZATION_PIPE'] = $PipeName
    }
    foreach ($argument in $Arguments) {
        $start.ArgumentList.Add($argument)
    }
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $start
    if (-not $process.Start()) {
        throw "Could not start capability probe $Path"
    }
    return $process
}

function Wait-Probe([Diagnostics.Process]$Process, [string]$Expected) {
    $stdout = $Process.StandardOutput.ReadToEndAsync()
    $stderr = $Process.StandardError.ReadToEndAsync()
    $Process.WaitForExit()
    $output = @(
        @($stdout.GetAwaiter().GetResult() -split '\r?\n')
        @($stderr.GetAwaiter().GetResult() -split '\r?\n')
    )
    if ($Process.ExitCode -ne 0 -or @($output | Where-Object { $_ -eq $Expected }).Count -ne 1) {
        throw "Capability probe returned '$($output -join "`n")', want $Expected"
    }
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-d5-capability-' + [guid]::NewGuid().ToString('N'))
$probe = Join-Path $testRoot 'capability-probe.exe'
$copy = Join-Path $testRoot 'forged-probe.exe'
$enumerationProbe = Join-Path $testRoot 'enumeration-probe.exe'
$networkEnumerationProbe = Join-Path $testRoot 'network-enumeration-probe.exe'
$enumerationCopy = Join-Path $testRoot 'forged-enumeration-probe.exe'
$heldProbe = $null
$heldAuthorization = $null
$environmentNames = @(
    'WINDSHARE_WINDOWS_OS_NETWORK',
    'WINDSHARE_D5_HARNESS_CAPABILITY',
    'WINDSHARE_D5_AUTHORIZATION_MANIFEST',
    'WINDSHARE_D5_AUTHORIZATION_PIPE',
    'WINDSHARE_D5_E2E_LEASE_TOKEN',
    'WINDSHARE_D5_RUNNER_PIPE',
    'WINDSHARE_D5_CHILD_MANIFEST'
)
$saved = @{}
try {
    foreach ($name in $environmentNames) {
        $saved[$name] = [pscustomobject]@{
            Defined = Test-Path "Env:$name"
            Value = [Environment]::GetEnvironmentVariable($name, 'Process')
        }
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }
    New-Item -ItemType Directory -Force -Path $testRoot | Out-Null
    Push-Location $repositoryRoot
    try {
        & go build -o $probe ./scripts/internal/d5capabilityprobe
        if ($LASTEXITCODE -ne 0) {
            throw 'Could not build the D5 capability probe'
        }
        & go build -ldflags '-X=main.probeMode=enumeration' -o $enumerationProbe ./scripts/internal/d5capabilityprobe
        if ($LASTEXITCODE -ne 0) {
            throw 'Could not build the D5 enumeration probe'
        }
        & go build -ldflags '-X=main.probeMode=network-enumeration' -o $networkEnumerationProbe ./scripts/internal/d5capabilityprobe
        if ($LASTEXITCODE -ne 0) {
            throw 'Could not build the D5 network-enumeration probe'
        }
    } finally {
        Pop-Location
    }
    Copy-Item -LiteralPath $probe -Destination $copy
    Copy-Item -LiteralPath $enumerationProbe -Destination $enumerationCopy
    $program = New-ProgramEvidence $probe
    $enumerationProgram = New-ProgramEvidence $enumerationProbe
    $networkEnumerationProgram = New-ProgramEvidence $networkEnumerationProbe

    # Public strings, copied tokens, and a child-selected manifest path have no
    # authority because the Go boundary accepts only a parent-owned one-use pipe.
    $env:WINDSHARE_WINDOWS_OS_NETWORK = 'stable-harness-v3'
    $env:WINDSHARE_D5_HARNESS_CAPABILITY = '0123456789abcdef0123456789abcdef'
    $env:WINDSHARE_D5_AUTHORIZATION_MANIFEST = Join-Path $testRoot 'forged.json'
    $unauthorized = New-ProbeProcess $probe
    try {
        Wait-Probe $unauthorized 'unauthorized'
    } finally {
        $unauthorized.Dispose()
    }

    $authorization = New-D5LaunchAuthorization 'capability-regression' @($program)
    $authorized = New-ProbeProcess $probe $authorization.Name
    $reuse = $null
    try {
        Complete-D5LaunchAuthorization $authorization $authorized $probe
        Wait-Probe $authorized 'authorized'
        $reuse = New-ProbeProcess $probe $authorization.Name
        Assert-Throws {
            Complete-D5LaunchAuthorization $authorization $reuse $probe
        } 'already consumed or released'
    } finally {
        if ($null -ne $reuse -and -not $reuse.HasExited) { $reuse.Kill($true); $reuse.WaitForExit() }
        if ($null -ne $reuse) { $reuse.Dispose() }
        if (-not $authorized.HasExited) { $authorized.Kill($true); $authorized.WaitForExit() }
        $authorized.Dispose()
        Release-D5LaunchAuthorization $authorization
    }

    # This is the forgery the rejected design admitted: identical bytes at an
    # arbitrary fixed-root path with a copied child environment. The parent PID
    # matches, but parent-owned expected path identity still rejects it.
    $forgeryAuthorization = New-D5LaunchAuthorization 'capability-forgery' @($program)
    $forgery = New-ProbeProcess $copy $forgeryAuthorization.Name
    try {
        Assert-Throws {
            Complete-D5LaunchAuthorization $forgeryAuthorization $forgery $probe
        } 'executable .* does not match'
    } finally {
        if (-not $forgery.HasExited) { $forgery.Kill($true); $forgery.WaitForExit() }
        $forgery.Dispose()
        Release-D5LaunchAuthorization $forgeryAuthorization
    }

    $badHash = New-ProgramEvidence $probe ('0' * 64)
    $hashAuthorization = New-D5LaunchAuthorization 'capability-hash-forgery' @($badHash)
    $hashForgery = New-ProbeProcess $probe $hashAuthorization.Name
    try {
        Assert-Throws {
            Complete-D5LaunchAuthorization $hashAuthorization $hashForgery $probe
        } 'differs from the parent-owned launch set'
    } finally {
        if (-not $hashForgery.HasExited) { $hashForgery.Kill($true); $hashForgery.WaitForExit() }
        $hashForgery.Dispose()
        Release-D5LaunchAuthorization $hashAuthorization
    }

    $execution = Invoke-D5NetworkAuthorizedTestProcess `
        -RunID 'execution-regression' `
        -Programs @($program) `
        -Executable $probe `
        -Arguments @() `
        -WorkingDirectory $testRoot
    if ($execution.ExitCode -ne 0 -or
        @($execution.StandardOutput -split '\r?\n' | Where-Object { $_ -eq 'authorized' }).Count -ne 1) {
        throw 'The explicit network execution operation did not complete its one-use handshake'
    }
    if ($null -ne $execution.AuthorizationRecord.PSObject.Properties['Pipe']) {
        throw 'A consumed launch pipe leaked into the execution record'
    }

    $sourceIdentity = [pscustomobject][ordered]@{
        IdentityKind = 'workspace-manifest'
        Commit = '0123456789abcdef0123456789abcdef01234567'
        WorktreeClean = $false
        SourceDigest = 'a' * 64
    }
    $sourceCheckpoint = {
        param([string]$Boundary)
        $sourceIdentity
    }.GetNewClosure()

    # Enumeration must remain safe even when the invoking shell carries every
    # historical authority label: the child receives none of them and never gates.
    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, 'forged-enumeration-authority', 'Process')
    }
    $enumeration = New-D5TestEnumerationOperation `
        -Executable $enumerationProbe `
        -WorkingDirectory $testRoot `
        -ProgramEvidence $enumerationProgram `
        -SourceIdentity $sourceIdentity
    $listed = Invoke-D5TestEnumerationProcess `
        -Operation $enumeration `
        -ParentPrograms @($enumerationProgram) `
        -ExpectedSourceIdentity $sourceIdentity `
        -SourceCheckpoint $sourceCheckpoint
    if ($listed.ExitCode -ne 0 -or
        @($listed.StandardOutput -split '\r?\n' | Where-Object { $_ -eq 'TestEnumerationProbe' }).Count -ne 1 -or
        -not [string]::IsNullOrWhiteSpace($listed.StandardError)) {
        throw 'The explicit non-network test enumeration did not return its exact manifest entry'
    }
    if ([string]$listed.OperationRecord.Authorization -ne 'none' -or
        $null -ne $listed.OperationRecord.PSObject.Properties['Pipe']) {
        throw 'Non-network enumeration exposed a launch authorization'
    }
    foreach ($name in $environmentNames) {
        if ([Environment]::GetEnvironmentVariable($name, 'Process') -ne 'forged-enumeration-authority') {
            throw "Enumeration mutated its parent authority environment: $name"
        }
    }

    Assert-Throws {
        New-D5TestEnumerationOperation `
            -Executable $enumerationCopy `
            -WorkingDirectory $testRoot `
            -ProgramEvidence $enumerationProgram `
            -SourceIdentity $sourceIdentity
    } 'does not match parent-owned path'

    $copiedOperation = New-D5TestEnumerationOperation `
        -Executable $enumerationProbe `
        -WorkingDirectory $testRoot `
        -ProgramEvidence $enumerationProgram `
        -SourceIdentity $sourceIdentity
    $copiedOperation.Executable.Path = $enumerationCopy
    Assert-Throws {
        Invoke-D5TestEnumerationProcess `
            -Operation $copiedOperation `
            -ParentPrograms @($enumerationProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -SourceCheckpoint $sourceCheckpoint
    } 'does not contain exactly one record'

    $badEnumerationHash = New-ProgramEvidence $enumerationProbe ('0' * 64)
    Assert-Throws {
        New-D5TestEnumerationOperation `
            -Executable $enumerationProbe `
            -WorkingDirectory $testRoot `
            -ProgramEvidence $badEnumerationHash `
            -SourceIdentity $sourceIdentity
    } 'differs from the parent-owned process set'

    $alteredArguments = New-D5TestEnumerationOperation `
        -Executable $enumerationProbe `
        -WorkingDirectory $testRoot `
        -ProgramEvidence $enumerationProgram `
        -SourceIdentity $sourceIdentity
    $alteredArguments.Arguments = @('-test.list=.*')
    Assert-Throws {
        Invoke-D5TestEnumerationProcess `
            -Operation $alteredArguments `
            -ParentPrograms @($enumerationProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -SourceCheckpoint $sourceCheckpoint
    } 'exact Go test-list arguments'

    $alteredSource = New-D5TestEnumerationOperation `
        -Executable $enumerationProbe `
        -WorkingDirectory $testRoot `
        -ProgramEvidence $enumerationProgram `
        -SourceIdentity $sourceIdentity
    $alteredSource.Source.SourceDigest = 'b' * 64
    Assert-Throws {
        Invoke-D5TestEnumerationProcess `
            -Operation $alteredSource `
            -ParentPrograms @($enumerationProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -SourceCheckpoint $sourceCheckpoint
    } 'source identity differs'

    $networkEnumeration = New-D5TestEnumerationOperation `
        -Executable $networkEnumerationProbe `
        -WorkingDirectory $testRoot `
        -ProgramEvidence $networkEnumerationProgram `
        -SourceIdentity $sourceIdentity
    $networkListing = Invoke-D5TestEnumerationProcess `
        -Operation $networkEnumeration `
        -ParentPrograms @($networkEnumerationProgram) `
        -ExpectedSourceIdentity $sourceIdentity `
        -SourceCheckpoint $sourceCheckpoint
    if ($networkListing.ExitCode -eq 0 -or
        $networkListing.StandardError -notmatch 'enumeration network authorization denied') {
        throw 'A listing process reached the Windows network gate without failing closed'
    }
    $fixtureRoot = Join-Path $testRoot 'execution-plan-fixture'
    $fixturePackage = Join-Path $fixtureRoot 'fixture'
    $fixtureInternal = Join-Path $fixtureRoot 'internal/testnetwork'
    $classifier = Join-Path $testRoot 'd5networkpolicy.exe'
    $fixtureBinary = Join-Path $testRoot 'execution-plan-fixture.test.exe'
    New-Item -ItemType Directory -Force -Path $fixturePackage, $fixtureInternal | Out-Null
    # The fixture reuses the root module identity, sources, and go.sum, so its
    # go directive and x/sys pin must come from the root go.mod; a hardcoded
    # version drifts on every root dependency bump and then fails hash checks.
    $rootModuleJSON = & go -C $repositoryRoot mod edit -json
    if ($LASTEXITCODE -ne 0) {
        throw 'Could not read the root go.mod for the execution-plan fixture'
    }
    $rootModule = ($rootModuleJSON -join "`n") | ConvertFrom-Json
    $fixtureRequires = @(
        $rootModule.Require | Where-Object { [string]$_.Path -eq 'golang.org/x/sys' }
    )
    if ($fixtureRequires.Count -ne 1) {
        throw 'Root go.mod does not pin exactly one golang.org/x/sys for the fixture'
    }
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRoot 'go.mod'),
        @"
module github.com/windshare/windshare

go $($rootModule.Go)

require golang.org/x/sys $($fixtureRequires[0].Version)
"@,
        [Text.UTF8Encoding]::new($false)
    )
    Copy-Item -LiteralPath (Join-Path $repositoryRoot 'go.sum') -Destination (
        Join-Path $fixtureRoot 'go.sum'
    )
    foreach ($file in 'gate.go', 'capability_windows.go', 'capability_other.go') {
        Copy-Item `
            -LiteralPath (Join-Path $repositoryRoot "internal/testnetwork/$file") `
            -Destination (Join-Path $fixtureInternal $file)
    }
    [IO.File]::WriteAllText(
        (Join-Path $fixturePackage 'fixture_test.go'),
        @'
package fixture

import (
    "net"
    "testing"
    "time"

    "github.com/windshare/windshare/internal/testnetwork"
)

func invoke(callback func()) { callback() }
func pureCallback() {}
func networkCallback() { testnetwork.AssertOSNetwork() }

func BenchmarkPure(b *testing.B) {
    for range b.N {
        invoke(pureCallback)
    }
}

func BenchmarkNetwork(b *testing.B) {
    testnetwork.RequireOSNetwork(b)
    for range b.N {
        invoke(networkCallback)
    }
}

func TestSubtests(t *testing.T) {
    t.Run("pure", func(*testing.T) {})
    t.Run("network", func(*testing.T) { invoke(networkCallback) })
}

// TestHold outlives any reasonable join deadline so the parallel wait loop's
// watchdog kill path can be exercised deterministically.
func TestHold(*testing.T) {
    time.Sleep(30 * time.Second)
}

// TestFail gives the parallel aggregation contract a deterministic nonzero exit.
func TestFail(t *testing.T) {
    t.Fatal("deliberate fixture failure")
}

// semanticBoundary keeps the fixture inside the same gate/resource proof as the
// repository without executing a socket in this socket-free lifecycle suite.
func semanticBoundary(t *testing.T) {
    testnetwork.RequireOSNetwork(t)
    listener, _ := net.Listen("tcp", "127.0.0.1:0")
    if listener != nil {
        _ = listener.Close()
    }
}
'@,
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRoot 'fixture-manifest.json'),
        '[{"Name":"fixture","Path":"./fixture"}]',
        [Text.UTF8Encoding]::new($false)
    )

    Push-Location $repositoryRoot
    try {
        & go build -o $classifier ./scripts/internal/d5networkpolicy
        if ($LASTEXITCODE -ne 0) {
            throw 'Could not build the compiler execution-plan classifier'
        }
    } finally {
        Pop-Location
    }
    $savedGoWork = [Environment]::GetEnvironmentVariable('GOWORK', 'Process')
    try {
        [Environment]::SetEnvironmentVariable('GOWORK', 'off', 'Process')
        Push-Location $fixtureRoot
        try {
            & go test -c -o $fixtureBinary ./fixture
            if ($LASTEXITCODE -ne 0) {
                throw 'Could not build the socket-free execution-plan fixture'
            }
        } finally {
            Pop-Location
        }
    } finally {
        [Environment]::SetEnvironmentVariable('GOWORK', $savedGoWork, 'Process')
    }

    $fixtureProgram = New-ProgramEvidence $fixtureBinary
    $planRequestPath = Join-Path $fixtureRoot 'plan-request.json'
    $planOutputPath = Join-Path $fixtureRoot 'plans.json'
    $planRequests = @(
        [ordered]@{
            RequestID = 'pure-benchmark'
            PackagePath = 'fixture'
            Executable = $fixtureProgram
            WorkingDirectory = $fixturePackage
            Arguments = @(
                '-test.run=^$',
                '-test.bench=^BenchmarkPure$',
                '-test.benchtime=1x'
            )
        },
        [ordered]@{
            RequestID = 'network-benchmark'
            PackagePath = 'fixture'
            Executable = $fixtureProgram
            WorkingDirectory = $fixturePackage
            Arguments = @(
                '-test.run=^$',
                '-test.bench=^BenchmarkNetwork$',
                '-test.benchtime=1x'
            )
        },
        [ordered]@{
            RequestID = 'mixed-benchmark'
            PackagePath = 'fixture'
            Executable = $fixtureProgram
            WorkingDirectory = $fixturePackage
            Arguments = @(
                '-test.run=^$',
                '-test.bench=^Benchmark(Pure|Network)$',
                '-test.benchtime=1x'
            )
        },
        [ordered]@{
            RequestID = 'pure-subtest'
            PackagePath = 'fixture'
            Executable = $fixtureProgram
            WorkingDirectory = $fixturePackage
            Arguments = @('-test.run=^TestSubtests$/pure$', '-test.count=1')
        },
        [ordered]@{
            RequestID = 'hold-subtest'
            PackagePath = 'fixture'
            Executable = $fixtureProgram
            WorkingDirectory = $fixturePackage
            Arguments = @('-test.run=^TestHold$', '-test.count=1')
        },
        [ordered]@{
            RequestID = 'fail-subtest'
            PackagePath = 'fixture'
            Executable = $fixtureProgram
            WorkingDirectory = $fixturePackage
            Arguments = @('-test.run=^TestFail$', '-test.count=1')
        }
    )
    [IO.File]::WriteAllText(
        $planRequestPath,
        ([ordered]@{
            SchemaVersion = 1
            RunID = 'execution-plan-regression'
            Source = $sourceIdentity
            Operations = $planRequests
        } | ConvertTo-Json -Depth 16),
        [Text.UTF8Encoding]::new($false)
    )
    & $classifier `
        -root $fixtureRoot `
        -manifest fixture-manifest.json `
        -execution-plan-request $planRequestPath `
        -execution-plan-output $planOutputPath
    if ($LASTEXITCODE -ne 0) {
        throw 'Compiler execution-plan fixture classification failed'
    }
    $planDocument = Get-Content -LiteralPath $planOutputPath -Raw | ConvertFrom-Json
    $plans = @{}
    foreach ($plan in @($planDocument.Plans)) {
        $plans[[string]$plan.RequestID] = $plan
        if ((Get-D5TestExecutionPlanSHA256 $plan) -cne [string]$plan.PlanSHA256) {
            throw "PowerShell and compiler execution-plan digests differ for $($plan.RequestID)"
        }
    }
    $expectedPlanClasses = @{
        'pure-benchmark' = @('none', 'non-network')
        'network-benchmark' = @('parent-owned-one-use-pipe', 'network')
        'mixed-benchmark' = @('parent-owned-one-use-pipe', 'mixed-network')
        'pure-subtest' = @('parent-owned-one-use-pipe', 'network')
    }
    foreach ($requestID in $expectedPlanClasses.Keys) {
        $plan = $plans[$requestID]
        $expectedClass = $expectedPlanClasses[$requestID]
        if ($null -eq $plan -or
            [string]$plan.NetworkAccess -cne $expectedClass[0] -or
            [string]$plan.SelectionClass -cne $expectedClass[1]) {
            throw "Compiler execution plan $requestID has an unexpected network class"
        }
    }

    $planResults = @{}
    foreach ($requestID in @(
        'pure-benchmark',
        'network-benchmark',
        'mixed-benchmark',
        'pure-subtest'
    )) {
        $plan = $plans[$requestID]
        $planResults[$requestID] = Invoke-D5PlannedTestProcess `
            -Plan $plan `
            -ParentPrograms @($fixtureProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -ExpectedRunID 'execution-plan-regression' `
            -ExpectedRequestID $requestID `
            -ExpectedPlanSHA256 $plan.PlanSHA256 `
            -SourceCheckpoint $sourceCheckpoint
        if ($planResults[$requestID].ExitCode -ne 0) {
            throw "Planned fixture execution $requestID failed"
        }
        if ($null -ne $planResults[$requestID].ExecutionRecord.PSObject.Properties['Pipe']) {
            throw "Planned fixture execution $requestID leaked its pipe name"
        }
    }
    if ([string]$planResults['pure-benchmark'].ExecutionRecord.Authorization -cne 'none' -or
        $null -ne $planResults['pure-benchmark'].AuthorizationRecord) {
        throw 'The pure benchmark acquired or waited for network authorization'
    }
    foreach ($requestID in 'network-benchmark', 'mixed-benchmark') {
        if ([string]$planResults[$requestID].AuthorizationRecord.Disposition -cne 'Consumed') {
            throw "Network-capable fixture $requestID did not consume its one-use pipe"
        }
    }
    if ([string]$planResults['pure-subtest'].AuthorizationRecord.Disposition -cne 'UnusedNoGate') {
        throw 'A network-capable parent with a pure subtest did not terminate its unused grant'
    }
    foreach ($name in $environmentNames) {
        if ([Environment]::GetEnvironmentVariable($name, 'Process') -ne
            'forged-enumeration-authority') {
            throw "Planned execution mutated its parent authority environment: $name"
        }
    }

    $purePlan = $plans['pure-benchmark']
    Assert-Throws {
        Invoke-D5PlannedTestProcess `
            -Plan $purePlan `
            -ParentPrograms @($fixtureProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -ExpectedRunID 'stale-run' `
            -ExpectedRequestID 'pure-benchmark' `
            -ExpectedPlanSHA256 $purePlan.PlanSHA256 `
            -SourceCheckpoint $sourceCheckpoint
    } 'identity is invalid or stale'

    $staleSource = Copy-TestObject $sourceIdentity
    $staleSource.SourceDigest = 'b' * 64
    Assert-Throws {
        Invoke-D5PlannedTestProcess `
            -Plan $purePlan `
            -ParentPrograms @($fixtureProgram) `
            -ExpectedSourceIdentity $staleSource `
            -ExpectedRunID 'execution-plan-regression' `
            -ExpectedRequestID 'pure-benchmark' `
            -ExpectedPlanSHA256 $purePlan.PlanSHA256 `
            -SourceCheckpoint $sourceCheckpoint
    } 'source identity differs'

    $alteredArgumentsPlan = Copy-TestObject $purePlan
    $alteredArgumentsPlan.Arguments[1] = '-test.bench=^BenchmarkNetwork$'
    Assert-Throws {
        Invoke-D5PlannedTestProcess `
            -Plan $alteredArgumentsPlan `
            -ParentPrograms @($fixtureProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -ExpectedRunID 'execution-plan-regression' `
            -ExpectedRequestID 'pure-benchmark' `
            -ExpectedPlanSHA256 $purePlan.PlanSHA256 `
            -SourceCheckpoint $sourceCheckpoint
    } 'digest does not match'

    $alteredHashPlan = Copy-TestObject $purePlan
    $alteredHashPlan.Executable.SHA256 = '0' * 64
    Assert-Throws {
        Invoke-D5PlannedTestProcess `
            -Plan $alteredHashPlan `
            -ParentPrograms @($fixtureProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -ExpectedRunID 'execution-plan-regression' `
            -ExpectedRequestID 'pure-benchmark' `
            -ExpectedPlanSHA256 $purePlan.PlanSHA256 `
            -SourceCheckpoint $sourceCheckpoint
    } 'differs from the parent-owned process set'

    $fixtureCopy = Join-Path $testRoot 'copied-execution-plan-fixture.test.exe'
    Copy-Item -LiteralPath $fixtureBinary -Destination $fixtureCopy
    $copiedProgram = New-ProgramEvidence $fixtureCopy
    Assert-Throws {
        Invoke-D5PlannedTestProcess `
            -Plan $purePlan `
            -ParentPrograms @($copiedProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -ExpectedRunID 'execution-plan-regression' `
            -ExpectedRequestID 'pure-benchmark' `
            -ExpectedPlanSHA256 $purePlan.PlanSHA256 `
            -SourceCheckpoint $sourceCheckpoint
    } 'does not contain exactly one record'

    $selfDeclaredPure = Copy-TestObject $plans['network-benchmark']
    $selfDeclaredPure.NetworkAccess = 'none'
    Assert-Throws {
        Invoke-D5PlannedTestProcess `
            -Plan $selfDeclaredPure `
            -ParentPrograms @($fixtureProgram) `
            -ExpectedSourceIdentity $sourceIdentity `
            -ExpectedRunID 'execution-plan-regression' `
            -ExpectedRequestID 'network-benchmark' `
            -ExpectedPlanSHA256 $plans['network-benchmark'].PlanSHA256 `
            -SourceCheckpoint $sourceCheckpoint
    } 'network selection does not match'

    # Parallel launch/join API: one consumed handshake and one terminal-unused
    # grant complete concurrently under a single poll-loop join.
    $parallelLogRoot = Join-Path $testRoot 'parallel-logs'
    New-Item -ItemType Directory -Force -Path $parallelLogRoot | Out-Null
    $happyWorkers = @(
        foreach ($requestID in 'network-benchmark', 'pure-subtest') {
            Start-D5PlannedTestProcess `
                -Plan $plans[$requestID] `
                -ParentPrograms @($fixtureProgram) `
                -ExpectedSourceIdentity $sourceIdentity `
                -ExpectedRunID 'execution-plan-regression' `
                -ExpectedRequestID $requestID `
                -ExpectedPlanSHA256 $plans[$requestID].PlanSHA256 `
                -Name $requestID `
                -LogPath (Join-Path $parallelLogRoot "$requestID.txt")
        }
    )
    $happyResults = @(Wait-D5PlannedTestProcesses `
        -Workers $happyWorkers `
        -Timeout ([timespan]::FromMinutes(5)))
    if ($happyResults.Count -ne 2) {
        throw "Parallel happy path returned $($happyResults.Count) results, want 2"
    }
    $happyByName = @{}
    foreach ($result in $happyResults) {
        $happyByName[[string]$result.Name] = $result
    }
    foreach ($requestID in 'network-benchmark', 'pure-subtest') {
        $result = $happyByName[$requestID]
        if ($null -eq $result -or $result.ExitCode -ne 0 -or $null -ne $result.Failure) {
            throw "Parallel worker $requestID did not complete cleanly"
        }
        $logPath = Join-Path $parallelLogRoot "$requestID.txt"
        if (-not (Test-Path -LiteralPath $logPath -PathType Leaf) -or
            (Get-Content -LiteralPath $logPath -Raw) -notmatch 'PASS') {
            throw "Parallel worker $requestID did not write its suite output to $logPath"
        }
    }
    if ([string]$happyByName['network-benchmark'].Disposition -cne 'Consumed') {
        throw 'A parallel network-capable worker did not consume its one-use pipe'
    }
    if ([string]$happyByName['pure-subtest'].Disposition -cne 'UnusedNoGate') {
        throw 'A parallel worker with a pure subtest did not terminate its unused grant'
    }
    foreach ($worker in $happyWorkers) {
        if ([string]$worker.Authorization.State -cne 'Released') {
            throw "Parallel worker $($worker.Name) authorization was not released"
        }
    }
    foreach ($name in $environmentNames) {
        if ([Environment]::GetEnvironmentVariable($name, 'Process') -ne
            'forged-enumeration-authority') {
            throw "Parallel execution mutated its parent authority environment: $name"
        }
    }

    # Deadline kill: the join watchdog kills an over-deadline worker while its
    # batch sibling still completes and reports.
    $holdWorker = Start-D5PlannedTestProcess `
        -Plan $plans['hold-subtest'] `
        -ParentPrograms @($fixtureProgram) `
        -ExpectedSourceIdentity $sourceIdentity `
        -ExpectedRunID 'execution-plan-regression' `
        -ExpectedRequestID 'hold-subtest' `
        -ExpectedPlanSHA256 $plans['hold-subtest'].PlanSHA256 `
        -Name 'hold-subtest' `
        -LogPath (Join-Path $parallelLogRoot 'hold-subtest.txt')
    $holdPID = $holdWorker.Process.Id
    $fastWorker = Start-D5PlannedTestProcess `
        -Plan $plans['pure-benchmark'] `
        -ParentPrograms @($fixtureProgram) `
        -ExpectedSourceIdentity $sourceIdentity `
        -ExpectedRunID 'execution-plan-regression' `
        -ExpectedRequestID 'pure-benchmark' `
        -ExpectedPlanSHA256 $plans['pure-benchmark'].PlanSHA256 `
        -Name 'pure-benchmark' `
        -LogPath (Join-Path $parallelLogRoot 'deadline-pure-benchmark.txt')
    $deadlineResults = @(Wait-D5PlannedTestProcesses `
        -Workers @($holdWorker, $fastWorker) `
        -Timeout ([timespan]::FromSeconds(3)))
    if ($deadlineResults.Count -ne 2) {
        throw "Deadline join returned $($deadlineResults.Count) results, want 2"
    }
    $deadlineByName = @{}
    foreach ($result in $deadlineResults) {
        $deadlineByName[[string]$result.Name] = $result
    }
    $holdResult = $deadlineByName['hold-subtest']
    if ($null -eq $holdResult -or
        [string]$holdResult.Disposition -cne 'JoinTimeout' -or
        [string]::IsNullOrEmpty([string]$holdResult.Failure) -or
        $holdResult.ExitCode -eq 0) {
        throw 'The join deadline did not record the held worker as a timeout failure'
    }
    if (@(Get-Process -ErrorAction SilentlyContinue | Where-Object Id -eq $holdPID).Count -ne 0) {
        throw "The join deadline left the held worker process $holdPID running"
    }
    $fastResult = $deadlineByName['pure-benchmark']
    if ($null -eq $fastResult -or $fastResult.ExitCode -ne 0 -or $null -ne $fastResult.Failure) {
        throw 'The sibling of a deadline-killed worker did not complete and report'
    }

    # Failure aggregation: all workers run to completion; a failing one carries
    # its exit code while the passing sibling stays green.
    $aggregationWorkers = @(
        foreach ($requestID in 'fail-subtest', 'pure-benchmark') {
            Start-D5PlannedTestProcess `
                -Plan $plans[$requestID] `
                -ParentPrograms @($fixtureProgram) `
                -ExpectedSourceIdentity $sourceIdentity `
                -ExpectedRunID 'execution-plan-regression' `
                -ExpectedRequestID $requestID `
                -ExpectedPlanSHA256 $plans[$requestID].PlanSHA256 `
                -Name $requestID `
                -LogPath (Join-Path $parallelLogRoot "aggregation-$requestID.txt")
        }
    )
    $aggregationResults = @(Wait-D5PlannedTestProcesses `
        -Workers $aggregationWorkers `
        -Timeout ([timespan]::FromMinutes(5)))
    if ($aggregationResults.Count -ne 2) {
        throw "Aggregation join returned $($aggregationResults.Count) results, want 2"
    }
    $aggregationByName = @{}
    foreach ($result in $aggregationResults) {
        $aggregationByName[[string]$result.Name] = $result
    }
    $failResult = $aggregationByName['fail-subtest']
    if ($null -eq $failResult -or
        $failResult.ExitCode -eq 0 -or
        $null -ne $failResult.Failure -or
        [string]$failResult.Disposition -cne 'CompletedWithoutNetworkAuthority') {
        throw 'A failing parallel worker did not carry its own exit code through the join'
    }
    $passResult = $aggregationByName['pure-benchmark']
    if ($null -eq $passResult -or $passResult.ExitCode -ne 0 -or $null -ne $passResult.Failure) {
        throw 'A failing parallel worker condemned its passing sibling'
    }

    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, $null, 'Process')
    }

    $heldAuthorization = New-D5LaunchAuthorization 'capability-wrapper-loss' @($program)
    $heldProbe = New-ProbeProcess $probe $heldAuthorization.Name @('hold-child', $probe)
    Complete-D5LaunchAuthorization $heldAuthorization $heldProbe $probe
    $ready = $heldProbe.StandardOutput.ReadLineAsync()
    if (-not $ready.Wait([timespan]::FromSeconds(10)) -or $ready.Result -ne 'authorized') {
        throw "Held capability probe did not authorize: $($heldProbe.StandardError.ReadToEnd())"
    }
    $childReady = $heldProbe.StandardOutput.ReadLineAsync()
    if (-not $childReady.Wait([timespan]::FromSeconds(10)) -or
        $childReady.Result -notmatch '^child-pid=([1-9][0-9]*)$') {
        throw "Held capability probe did not register its child: $($heldProbe.StandardError.ReadToEnd())"
    }
    $heldChildPID = [int]$Matches[1]
    Release-D5LaunchAuthorization $heldAuthorization
    if (-not $heldProbe.WaitForExit(10000) -or $heldProbe.ExitCode -ne 125) {
        throw "Held capability probe survived parent guard loss or exited unexpectedly: $($heldProbe.ExitCode)"
    }
    if (@(Get-Process | Where-Object Id -eq $heldChildPID).Count -ne 0) {
        throw "Registered child $heldChildPID survived parent guard loss"
    }
} finally {
    foreach ($name in $environmentNames) {
        $record = $saved[$name]
        if ($null -ne $record -and $record.Defined) {
            [Environment]::SetEnvironmentVariable($name, [string]$record.Value, 'Process')
        } else {
            [Environment]::SetEnvironmentVariable($name, $null, 'Process')
        }
    }
    Release-D5LaunchAuthorization $heldAuthorization
    if ($null -ne $heldProbe -and -not $heldProbe.HasExited) {
        $heldProbe.Kill($true)
        $heldProbe.WaitForExit()
    }
    if ($null -ne $heldProbe) {
        $heldProbe.Dispose()
    }
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'D5 Windows test process lifecycle tests PASS'
