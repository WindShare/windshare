Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$windowsNativeModule = Import-Module `
    (Join-Path $PSScriptRoot 'core-release-windows-native.psm1') `
    -Force `
    -PassThru

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

function Assert-ThrowsContaining(
    [string]$Label,
    [string]$ExpectedMessage,
    [scriptblock]$Body
) {
    try {
        $null = & $Body
    } catch {
        if (-not $_.Exception.Message.Contains($ExpectedMessage, [StringComparison]::Ordinal)) {
            throw "$Label failed for the wrong reason: $($_.Exception.Message)"
        }
        return
    }
    throw "$Label did not fail closed"
}

function ConvertTo-TestWindowsNativeArgument([string]$Value) {
    return & $windowsNativeModule {
        param([string]$ArgumentValue)
        ConvertTo-WindowsNativeArgument $ArgumentValue
    } $Value
}

function New-TestWindowsNativeCoordinatorReleaseRoot(
    [string]$BasePath,
    [string]$LeafName
) {
    return & $windowsNativeModule {
        param([string]$RootBase, [string]$RootLeaf)
        New-WindowsNativeCoordinatorReleaseRootAt `
            -BasePath $RootBase `
            -LeafName $RootLeaf
    } $BasePath $LeafName
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

$canonicalCommonData = Get-WindowsNativeCommonApplicationDataRoot
$expectedCommonData = [IO.Path]::GetFullPath(
    (Resolve-Path -LiteralPath ([Environment]::GetFolderPath(
        [Environment+SpecialFolder]::CommonApplicationData
    ))).Path
)
if (-not [string]::Equals(
    $canonicalCommonData,
    $expectedCommonData,
    [StringComparison]::OrdinalIgnoreCase
)) {
    throw 'native release root base is not canonical CommonApplicationData'
}

$currentGoCommand = Get-Command go -CommandType Application -ErrorAction Stop
$currentGoExecutable = [IO.Path]::GetFullPath(
    (Resolve-Path -LiteralPath $currentGoCommand.Source).Path
)
$originalGoToolchain = [Environment]::GetEnvironmentVariable('GOTOOLCHAIN', 'Process')
try {
    $env:GOTOOLCHAIN = 'local'
    $currentToolchain = Get-WindowsNativeCoordinatorGoToolchain `
        -GoExecutable $currentGoExecutable
} finally {
    if ($null -eq $originalGoToolchain) {
        Remove-Item Env:GOTOOLCHAIN -ErrorAction SilentlyContinue
    } else {
        $env:GOTOOLCHAIN = $originalGoToolchain
    }
}
if (-not [string]::Equals(
    $currentToolchain.GoExecutable,
    $currentGoExecutable,
    [StringComparison]::OrdinalIgnoreCase
) -or -not [string]::Equals(
    (Join-Path $currentToolchain.GoRoot 'bin\go.exe'),
    $currentToolchain.GoExecutable,
    [StringComparison]::OrdinalIgnoreCase
)) {
    throw 'coordinator Go toolchain discovery did not preserve the exact go env GOROOT binding'
}

$toolchainCopyRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-native-toolchain-copy-{0}' -f [Guid]::NewGuid().ToString('N')
)
$syntheticGoRoot = Join-Path $toolchainCopyRoot 'source-go'
$syntheticGoBin = Join-Path $syntheticGoRoot 'bin'
$syntheticGoNested = Join-Path $syntheticGoRoot 'pkg\tool\windows_amd64'
$stagedSyntheticGoRoot = Join-Path $toolchainCopyRoot 'staged-go'
New-Item -ItemType Directory -Path $syntheticGoBin | Out-Null
New-Item -ItemType Directory -Path $syntheticGoNested | Out-Null
$syntheticGoExecutable = Join-Path $syntheticGoBin 'go.exe'
[IO.File]::WriteAllText($syntheticGoExecutable, 'synthetic-go')
[IO.File]::WriteAllText(
    (Join-Path $syntheticGoNested 'compile.exe'),
    'synthetic-compile'
)
try {
    $stagedSyntheticToolchain = Copy-WindowsNativeGoToolchain `
        -Toolchain ([pscustomobject]@{
            GoRoot = $syntheticGoRoot
            GoExecutable = $syntheticGoExecutable
        }) `
        -DestinationRoot $stagedSyntheticGoRoot
    if (-not [string]::Equals(
        $stagedSyntheticToolchain.GoRoot,
        [IO.Path]::GetFullPath($stagedSyntheticGoRoot),
        [StringComparison]::OrdinalIgnoreCase
    ) -or [IO.File]::ReadAllText(
        (Join-Path $stagedSyntheticGoRoot 'pkg\tool\windows_amd64\compile.exe')
    ) -cne 'synthetic-compile') {
        throw 'staged Go toolchain copy did not preserve its root layout and files'
    }
    Assert-WindowsNativeTreeHasNoReparsePoints `
        -RootPath $stagedSyntheticGoRoot `
        -Label 'synthetic staged GOROOT'
    $reparseTarget = Join-Path $toolchainCopyRoot 'reparse-target'
    $reparsePath = Join-Path $stagedSyntheticGoRoot 'reparse-probe'
    New-Item -ItemType Directory -Path $reparseTarget | Out-Null
    New-Item -ItemType Junction -Path $reparsePath -Target $reparseTarget | Out-Null
    try {
        Assert-ThrowsContaining `
            'reparse point in staged GOROOT' `
            'contains a reparse point' {
                Assert-WindowsNativeTreeHasNoReparsePoints `
                    -RootPath $stagedSyntheticGoRoot `
                    -Label 'synthetic staged GOROOT'
            }
    } finally {
        if (Test-Path -LiteralPath $reparsePath) {
            [IO.Directory]::Delete($reparsePath, $false)
        }
    }
    Assert-Throws 'pre-existing staged GOROOT' {
        Copy-WindowsNativeGoToolchain `
            -Toolchain ([pscustomobject]@{
                GoRoot = $syntheticGoRoot
                GoExecutable = $syntheticGoExecutable
            }) `
            -DestinationRoot $stagedSyntheticGoRoot
    }
} finally {
    if (Test-Path -LiteralPath $toolchainCopyRoot) {
        Remove-Item -LiteralPath $toolchainCopyRoot -Recurse -Force
    }
}

$rootContractBase = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-native-root-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
New-Item -ItemType Directory -Path $rootContractBase | Out-Null
$ownedRoot = $null
try {
    $ownedLeaf = 'windshare-core-release-{0}' -f [Guid]::NewGuid().ToString('N')
    $ownedRoot = New-TestWindowsNativeCoordinatorReleaseRoot `
        -BasePath $rootContractBase `
        -LeafName $ownedLeaf
    Assert-WindowsNativeCoordinatorReleaseRoot -Ownership $ownedRoot -RequireEmpty
    if ($ownedRoot.LeafName -cne $ownedLeaf -or
        -not [string]::Equals(
            ([IO.DirectoryInfo]::new($ownedRoot.RootPath)).Parent.FullName,
            [IO.Path]::GetFullPath($rootContractBase),
            [StringComparison]::OrdinalIgnoreCase
        )) {
        throw 'protected native root is not the exact requested direct child'
    }
    Assert-ThrowsContaining `
        'atomic protected-root collision' `
        'native release root collision' {
            New-TestWindowsNativeCoordinatorReleaseRoot `
                -BasePath $rootContractBase `
                -LeafName $ownedLeaf
        }
    if (-not (Test-Path -LiteralPath $ownedRoot.RootPath -PathType Container)) {
        throw 'collision handling removed the original protected native root'
    }

    $workerAccessSID = 'S-1-5-32-545'
    $ownedRoot.WorkerSID = $workerAccessSID
    $icacls = Join-Path $env:SystemRoot 'System32\icacls.exe'
    & $icacls $ownedRoot.RootPath '/grant:r' "*${workerAccessSID}:(OI)(CI)RX" '/T' '/Q' | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "worker root-access contract setup failed with code $LASTEXITCODE"
    }
    Assert-WindowsNativeCoordinatorReleaseRoot -Ownership $ownedRoot

    [IO.File]::WriteAllText((Join-Path $ownedRoot.RootPath 'cleanup-evidence'), 'owned')
    Remove-WindowsNativeCoordinatorReleaseRoot -Ownership $ownedRoot
    if (Test-Path -LiteralPath $ownedRoot.RootPath) {
        throw 'validated native release root cleanup left its non-empty root behind'
    }
    $ownedRoot = $null

    Assert-ThrowsContaining `
        'malformed owned root leaf' `
        'leaf name does not satisfy' {
            New-TestWindowsNativeCoordinatorReleaseRoot `
                -BasePath $rootContractBase `
                -LeafName 'not-an-owned-release-root'
        }

    $nestedParent = Join-Path $rootContractBase 'nested'
    New-Item -ItemType Directory -Path $nestedParent | Out-Null
    $nestedLeaf = 'windshare-core-release-{0}' -f [Guid]::NewGuid().ToString('N')
    $nestedPath = Join-Path $nestedParent $nestedLeaf
    New-Item -ItemType Directory -Path $nestedPath | Out-Null
    $forgedOwnership = [pscustomobject]@{
        BasePath = [IO.Path]::GetFullPath($rootContractBase)
        RootPath = [IO.Path]::GetFullPath($nestedPath)
        LeafName = $nestedLeaf
        CoordinatorSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        WorkerSID = ''
    }
    Assert-ThrowsContaining `
        'nested cleanup target' `
        'not a direct child' {
            Remove-WindowsNativeCoordinatorReleaseRoot -Ownership $forgedOwnership
        }
    if (-not (Test-Path -LiteralPath $nestedPath -PathType Container)) {
        throw 'failed-closed cleanup removed a forged nested target'
    }
} finally {
    if ($null -ne $ownedRoot -and (Test-Path -LiteralPath $ownedRoot.RootPath)) {
        Remove-Item -LiteralPath $ownedRoot.RootPath -Recurse -Force
    }
    if (Test-Path -LiteralPath $rootContractBase) {
        Remove-Item -LiteralPath $rootContractBase -Recurse -Force
    }
}

$credentialCommandLineMaximumCharacters = 1023
$githubTemporaryRoot = 'C:\ProgramData\windshare-core-release-{0}' -f ('a' * 32)
$githubWorkerRoot = [IO.Path]::Combine($githubTemporaryRoot, 'windows-native-worker')
$githubWorkerScript = [IO.Path]::Combine(
    $githubWorkerRoot,
    'core-release-windows-native-worker.ps1'
)
$githubArtifactRoot = [IO.Path]::Combine($githubTemporaryRoot, 'extracted-core')
$githubPowerShellExecutable = 'C:\Program Files\PowerShell\7\pwsh.exe'
$githubGoExecutable = [IO.Path]::Combine(
    $githubTemporaryRoot,
    'go-toolchain',
    'bin',
    'go.exe'
)
$githubWorkerSID = 'S-1-5-21-1234567890-1234567890-1234567890-1001'
$githubArgumentLine = New-WindowsNativeWorkerArgumentLine `
    -PowerShellExecutable $githubPowerShellExecutable `
    -WorkerScript $githubWorkerScript `
    -ArtifactRoot $githubArtifactRoot `
    -WorkRoot $githubWorkerRoot `
    -GoExecutable $githubGoExecutable `
    -ExpectedUserSID $githubWorkerSID
$expectedGithubArgumentLine = @(
    '-NoLogo',
    '-NoProfile',
    '-NonInteractive',
    '-ExecutionPolicy',
    'Bypass',
    '-File',
    ('"{0}"' -f $githubWorkerScript),
    '-ArtifactRoot',
    ('"{0}"' -f $githubArtifactRoot),
    '-WorkRoot',
    ('"{0}"' -f $githubWorkerRoot),
    '-GoExecutable',
    ('"{0}"' -f $githubGoExecutable),
    '-ExpectedUserSID',
    ('"{0}"' -f $githubWorkerSID)
) -join ' '
if ($githubArgumentLine -cne $expectedGithubArgumentLine) {
    throw 'standard-user worker argument line lost its direct -File contract or exact quoting'
}
$githubFullCommandLine = '"{0}" {1}' -f $githubPowerShellExecutable, $githubArgumentLine
if ($githubFullCommandLine.Length -gt $credentialCommandLineMaximumCharacters) {
    throw "representative GitHub worker command line is unexpectedly $($githubFullCommandLine.Length) characters"
}
if ((ConvertTo-TestWindowsNativeArgument '') -cne '""' -or
    (ConvertTo-TestWindowsNativeArgument 'alpha"beta') -cne '"alpha\"beta"' -or
    (ConvertTo-TestWindowsNativeArgument 'C:\') -cne '"C:\\"') {
    throw 'Windows native argument quoting does not preserve empty, quoted, or trailing-backslash values'
}
Assert-ThrowsContaining `
    'overlong credential command line' `
    'CreateProcessWithLogonW permits at most 1023' {
        New-WindowsNativeWorkerArgumentLine `
            -PowerShellExecutable $githubPowerShellExecutable `
            -WorkerScript $githubWorkerScript `
            -ArtifactRoot ('D:\' + ('a' * 900)) `
            -WorkRoot $githubWorkerRoot `
            -GoExecutable $githubGoExecutable `
            -ExpectedUserSID $githubWorkerSID
    }
Assert-ThrowsContaining `
    'control character in worker launch value' `
    'contains a control character' {
        New-WindowsNativeWorkerArgumentLine `
            -PowerShellExecutable $githubPowerShellExecutable `
            -WorkerScript $githubWorkerScript `
            -ArtifactRoot ($githubArtifactRoot + "`nchild") `
            -WorkRoot $githubWorkerRoot `
            -GoExecutable $githubGoExecutable `
            -ExpectedUserSID $githubWorkerSID
    }
Assert-ThrowsContaining `
    'quote in worker launch value' `
    'contains a double quote' {
        New-WindowsNativeWorkerArgumentLine `
            -PowerShellExecutable $githubPowerShellExecutable `
            -WorkerScript ($githubWorkerScript + '"') `
            -ArtifactRoot $githubArtifactRoot `
            -WorkRoot $githubWorkerRoot `
            -GoExecutable $githubGoExecutable `
            -ExpectedUserSID $githubWorkerSID
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
    -not $linuxReleaseJob.Contains('timeout-minutes: 60', [StringComparison]::Ordinal) -or
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
    -not $ordinaryReleaseJob.Contains('timeout-minutes: 60', [StringComparison]::Ordinal) -or
    -not $ordinaryReleaseJob.Contains('go-version-file: core/go.mod', [StringComparison]::Ordinal) -or
    -not $ordinaryReleaseJob.Contains('cache: false', [StringComparison]::Ordinal) -or
    $ordinaryReleaseJob.Contains('go-version-file: go.work', [StringComparison]::Ordinal)) {
    throw 'ordinary CI core-release job lacks the fixed timeout or uncached core toolchain'
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
    -Expected 'New-WindowsNativeWorkerArgumentLine'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected '-ArgumentList $workerArgumentLine'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected '-LoadUserProfile'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected '-UseNewEnvironment'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'New-WindowsNativeCoordinatorReleaseRoot'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'Wait-EphemeralWindowsUserProfileUnload'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected "-Permission M"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'Deny-EphemeralWindowsUserTreeMutation'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected 'Get-WindowsNativeCoordinatorGoToolchain'
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected "-DestinationRoot (Join-Path `$temporaryRoot 'go-toolchain')"
Assert-FileContains `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Expected '-GoExecutable $stagedToolchain.GoExecutable'
Assert-FileDoesNotContain `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Forbidden '-EncodedCommand'
Assert-FileDoesNotContain `
    -Path (Join-Path $PSScriptRoot 'core-release.ps1') `
    -Forbidden 'ConvertTo-SingleQuotedPowerShellLiteral'
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

$releaseScriptPath = Join-Path $PSScriptRoot 'core-release.ps1'
$releaseScript = [IO.File]::ReadAllText($releaseScriptPath)
$nativeGateIndex = $releaseScript.LastIndexOf(
    'Invoke-RequiredWindowsNativeTestsAsStandardUser',
    [StringComparison]::Ordinal
)
$vulnerabilityIndex = $releaseScript.IndexOf(
    "Invoke-Step 'version-pinned govulncheck (extracted core)'",
    [StringComparison]::Ordinal
)
$ordinaryTestIndex = $releaseScript.IndexOf(
    "Invoke-Step 'GOWORK=off go test ./... (extracted core)'",
    [StringComparison]::Ordinal
)
$raceTestIndex = $releaseScript.IndexOf(
    "Invoke-Step 'GOWORK=off go test -race ./... (extracted core)'",
    [StringComparison]::Ordinal
)
$coverageTestIndex = $releaseScript.IndexOf(
    "Invoke-Step 'GOWORK=off go test with coverage (extracted core)'",
    [StringComparison]::Ordinal
)
if ($vulnerabilityIndex -lt 0 -or
    $nativeGateIndex -le $vulnerabilityIndex -or
    $ordinaryTestIndex -le $nativeGateIndex -or
    $raceTestIndex -le $nativeGateIndex -or
    $coverageTestIndex -le $nativeGateIndex) {
    throw 'Windows native gate is not fail-fast after artifact build/vulnerability verification and before ordinary test evidence'
}
$profileWaitIndex = $releaseScript.IndexOf(
    'Wait-EphemeralWindowsUserProfileUnload -UserSID $UserSID',
    [StringComparison]::Ordinal
)
$userRemovalIndex = $releaseScript.IndexOf(
    'Remove-LocalUser -Name $UserName',
    [StringComparison]::Ordinal
)
if ($profileWaitIndex -lt 0 -or
    $userRemovalIndex -lt 0 -or
    $profileWaitIndex -ge $userRemovalIndex) {
    throw 'bounded profile-unload wait does not precede ephemeral user deletion'
}
$toolchainCopyIndex = $releaseScript.IndexOf(
    'Copy-WindowsNativeGoToolchain',
    [StringComparison]::Ordinal
)
$workerAccessGrantIndex = $releaseScript.IndexOf(
    'Grant-EphemeralWindowsUserAccess -Path $temporaryRoot',
    [StringComparison]::Ordinal
)
if ($toolchainCopyIndex -lt 0 -or
    $workerAccessGrantIndex -lt 0 -or
    $toolchainCopyIndex -ge $workerAccessGrantIndex) {
    throw 'coordinator GOROOT is not staged before the worker receives release-root access'
}
$immutableTreeDenials = [regex]::Matches(
    $releaseScript,
    '(?m)^\s*Deny-EphemeralWindowsUserTreeMutation `\r?$'
)
if ($immutableTreeDenials.Count -ne 2 -or
    -not $releaseScript.Contains("-Path `$stagedToolchain.GoRoot", [StringComparison]::Ordinal)) {
    throw 'artifact and staged GOROOT do not both receive direct recursive mutation denials'
}

$nativeModulePath = Join-Path $PSScriptRoot 'core-release-windows-native.psm1'
foreach ($requiredModuleText in @(
    'CreateDirectoryW',
    'CreateExclusive',
    'SetAccessRuleProtection($true, $false)',
    '[Environment+SpecialFolder]::CommonApplicationData',
    'CoordinatorReleaseRootLeafPattern',
    'Assert-WindowsNativeCoordinatorReleaseRoot -Ownership $Ownership',
    '& $resolvedGoExecutable env GOROOT',
    'Copying into the already protected release root',
    'Copy-Item',
    'Assert-WindowsNativeTreeHasNoReparsePoints',
    'staged Go executable length differs from the coordinator executable'
)) {
    Assert-FileContains -Path $nativeModulePath -Expected $requiredModuleText
}
$nativeModuleSource = [IO.File]::ReadAllText($nativeModulePath)
$interopInitializerIndex = $nativeModuleSource.IndexOf(
    'function Initialize-WindowsNativeDirectoryInterop',
    [StringComparison]::Ordinal
)
$addTypeIndex = $nativeModuleSource.IndexOf(
    'Add-Type -TypeDefinition',
    [StringComparison]::Ordinal
)
$nextFunctionIndex = $nativeModuleSource.IndexOf(
    'function Resolve-WindowsNativeLocalFixedNTFSDirectory',
    [StringComparison]::Ordinal
)
$interopCallIndex = $nativeModuleSource.LastIndexOf(
    'Initialize-WindowsNativeDirectoryInterop',
    [StringComparison]::Ordinal
)
if ($interopInitializerIndex -lt 0 -or
    $addTypeIndex -le $interopInitializerIndex -or
    $nextFunctionIndex -le $addTypeIndex -or
    $interopCallIndex -le $nextFunctionIndex) {
    throw 'CreateDirectoryW interop is not lazy and coordinator-root scoped'
}

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
    '[Environment+SpecialFolder]::UserProfile',
    '$env:USERPROFILE = $resolvedUserProfile',
    '$env:HOME = $resolvedUserProfile',
    '$env:HOMEDRIVE = $homeDrive',
    '$env:HOMEPATH = $homePath',
    '$env:GOTMPDIR = $goTempRoot',
    '$env:GOROOT = $resolvedGoRoot',
    '-- standard-user profile and identity environment verified',
    '-- standard-user NTFS roots, artifact immutability, and staged GOROOT immutability verified',
    "-Label 'staged GOROOT'",
    'Assert-WindowsNativeTreeHasNoReparsePoints',
    '& $resolvedGoExecutable env GOROOT',
    '-- standard-user Go toolchain verified:',
    "@('GOOS', 'GOARCH', 'CGO_ENABLED', 'GOEXPERIMENT')"
)) {
    Assert-FileContains -Path $nativeWorkerPath -Expected $requiredWorkerText
}

Write-Output 'core-release Windows native helper tests: PASS'
