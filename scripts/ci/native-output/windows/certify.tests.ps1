Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$windowsNativeModule = Import-Module `
    (Join-Path $PSScriptRoot 'certify.psm1') `
    -Force `
    -PassThru
$coordinatorGoApplication = @(Get-Command go -CommandType Application -ErrorAction Stop)[0]
$coordinatorGoExecutable = [IO.Path]::GetFullPath($coordinatorGoApplication.Path)

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

function Remove-TestWindowsNativeMutationDeny(
    [string[]]$Paths,
    [string]$UserSID
) {
    & $windowsNativeModule {
        param([string[]]$EntryPaths, [string]$DeniedSID)

        foreach ($entryPath in $EntryPaths) {
            if (-not (Test-Path -LiteralPath $entryPath)) {
                continue
            }
            $entry = Get-Item -LiteralPath $entryPath -Force
            $security = Get-WindowsNativeFileSystemAccessControl -Entry $entry
            $denyRules = @($security.GetAccessRules(
                $true,
                $false,
                [Security.Principal.SecurityIdentifier]
            ) | Where-Object {
                $_.IdentityReference.Value -ceq $DeniedSID -and
                    $_.AccessControlType -eq [Security.AccessControl.AccessControlType]::Deny
            })
            foreach ($denyRule in $denyRules) {
                [void]$security.RemoveAccessRuleSpecific($denyRule)
            }
            Set-WindowsNativeFileSystemAccessControl `
                -Entry $entry `
                -Security $security
        }
    } $Paths $UserSID
}

function Assert-FileContains([string]$Path, [string]$Expected) {
    $content = [IO.File]::ReadAllText($Path)
    if (-not $content.Contains($Expected, [StringComparison]::Ordinal)) {
        throw "$Path is missing required release contract text: $Expected"
    }
}

$requiredSelectors = @(Get-WindowsNativeRequiredTestSelectors)
if ($requiredSelectors.Count -ne 2) {
    throw 'required native test selectors drifted from the release contract'
}
$coreSelector = @($requiredSelectors | Where-Object {
    $_.PackageArgument -ceq './osfs'
})
$portableSelector = @($requiredSelectors | Where-Object {
    $_.PackageArgument -ceq './osfs/internal/outputportable'
})
if ($coreSelector.Count -ne 1 -or
    $portableSelector.Count -ne 1 -or
    $coreSelector[0].BinaryName -cne 'osfs.test.exe' -or
    $coreSelector[0].ImportPath -cne 'github.com/windshare/windshare/osfs' -or
    $portableSelector[0].BinaryName -cne 'outputportable.test.exe' -or
    $portableSelector[0].ImportPath -cne 'github.com/windshare/windshare/osfs/internal/outputportable' -or
    (Get-WindowsNativeRequiredTestExpression -TestNames $coreSelector[0].TestNames) -cne
        '^(TestNativeOutputDestinationCapabilities|TestNativeOutputProcessRestartKeepsRanges|TestNativeOutputRetainedCandidateListAndReopen)$' -or
    (Get-WindowsNativeRequiredTestExpression -TestNames $portableSelector[0].TestNames) -cne
        '^(TestTaskLockIsProcessExclusiveAndRecoversAfterExit|TestOrdinaryFilesPublishWithoutLinksAndNeverOverwrite|TestWindowsModifiedTimePreservesNativePrecisionAndHandleIdentity)$') {
    throw 'required native tests are not bound to their owning package selectors and precompiled binaries'
}
$requiredTests = @($requiredSelectors | ForEach-Object { $_.TestNames })
$lockTestName = 'TestTaskLockIsProcessExclusiveAndRecoversAfterExit'

$profilePolicySID = 'S-1-5-21-4000000001-4000000002-4000000003-4000000004'
$profileDeleteCodes = [Collections.Generic.Queue[int]]::new()
$profileDeleteCodes.Enqueue(32)
$profileDeleteCodes.Enqueue(0)
$profileDeleteDelays = [Collections.Generic.List[int]]::new()
Remove-WindowsNativeEphemeralUserProfile `
    -UserSID $profilePolicySID `
    -TimeoutMilliseconds 1000 `
    -PollMilliseconds 7 `
    -DeleteAttempt { param([string]$SID, [string]$Path) $profileDeleteCodes.Dequeue() } `
    -Delay { param([int]$Milliseconds) $profileDeleteDelays.Add($Milliseconds) }
if ($profileDeleteCodes.Count -ne 0 -or
    $profileDeleteDelays.Count -ne 1 -or
    $profileDeleteDelays[0] -ne 7) {
    throw 'profile deletion did not retry one transient native error exactly once'
}
Remove-WindowsNativeEphemeralUserProfile `
    -UserSID $profilePolicySID `
    -TimeoutMilliseconds 0 `
    -PollMilliseconds 1 `
    -DeleteAttempt { param([string]$SID, [string]$Path) 2 } `
    -Delay { throw 'absent profile must not delay' }
Assert-ThrowsContaining 'non-retryable profile deletion error' 'Win32 error 5' {
    Remove-WindowsNativeEphemeralUserProfile `
        -UserSID $profilePolicySID `
        -TimeoutMilliseconds 1000 `
        -PollMilliseconds 1 `
        -DeleteAttempt { param([string]$SID, [string]$Path) 5 } `
        -Delay { throw 'non-retryable profile error must not delay' }
}
Assert-ThrowsContaining 'busy profile deletion timeout' 'remained busy' {
    Remove-WindowsNativeEphemeralUserProfile `
        -UserSID $profilePolicySID `
        -TimeoutMilliseconds 0 `
        -PollMilliseconds 1 `
        -DeleteAttempt { param([string]$SID, [string]$Path) 170 } `
        -Delay { throw 'expired profile deletion must not delay' }
}
Assert-Throws 'invalid profile deletion SID' {
    Remove-WindowsNativeEphemeralUserProfile `
        -UserSID '..\not-a-sid' `
        -TimeoutMilliseconds 0 `
        -PollMilliseconds 1 `
        -DeleteAttempt { param([string]$SID, [string]$Path) 0 }
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

$reparseTreeRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-native-reparse-check-{0}' -f [Guid]::NewGuid().ToString('N')
)
$reparseCheckedDir = Join-Path $reparseTreeRoot 'checked'
$reparseTarget = Join-Path $reparseTreeRoot 'target'
$reparseLink = Join-Path $reparseCheckedDir 'junction'
New-Item -ItemType Directory -Path @($reparseCheckedDir, $reparseTarget) | Out-Null
try {
    Assert-WindowsNativeTreeHasNoReparsePoints `
        -RootPath $reparseCheckedDir `
        -Label 'clean binaries dir'
    New-Item -ItemType Junction -Path $reparseLink -Target $reparseTarget | Out-Null
    try {
        Assert-ThrowsContaining `
            'reparse point in binaries dir' `
            'contains a reparse point' {
                Assert-WindowsNativeTreeHasNoReparsePoints `
                    -RootPath $reparseCheckedDir `
                    -Label 'clean binaries dir'
            }
    } finally {
        if (Test-Path -LiteralPath $reparseLink) {
            [IO.Directory]::Delete($reparseLink, $false)
        }
    }
} finally {
    if (Test-Path -LiteralPath $reparseTreeRoot) {
        Remove-Item -LiteralPath $reparseTreeRoot -Recurse -Force
    }
}

$mutationDenyTestRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-native-mutation-deny-{0}' -f [Guid]::NewGuid().ToString('N')
)
$mutationDenyTree = Join-Path $mutationDenyTestRoot 'immutable'
$mutationDenyNested = Join-Path $mutationDenyTree 'nested'
$mutationDenyFile = Join-Path $mutationDenyNested 'readable.txt'
$mutationDenySID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$mutationDenyPaths = @($mutationDenyTree, $mutationDenyNested, $mutationDenyFile)
New-Item -ItemType Directory -Path $mutationDenyNested | Out-Null
[IO.File]::WriteAllText($mutationDenyFile, 'readable-after-mutation-deny')
try {
    $mutationDeny = Set-WindowsNativeTreeMutationDeny `
        -RootPath $mutationDenyTree `
        -UserSID $mutationDenySID `
        -Label 'mutation-deny test tree'
    $expectedMutationRights = `
        [Security.AccessControl.FileSystemRights]::WriteData -bor `
        [Security.AccessControl.FileSystemRights]::AppendData -bor `
        [Security.AccessControl.FileSystemRights]::WriteExtendedAttributes -bor `
        [Security.AccessControl.FileSystemRights]::WriteAttributes -bor `
        [Security.AccessControl.FileSystemRights]::Delete -bor `
        [Security.AccessControl.FileSystemRights]::DeleteSubdirectoriesAndFiles -bor `
        [Security.AccessControl.FileSystemRights]::ChangePermissions -bor `
        [Security.AccessControl.FileSystemRights]::TakeOwnership
    $forbiddenReadExecutionRights = `
        [Security.AccessControl.FileSystemRights]::Synchronize -bor `
        [Security.AccessControl.FileSystemRights]::ReadData -bor `
        [Security.AccessControl.FileSystemRights]::ReadExtendedAttributes -bor `
        [Security.AccessControl.FileSystemRights]::ReadAttributes -bor `
        [Security.AccessControl.FileSystemRights]::ReadPermissions -bor `
        [Security.AccessControl.FileSystemRights]::ExecuteFile
    if ($mutationDeny.EntryCount -ne $mutationDenyPaths.Count -or
        [int64]$mutationDeny.DeniedRights -ne [int64]$expectedMutationRights) {
        throw 'mutation-deny helper did not cover the complete synthetic tree with the exact mask'
    }
    foreach ($entryPath in $mutationDenyPaths) {
        $entry = Get-Item -LiteralPath $entryPath -Force
        $security = if ($entry -is [IO.DirectoryInfo]) {
            [IO.FileSystemAclExtensions]::GetAccessControl([IO.DirectoryInfo]$entry)
        } else {
            [IO.FileSystemAclExtensions]::GetAccessControl([IO.FileInfo]$entry)
        }
        $storedDenials = @($security.GetAccessRules(
            $true,
            $false,
            [Security.Principal.SecurityIdentifier]
        ) | Where-Object {
            $_.IdentityReference.Value -ceq $mutationDenySID -and
                $_.AccessControlType -eq [Security.AccessControl.AccessControlType]::Deny
        })
        if ($storedDenials.Count -ne 1 -or
            [int64]$storedDenials[0].FileSystemRights -ne [int64]$expectedMutationRights -or
            ([int64]$storedDenials[0].FileSystemRights -band
                [int64]$forbiddenReadExecutionRights) -ne 0) {
            throw "stored mutation-deny ACE broadened into read, execute, or synchronize rights: $entryPath"
        }
    }
    $enumeratedEntries = @(Get-ChildItem `
        -LiteralPath $mutationDenyTree `
        -Force `
        -Recurse `
        -ErrorAction Stop)
    if ($enumeratedEntries.Count -ne 2) {
        throw 'mutation-only deny prevented complete tree enumeration'
    }
    $readStream = [IO.File]::OpenRead($mutationDenyFile)
    try {
        if ($readStream.Length -ne 'readable-after-mutation-deny'.Length) {
            throw 'mutation-only deny did not preserve file read access'
        }
    } finally {
        $readStream.Dispose()
    }
    Assert-Throws 'mutation-only deny file creation' {
        [IO.File]::WriteAllText(
            (Join-Path $mutationDenyTree 'forbidden-write.txt'),
            'forbidden'
        )
    }
} finally {
    Remove-TestWindowsNativeMutationDeny `
        -Paths $mutationDenyPaths `
        -UserSID $mutationDenySID
    if (Test-Path -LiteralPath $mutationDenyTestRoot) {
        Remove-Item -LiteralPath $mutationDenyTestRoot -Recurse -Force
    }
}

$rootContractBase = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-native-root-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
New-Item -ItemType Directory -Path $rootContractBase | Out-Null
$ownedRoot = $null
try {
    $ownedLeaf = 'windshare-release-{0}' -f [Guid]::NewGuid().ToString('N')
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
    & $icacls $ownedRoot.RootPath '/grant:r' "*${workerAccessSID}:RX" '/Q' | Out-Null
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
    $nestedLeaf = 'windshare-release-{0}' -f [Guid]::NewGuid().ToString('N')
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
$githubTemporaryRoot = 'C:\ProgramData\windshare-release-{0}' -f ('a' * 32)
$githubBinariesRoot = [IO.Path]::Combine($githubTemporaryRoot, 'windows-native-binaries')
$githubWorkerRoot = [IO.Path]::Combine($githubTemporaryRoot, 'windows-native-worker')
$githubWorkerScript = [IO.Path]::Combine($githubBinariesRoot, 'worker.ps1')
$githubPowerShellExecutable = 'C:\Program Files\PowerShell\7\pwsh.exe'
$githubWorkerSID = 'S-1-5-21-1234567890-1234567890-1234567890-1001'
$githubArgumentLine = New-WindowsNativeWorkerArgumentLine `
    -PowerShellExecutable $githubPowerShellExecutable `
    -WorkerScript $githubWorkerScript `
    -BinariesRoot $githubBinariesRoot `
    -WorkRoot $githubWorkerRoot `
    -ExpectedUserSID $githubWorkerSID
$expectedGithubArgumentLine = @(
    '-NoLogo',
    '-NoProfile',
    '-NonInteractive',
    '-ExecutionPolicy',
    'Bypass',
    '-File',
    ('"{0}"' -f $githubWorkerScript),
    '-BinariesRoot',
    ('"{0}"' -f $githubBinariesRoot),
    '-WorkRoot',
    ('"{0}"' -f $githubWorkerRoot),
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
            -BinariesRoot ('D:\' + ('a' * 900)) `
            -WorkRoot $githubWorkerRoot `
            -ExpectedUserSID $githubWorkerSID
    }
Assert-ThrowsContaining `
    'control character in worker launch value' `
    'contains a control character' {
        New-WindowsNativeWorkerArgumentLine `
            -PowerShellExecutable $githubPowerShellExecutable `
            -WorkerScript $githubWorkerScript `
            -BinariesRoot ($githubBinariesRoot + "`nchild") `
            -WorkRoot $githubWorkerRoot `
            -ExpectedUserSID $githubWorkerSID
    }
Assert-ThrowsContaining `
    'quote in worker launch value' `
    'contains a double quote' {
        New-WindowsNativeWorkerArgumentLine `
            -PowerShellExecutable $githubPowerShellExecutable `
            -WorkerScript ($githubWorkerScript + '"') `
            -BinariesRoot $githubBinariesRoot `
            -WorkRoot $githubWorkerRoot `
            -ExpectedUserSID $githubWorkerSID
    }

$writableProbeRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-native-readonly-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
New-Item -ItemType Directory -Path $writableProbeRoot | Out-Null
try {
    Assert-Throws 'writable binaries root' {
        Assert-WindowsNativeReadOnlyDirectory -Path $writableProbeRoot -Label 'contract binaries'
    }
    if (@(Get-ChildItem -LiteralPath $writableProbeRoot -Force).Count -ne 0) {
        throw 'write-denial probe did not remove its unexpected artifact'
    }
} finally {
    Remove-Item -LiteralPath $writableProbeRoot -Recurse -Force
}

$rawTest2JsonRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-test2json-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
New-Item -ItemType Directory -Path $rawTest2JsonRoot | Out-Null
try {
    $rawStdoutPath = Join-Path $rawTest2JsonRoot 'binary-stdout.log'
    $rawStderrPath = Join-Path $rawTest2JsonRoot 'binary-stderr.log'
    $binaryExitCode = Invoke-WindowsNativeTestBinary `
        -BinaryPath $coordinatorGoExecutable `
        -WorkingDirectory $rawTest2JsonRoot `
        -Arguments @('version') `
        -StdoutPath $rawStdoutPath `
        -StderrPath $rawStderrPath
    if ($binaryExitCode -isnot [int] -or $binaryExitCode -ne 0) {
        throw "Invoke-WindowsNativeTestBinary did not return a singular zero int exit code: $($binaryExitCode | Out-String)"
    }
    if (-not ([IO.File]::ReadAllText($rawStdoutPath)).StartsWith('go version ', [StringComparison]::Ordinal)) {
        throw 'Invoke-WindowsNativeTestBinary did not capture raw stdout'
    }

    $rawTest2JsonFile = Join-Path $rawTest2JsonRoot 'framed.log'
    $frameMarker = [char]0x16
    $rawContent = @(
        "${frameMarker}=== RUN   TestTaskLockIsProcessExclusiveAndRecoversAfterExit",
        "${frameMarker}--- PASS: TestTaskLockIsProcessExclusiveAndRecoversAfterExit (0.01s)",
        "${frameMarker}PASS"
    ) -join "`n"
    [IO.File]::WriteAllText($rawTest2JsonFile, $rawContent + "`n", [Text.UTF8Encoding]::new($false))
    $convertedLines = @(ConvertFrom-WindowsNativeTest2JsonOutput `
        -GoExecutable $coordinatorGoExecutable `
        -PackageImportPath 'github.com/windshare/windshare/osfs/internal/outputportable' `
        -RawOutputPath $rawTest2JsonFile)
    $convertedEvents = @(ConvertFrom-WindowsNativeTestJSONLines -Lines $convertedLines)
    Assert-WindowsNativeTestEvents `
        -ExitCode 0 `
        -Events $convertedEvents `
        -RequiredTests @($lockTestName) `
        -SelectorPackage './osfs/internal/outputportable' `
        -SelectorExpression '^TestTaskLockIsProcessExclusiveAndRecoversAfterExit$'
} finally {
    if (Test-Path -LiteralPath $rawTest2JsonRoot) {
        Remove-Item -LiteralPath $rawTest2JsonRoot -Recurse -Force
    }
}

$passingEvents = @($requiredTests | ForEach-Object {
    [pscustomobject]@{ Action = 'run'; Test = $_; Package = 'example/osfs' }
    [pscustomobject]@{ Action = 'pass'; Test = $_; Package = 'example/osfs' }
}) + [pscustomobject]@{ Action = 'pass'; Package = 'example/osfs' }
$jsonEvents = @(ConvertFrom-WindowsNativeTestJSONLines -Lines @(
    '{"Action":"run","Test":"TestNativeOutputDestinationCapabilities"}',
    '{"Action":"pass","Test":"TestNativeOutputDestinationCapabilities"}',
    '{"Action":"run","Test":"TestNativeOutputProcessRestartKeepsRanges"}',
    '{"Action":"pass","Test":"TestNativeOutputProcessRestartKeepsRanges"}',
    '{"Action":"run","Test":"TestNativeOutputRetainedCandidateListAndReopen"}',
    '{"Action":"pass","Test":"TestNativeOutputRetainedCandidateListAndReopen"}',
    '{"Action":"run","Test":"TestTaskLockIsProcessExclusiveAndRecoversAfterExit"}',
    '{"Action":"pass","Test":"TestTaskLockIsProcessExclusiveAndRecoversAfterExit"}',
    '{"Action":"run","Test":"TestOrdinaryFilesPublishWithoutLinksAndNeverOverwrite"}',
    '{"Action":"pass","Test":"TestOrdinaryFilesPublishWithoutLinksAndNeverOverwrite"}',
    '{"Action":"run","Test":"TestWindowsModifiedTimePreservesNativePrecisionAndHandleIdentity"}',
    '{"Action":"pass","Test":"TestWindowsModifiedTimePreservesNativePrecisionAndHandleIdentity"}'
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
Assert-Throws 'missing lock evidence' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        $testName -cne $lockTestName
    })
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'missing lock top-level run' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        -not ($_.Action -ceq 'run' -and $testName -ceq $lockTestName)
    })
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'missing lock top-level pass' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        -not ($_.Action -ceq 'pass' -and $testName -ceq $lockTestName)
    })
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'skipped lock test' {
    $events = @($passingEvents) + [pscustomobject]@{
        Action = 'skip'
        Test = $lockTestName
        Package = 'example/osfs'
    }
    Assert-WindowsNativeTestEvents -ExitCode 0 -Events $events -RequiredTests $requiredTests
}
Assert-Throws 'failed lock test' {
    $events = @($passingEvents | Where-Object {
        $testProperty = $_.PSObject.Properties['Test']
        $testName = if ($null -eq $testProperty) { '' } else { [string]$testProperty.Value }
        -not ($_.Action -ceq 'pass' -and $testName -ceq $lockTestName)
    }) + [pscustomobject]@{
        Action = 'fail'
        Test = $lockTestName
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
Assert-ThrowsContaining 'zero-test package selector' 'selector ran zero tests: package=./osfs/internal/outputportable' {
    Assert-WindowsNativeTestEvents `
        -ExitCode 0 `
        -Events @(
            [pscustomobject]@{ Action = 'start'; Package = 'example/outputportable' },
            [pscustomobject]@{ Action = 'pass'; Package = 'example/outputportable' }
        ) `
        -RequiredTests @('TestTaskLockIsProcessExclusiveAndRecoversAfterExit') `
        -SelectorPackage './osfs/internal/outputportable' `
        -SelectorExpression '^(TestTaskLockIsProcessExclusiveAndRecoversAfterExit)$'
}

$nativeModulePath = Join-Path $PSScriptRoot 'certify.psm1'
foreach ($requiredModuleText in @(
    'CreateDirectoryW',
    'CreateExclusive',
    'DeleteProfileW',
    'Get-WindowsNativeEphemeralUserProfileRegistration',
    'WindowsNativeProfileRetryableErrors',
    'Remove-WindowsNativeEphemeralUserProfile',
    'SetAccessRuleProtection($true, $false)',
    '[Environment+SpecialFolder]::CommonApplicationData',
    'CoordinatorReleaseRootLeafPattern',
    'Assert-WindowsNativeCoordinatorReleaseRoot -Ownership $Ownership',
    'Assert-WindowsNativeTreeHasNoReparsePoints',
    'Set-WindowsNativeTreeMutationDeny',
    'ConvertFrom-WindowsNativeTest2JsonOutput',
    'FileSystemAccessRule'
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
    $interopInitializerIndex,
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

$nativeWorkerPath = Join-Path $PSScriptRoot 'worker.ps1'
foreach ($requiredWorkerText in @(
    "`$moduleSuiteTestTimeout = '30m'",
    '"-test.timeout=$moduleSuiteTestTimeout"',
    "'-test.v=test2json'",
    '[Environment+SpecialFolder]::UserProfile',
    '$env:USERPROFILE = $resolvedUserProfile',
    '$env:HOME = $resolvedUserProfile',
    '$env:HOMEDRIVE = $homeDrive',
    '$env:HOMEPATH = $homePath',
    '-- standard-user profile and identity environment verified',
    '-- standard-user NTFS roots and precompiled test binary immutability verified',
    'Assert-WindowsNativeTreeHasNoReparsePoints',
    'Get-WindowsNativeRequiredTestSelectors'
)) {
    Assert-FileContains -Path $nativeWorkerPath -Expected $requiredWorkerText
}

Write-Output 'Windows native-output certification tests: PASS'
