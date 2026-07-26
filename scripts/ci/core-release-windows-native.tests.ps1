Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'core-release-windows-native.psm1') -Force

function Assert-Throws([string]$Label, [scriptblock]$Body) {
    $threw = $false
    try {
        & $Body
    } catch {
        $threw = $true
    }
    if (-not $threw) {
        throw "$Label did not fail closed"
    }
}

function Assert-FileContains([string]$Path, [string]$Expected) {
    $content = [IO.File]::ReadAllText($Path)
    if (-not $content.Contains($Expected, [StringComparison]::Ordinal)) {
        throw "$Path is missing required release contract text: $Expected"
    }
}

function Assert-FileDoesNotContain([string]$Path, [string]$Forbidden) {
    $content = [IO.File]::ReadAllText($Path)
    if ($content.Contains($Forbidden, [StringComparison]::Ordinal)) {
        throw "$Path contains forbidden release contract text: $Forbidden"
    }
}

$requiredTests = @(Get-WindowsNativeRequiredTestNames)
$expectedExpression = '^(TestWindowsNTFSNativeCertification|TestWindowsNTFSProcessRestartRecovery|TestWindowsNTFSProbeMutexIsProcessExclusiveAndRecoversAbandonment)$'
if ((Get-WindowsNativeRequiredTestExpression) -cne $expectedExpression) {
    throw 'required native test expression drifted from the release contract'
}

$ordinarySID = 'S-1-5-21-100-200-300-1001'
Assert-WindowsNativeStandardUserIdentity `
    -ActualUserSID $ordinarySID `
    -ExpectedUserSID $ordinarySID `
    -GroupSIDs @('S-1-1-0', 'S-1-5-32-545') `
    -IsAdministrator $false
Assert-Throws 'mismatched worker SID' {
    Assert-WindowsNativeStandardUserIdentity `
        -ActualUserSID $ordinarySID `
        -ExpectedUserSID 'S-1-5-21-100-200-300-1002' `
        -GroupSIDs @('S-1-5-32-545') `
        -IsAdministrator $false
}
Assert-Throws 'administrator group token' {
    Assert-WindowsNativeStandardUserIdentity `
        -ActualUserSID $ordinarySID `
        -ExpectedUserSID $ordinarySID `
        -GroupSIDs @('S-1-5-32-544', 'S-1-5-32-545') `
        -IsAdministrator $false
}
Assert-Throws 'administrator role token' {
    Assert-WindowsNativeStandardUserIdentity `
        -ActualUserSID $ordinarySID `
        -ExpectedUserSID $ordinarySID `
        -GroupSIDs @('S-1-5-32-545') `
        -IsAdministrator $true
}
Assert-Throws 'service identity token' {
    Assert-WindowsNativeStandardUserIdentity `
        -ActualUserSID 'S-1-5-18' `
        -ExpectedUserSID 'S-1-5-18' `
        -GroupSIDs @('S-1-5-32-545') `
        -IsAdministrator $false
}
Assert-Throws 'token without Users group' {
    Assert-WindowsNativeStandardUserIdentity `
        -ActualUserSID $ordinarySID `
        -ExpectedUserSID $ordinarySID `
        -GroupSIDs @('S-1-1-0') `
        -IsAdministrator $false
}

$writableProbeRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-native-readonly-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
New-Item -ItemType Directory -Path $writableProbeRoot | Out-Null
try {
    Assert-Throws 'writable artifact root' {
        Assert-WindowsNativeReadOnlyDirectory -Path $writableProbeRoot -Label 'contract artifact'
    }
    if (@(Get-ChildItem -LiteralPath $writableProbeRoot -Force).Count -ne 0) {
        throw 'write-denial probe did not remove its unexpected artifact'
    }
} finally {
    Remove-Item -LiteralPath $writableProbeRoot -Recurse -Force
}

$passingEvents = @(
    [pscustomobject]@{ Action = 'run'; Test = $requiredTests[0]; Package = 'example/osfs' },
    [pscustomobject]@{ Action = 'pass'; Test = $requiredTests[0]; Package = 'example/osfs' },
    [pscustomobject]@{ Action = 'run'; Test = $requiredTests[1]; Package = 'example/osfs' },
    [pscustomobject]@{ Action = 'pass'; Test = $requiredTests[1]; Package = 'example/osfs' },
    [pscustomobject]@{ Action = 'run'; Test = $requiredTests[2]; Package = 'example/osfs' },
    [pscustomobject]@{ Action = 'pass'; Test = $requiredTests[2]; Package = 'example/osfs' },
    [pscustomobject]@{ Action = 'pass'; Package = 'example/osfs' }
)
$jsonEvents = @(ConvertFrom-WindowsNativeTestJSONLines -Lines @(
    '{"Action":"run","Test":"TestWindowsNTFSNativeCertification"}',
    '{"Action":"pass","Test":"TestWindowsNTFSNativeCertification"}',
    '{"Action":"run","Test":"TestWindowsNTFSProcessRestartRecovery"}',
    '{"Action":"pass","Test":"TestWindowsNTFSProcessRestartRecovery"}',
    '{"Action":"run","Test":"TestWindowsNTFSProbeMutexIsProcessExclusiveAndRecoversAbandonment"}',
    '{"Action":"pass","Test":"TestWindowsNTFSProbeMutexIsProcessExclusiveAndRecoversAbandonment"}'
))
Assert-WindowsNativeTestEvents -ExitCode 0 -Events $jsonEvents -RequiredTests $requiredTests
Assert-Throws 'invalid JSON event' {
    ConvertFrom-WindowsNativeTestJSONLines -Lines @('{not-json}')
}
Assert-Throws 'empty JSON line' {
    ConvertFrom-WindowsNativeTestJSONLines -Lines @('')
}
Assert-WindowsNativeTestEvents -ExitCode 0 -Events $passingEvents -RequiredTests $requiredTests
Assert-Throws 'nonzero go test exit' {
    Assert-WindowsNativeTestEvents -ExitCode 1 -Events $passingEvents -RequiredTests $requiredTests
}
Assert-Throws 'subtest skip' {
    $events = @($passingEvents) + [pscustomobject]@{
        Action = 'skip'
        Test = "$($requiredTests[0])/unsupported"
        Package = 'example/osfs'
    }
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'package skip' {
    $events = @($passingEvents) + [pscustomobject]@{
        Action = 'skip'
        Package = 'example/osfs'
    }
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'missing mutex evidence' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        $testName -cne $requiredTests[2]
    })
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'missing mutex top-level run' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        -not ($_.Action -ceq 'run' -and $testName -ceq $requiredTests[2])
    })
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'missing mutex top-level pass' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        -not ($_.Action -ceq 'pass' -and $testName -ceq $requiredTests[2])
    })
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'skipped mutex test' {
    $events = @($passingEvents) + [pscustomobject]@{
        Action = 'skip'
        Test = $requiredTests[2]
        Package = 'example/osfs'
    }
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'failed mutex test' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        -not ($_.Action -ceq 'pass' -and $testName -ceq $requiredTests[2])
    }) + [pscustomobject]@{
        Action = 'fail'
        Test = $requiredTests[2]
        Package = 'example/osfs'
    }
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'missing top-level pass' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        -not ($_.Action -ceq 'pass' -and $testName -ceq $requiredTests[1])
    })
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'empty JSON event stream' {
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events @() -RequiredTests $requiredTests
}

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$releaseWorkflowPath = Join-Path $repositoryRoot '.github\workflows\core-release.yml'
$releaseWorkflow = [IO.File]::ReadAllText($releaseWorkflowPath)
$actionLines = [regex]::Matches($releaseWorkflow, '(?m)^[ \t]*-[ \t]+uses:[^\r\n]+')
foreach ($actionLine in $actionLines) {
    if ($actionLine.Value -cnotmatch '^[ \t]*-[ \t]+uses:[ \t]+[^\s#]+@[0-9a-f]{40}[ \t]+#[ \t]+v[0-9]+(?:\.[0-9]+){0,2}[ \t]*$') {
        throw "core release workflow action is not pinned to a full SHA: $($actionLine.Value)"
    }
}
if ([regex]::Matches($releaseWorkflow, 'go-version-file:[ \t]+core/go\.mod').Count -ne 2 -or
    [regex]::Matches($releaseWorkflow, 'cache:[ \t]+false').Count -ne 2 -or
    $releaseWorkflow.Contains('go-version-file: go.work', [StringComparison]::Ordinal)) {
    throw 'core release jobs do not derive an uncached toolchain from core/go.mod'
}
$linuxReleaseJob = [regex]::Match(
    $releaseWorkflow,
    '(?ms)^  linux-ext4:\r?\n.*?(?=^  windows-ntfs:\r?$)'
).Value
$windowsReleaseJob = [regex]::Match(
    $releaseWorkflow,
    '(?ms)^  windows-ntfs:\r?\n.*\z'
).Value
if ([string]::IsNullOrWhiteSpace($linuxReleaseJob) -or
    -not $linuxReleaseJob.Contains('timeout-minutes: 40', [StringComparison]::Ordinal) -or
    [string]::IsNullOrWhiteSpace($windowsReleaseJob) -or
    -not $windowsReleaseJob.Contains('timeout-minutes: 90', [StringComparison]::Ordinal)) {
    throw 'core release workflow job timeouts do not preserve the platform-specific evidence budgets'
}
$ciWorkflowPath = Join-Path $repositoryRoot '.github\workflows\ci.yml'
$ciWorkflow = [IO.File]::ReadAllText($ciWorkflowPath)
$ordinaryReleaseJob = [regex]::Match(
    $ciWorkflow,
    '(?ms)^  core-release:\r?\n.*?(?=^  gowork-off-root:\r?$)'
).Value
if ([string]::IsNullOrWhiteSpace($ordinaryReleaseJob) -or
    -not $ordinaryReleaseJob.Contains('go-version-file: core/go.mod', [StringComparison]::Ordinal) -or
    -not $ordinaryReleaseJob.Contains('cache: false', [StringComparison]::Ordinal) -or
    $ordinaryReleaseJob.Contains('go-version-file: go.work', [StringComparison]::Ordinal)) {
    throw 'ordinary CI core-release job does not use the uncached core toolchain'
}
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'go run ./scripts/ci/_corevulnerability'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected '-module $artifactRoot'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected "-cache (Join-Path `$temporaryRoot 'vulnerability-cache')"
Assert-FileContains `
    -Path (Join-Path $repositoryRoot 'scripts\ci\_corevulnerability\main.go') `
    -Expected 'golang.org/x/vuln/cmd/govulncheck@v1.6.0'
Assert-FileContains `
    -Path $releaseWorkflowPath `
    -Expected 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7'
Assert-FileContains `
    -Path $releaseWorkflowPath `
    -Expected 'actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6'
Assert-FileContains `
    -Path $releaseWorkflowPath `
    -Expected 'go-version-file: core/go.mod'
Assert-FileContains `
    -Path $releaseWorkflowPath `
    -Expected 'run: bash scripts/ci/core-release.sh "$CORE_RELEASE_VERSION" "$CORE_RELEASE_COMMIT_SHA" linux-ext4'
Assert-FileContains `
    -Path $releaseWorkflowPath `
    -Expected 'run: ./scripts/ci/core-release.ps1 -Version $env:CORE_RELEASE_VERSION -CommitSHA $env:CORE_RELEASE_COMMIT_SHA -NativeProfile windows-ntfs'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'Enter-CoreReleaseGoEnvironment -ReleaseRoot $temporaryRoot'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected "(Join-Path `$PSScriptRoot 'core-release-checkout.tests.ps1')"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'New-ExactCoreReleaseCheckout'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'Set-Location $releaseRepository'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'Assert-ExactCoreReleaseFileProjection'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected '-commit $CommitSHA'
Assert-FileContains `
    -Path (Join-Path $repositoryRoot 'scripts\ci\_coremodulezip\main.go') `
    -Expected '"ls-tree", "-r", "-z", "--full-tree", commitSHA'
Assert-FileContains `
    -Path (Join-Path $repositoryRoot 'scripts\ci\_coremodulezip\main.go') `
    -Expected '"cat-file", "blob", objectID'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release-checkout.psm1') `
    -Expected "StartsWith('GIT_', [StringComparison]::OrdinalIgnoreCase)"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release-checkout.psm1') `
    -Expected "'hash-object', '--no-filters'"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected "`$coverageTool = 'github.com/vladopajic/go-test-coverage/v2@v2.18.8'"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected "`$coreSuiteTestTimeout = '30m'"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected "`$windowsNativeWorkerTimeoutMinutes = 35"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected '[TimeSpan]::FromMinutes('
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'go test -count=1 "-timeout=$coreSuiteTestTimeout" ./...'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'go test -race -count=1 "-timeout=$coreSuiteTestTimeout" ./...'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'go test -count=1 "-timeout=$coreSuiteTestTimeout" ./... -covermode=atomic'
Assert-FileDoesNotContain `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Forbidden 'GO_TEST_COVERAGE'

$nativeWorkerPath = Join-Path $PSScriptRoot 'core-release-windows-native-worker.ps1'
foreach ($requiredWorkerText in @(
    "`$coreSuiteTestTimeout = '30m'",
    '"-timeout=$coreSuiteTestTimeout"',
    "`$env:GOPROXY = 'https://proxy.golang.org'",
    "`$env:GOSUMDB = 'sum.golang.org'",
    "`$env:GOPRIVATE = ''",
    "`$env:GONOSUMDB = ''",
    "`$env:GONOPROXY = ''",
    "`$env:GOINSECURE = ''",
    "`$env:GOTELEMETRY = 'off'",
    "@('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOEXPERIMENT')"
)) {
    Assert-FileContains -Path $nativeWorkerPath -Expected $requiredWorkerText
}

Write-Output 'core-release Windows native helper tests: PASS'
