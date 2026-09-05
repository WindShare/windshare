[CmdletBinding()]
param()

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$templatePath = Join-Path $repositoryRoot 'web/src/output/file-system-access/compatible-name/restoration/windows-v1.ps1'
$expectedTemplateId = 'windows-powershell-v1'
$expectedSidecarVersion = 'windshare-name-restoration/v2'
$utf8WithoutBom = New-Object Text.UTF8Encoding($false, $true)

function Assert-True {
    param(
        [Parameter(Mandatory = $true)]
        [bool]$Condition,

        [Parameter(Mandatory = $true)]
        [string]$Message
    )

    if (-not $Condition) {
        throw "ASSERTION FAILED: $Message"
    }
}

function Assert-Equal {
    param(
        [Parameter(Mandatory = $true)]
        [AllowNull()]
        [object]$Expected,

        [Parameter(Mandatory = $true)]
        [AllowNull()]
        [object]$Actual,

        [Parameter(Mandatory = $true)]
        [string]$Message
    )

    if ($Expected -cne $Actual) {
        throw "ASSERTION FAILED: $Message. Expected '$Expected', got '$Actual'."
    }
}

function Assert-Throws {
    param(
        [Parameter(Mandatory = $true)]
        [scriptblock]$Action,

        [Parameter(Mandatory = $true)]
        [string]$Message
    )

    $threw = $false
    try {
        & $Action
    } catch {
        $threw = $true
    }
    Assert-True -Condition $threw -Message $Message
}

function ConvertTo-Base64Utf8 {
    param(
        [Parameter(Mandatory = $true)]
        [AllowEmptyString()]
        [string]$Value
    )

    return [Convert]::ToBase64String($utf8WithoutBom.GetBytes($Value))
}

function Write-Sidecar {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [object[]]$Records,

        [Parameter(Mandatory = $true)]
        [ValidateSet('active', 'completed', 'stopped', 'failed')]
        [string]$State,

        [Parameter()]
        [AllowEmptyString()]
        [string]$IncompleteTail = ''
    )

    $lines = New-Object 'Collections.Generic.List[string]'
    $operationId = ConvertTo-Base64Utf8 -Value ('contract-' + [IO.Path]::GetFileName(
        [IO.Path]::GetDirectoryName($Path)
    ))
    [void]$lines.Add("H`t$expectedSidecarVersion`t$operationId`tinside")
    foreach ($record in $Records) {
        $logicalPath = ConvertTo-Base64Utf8 -Value $record.LogicalPath
        $physicalComponent = ConvertTo-Base64Utf8 -Value $record.PhysicalComponent
        [void]$lines.Add(
            "M`t$($record.Ordinal)`t$($record.Kind)`t$logicalPath`t$physicalComponent"
        )
    }
    [void]$lines.Add("F`t$($Records.Count)`t$State")

    $text = [string]::Join("`n", $lines.ToArray()) + "`n" + $IncompleteTail
    [IO.File]::WriteAllText($Path, $text, $utf8WithoutBom)
}

function New-ContractFile {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$Contents
    )

    [IO.File]::WriteAllText($Path, $Contents, $utf8WithoutBom)
}

function Get-TreeSnapshot {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    $root = [IO.Path]::GetFullPath($Path)
    $entries = @(
        Get-ChildItem -LiteralPath $root -Force -Recurse |
            Sort-Object -Property FullName
    )
    $snapshot = New-Object 'Collections.Generic.List[string]'
    foreach ($entry in $entries) {
        $relativePath = $entry.FullName.Substring($root.Length).TrimStart('\')
        if ($entry.PSIsContainer) {
            [void]$snapshot.Add("D`t$relativePath")
        } else {
            $bytes = [IO.File]::ReadAllBytes($entry.FullName)
            [void]$snapshot.Add(
                "F`t$relativePath`t$([Convert]::ToBase64String($bytes))"
            )
        }
    }
    return [string]::Join("`n", $snapshot.ToArray())
}

function New-OperationRoot {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Parent,

        [Parameter(Mandatory = $true)]
        [string]$Name
    )

    $operationRoot = Join-Path $Parent $Name
    [void](New-Item -ItemType Directory -Path $operationRoot)
    $scriptPath = Join-Path $operationRoot 'restore.windshare-aaaaaa.ps1'
    [IO.File]::Copy($templatePath, $scriptPath)
    Assert-Equal `
        -Expected (Get-FileHash -LiteralPath $templatePath -Algorithm SHA256).Hash `
        -Actual (Get-FileHash -LiteralPath $scriptPath -Algorithm SHA256).Hash `
        -Message 'the executed restoration script must be the exact production asset'
    return [PSCustomObject]@{
        Root = $operationRoot
        Script = $scriptPath
        Sidecar = [IO.Path]::ChangeExtension($scriptPath, '.data')
    }
}

function Invoke-DisplayedRestorationCommand {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ScriptPath
    )

    # A fresh process proves the user-facing command owns its policy override instead of inheriting
    # this contract process's ambient Bypass policy.
    & powershell.exe `
        -NoProfile `
        -ExecutionPolicy Bypass `
        -File $ScriptPath
    if ($LASTEXITCODE -ne 0) {
        throw "Displayed WindShare restoration command failed with exit code $LASTEXITCODE."
    }
}

function Invoke-DisplayedRestorationCommandFromRestrictedParent {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ScriptPath
    )

    $displayedCommand = (
        'powershell.exe -NoProfile -ExecutionPolicy Bypass -File ".\' +
        [IO.Path]::GetFileName($ScriptPath) + '"'
    )
    $parentCommand = (
        "if ((Get-ExecutionPolicy) -ne 'Restricted') { throw 'Expected a Restricted parent.' }; " +
        $displayedCommand + '; exit $LASTEXITCODE'
    )
    # Encoding transports the copied command intact through the outer native argument parser.
    $encodedParentCommand = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($parentCommand))
    Push-Location -LiteralPath ([IO.Path]::GetDirectoryName($ScriptPath))
    try {
        & powershell.exe `
            -NoProfile `
            -NonInteractive `
            -ExecutionPolicy Restricted `
            -EncodedCommand $encodedParentCommand
        if ($LASTEXITCODE -ne 0) {
            throw "Displayed command failed from a Restricted parent with exit code $LASTEXITCODE."
        }
    } finally {
        Pop-Location
    }
}

if (
    $PSVersionTable.PSVersion.Major -ne 5 -or
    $PSVersionTable.PSVersion.Minor -ne 1
) {
    throw "This matching-host contract must run under Windows PowerShell 5.1, got $($PSVersionTable.PSVersion)."
}
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    throw 'This contract requires a Windows operating system.'
}
Assert-True -Condition (Test-Path -LiteralPath $templatePath -PathType Leaf) -Message 'production template must exist'

# Dot-sourcing is the supported proof seam: the contract below calls the exact
# production P/Invoke function rather than maintaining a lookalike helper.
. $templatePath
Assert-Equal `
    -Expected $expectedTemplateId `
    -Actual $script:WindShareRestorationTemplateId `
    -Message 'template identifier'
Assert-Equal `
    -Expected $expectedSidecarVersion `
    -Actual $script:WindShareSidecarVersion `
    -Message 'sidecar format identifier'
Assert-True `
    -Condition ($null -ne (Get-Command Invoke-WindShareNoReplaceMove -ErrorAction Stop)) `
    -Message 'dot-sourcing must expose the production no-replace primitive'

$temporaryBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$temporaryRoot = Join-Path $temporaryBase (
    'windshare-browser-restoration-contract-' + [Guid]::NewGuid().ToString('N')
)
[void](New-Item -ItemType Directory -Path $temporaryRoot)

try {
    Write-Output '-- exact MoveFileExW flags=0 primitive'

    $fileSuccessRoot = Join-Path $temporaryRoot 'primitive-file-success'
    [void](New-Item -ItemType Directory -Path $fileSuccessRoot)
    $fileSuccessSource = Join-Path $fileSuccessRoot 'source.windshare-aaaaaa'
    $fileSuccessTarget = Join-Path $fileSuccessRoot 'pyvenv.cfg'
    New-ContractFile -Path $fileSuccessSource -Contents 'file-source-bytes'
    Invoke-WindShareNoReplaceMove `
        -SourcePath $fileSuccessSource `
        -TargetPath $fileSuccessTarget
    Assert-True -Condition (-not (Test-Path -LiteralPath $fileSuccessSource)) -Message 'successful file move removes source name'
    Assert-Equal -Expected 'file-source-bytes' -Actual ([IO.File]::ReadAllText($fileSuccessTarget)) -Message 'successful file move preserves bytes'

    $directorySuccessRoot = Join-Path $temporaryRoot 'primitive-directory-success'
    [void](New-Item -ItemType Directory -Path $directorySuccessRoot)
    $directorySuccessSource = Join-Path $directorySuccessRoot 'folder.windshare-aaaaaa'
    $directorySuccessTarget = Join-Path $directorySuccessRoot 'folder'
    [void](New-Item -ItemType Directory -Path (Join-Path $directorySuccessSource 'nested'))
    New-ContractFile -Path (Join-Path $directorySuccessSource 'nested/payload.bin') -Contents 'directory-tree-bytes'
    Invoke-WindShareNoReplaceMove `
        -SourcePath $directorySuccessSource `
        -TargetPath $directorySuccessTarget
    Assert-True -Condition (-not (Test-Path -LiteralPath $directorySuccessSource)) -Message 'successful directory move removes source name'
    Assert-Equal `
        -Expected 'directory-tree-bytes' `
        -Actual ([IO.File]::ReadAllText((Join-Path $directorySuccessTarget 'nested/payload.bin'))) `
        -Message 'successful non-empty directory move preserves its tree'

    $fileRefusalRoot = Join-Path $temporaryRoot 'primitive-file-refusal'
    [void](New-Item -ItemType Directory -Path $fileRefusalRoot)
    $fileRefusalSource = Join-Path $fileRefusalRoot 'source.windshare-bbbbbb'
    $fileRefusalTarget = Join-Path $fileRefusalRoot 'occupied.txt'
    New-ContractFile -Path $fileRefusalSource -Contents 'source-must-survive'
    New-ContractFile -Path $fileRefusalTarget -Contents 'target-must-survive'
    $fileRefusalBefore = Get-TreeSnapshot -Path $fileRefusalRoot
    Assert-Throws `
        -Action {
            Invoke-WindShareNoReplaceMove `
                -SourcePath $fileRefusalSource `
                -TargetPath $fileRefusalTarget
        } `
        -Message 'occupied file target must be refused'
    Assert-Equal `
        -Expected $fileRefusalBefore `
        -Actual (Get-TreeSnapshot -Path $fileRefusalRoot) `
        -Message 'occupied file refusal preserves both source and target bytes'

    $directoryRefusalRoot = Join-Path $temporaryRoot 'primitive-directory-refusal'
    [void](New-Item -ItemType Directory -Path $directoryRefusalRoot)
    $directoryRefusalSource = Join-Path $directoryRefusalRoot 'source.windshare-cccccc'
    $directoryRefusalTarget = Join-Path $directoryRefusalRoot 'occupied'
    [void](New-Item -ItemType Directory -Path (Join-Path $directoryRefusalSource 'source-child'))
    [void](New-Item -ItemType Directory -Path (Join-Path $directoryRefusalTarget 'target-child'))
    New-ContractFile -Path (Join-Path $directoryRefusalSource 'source-child/source.bin') -Contents 'source-tree-must-survive'
    New-ContractFile -Path (Join-Path $directoryRefusalTarget 'target-child/target.bin') -Contents 'target-tree-must-survive'
    $directoryRefusalBefore = Get-TreeSnapshot -Path $directoryRefusalRoot
    Assert-Throws `
        -Action {
            Invoke-WindShareNoReplaceMove `
                -SourcePath $directoryRefusalSource `
                -TargetPath $directoryRefusalTarget
        } `
        -Message 'occupied directory target must be refused'
    Assert-Equal `
        -Expected $directoryRefusalBefore `
        -Actual (Get-TreeSnapshot -Path $directoryRefusalRoot) `
        -Message 'occupied directory refusal preserves both non-empty trees'

    Write-Output '-- production sidecar restoration entry point'

    # Build Unicode at runtime because Windows PowerShell reads BOM-less scripts using ANSI.
    $unicodeFolder = 'complete and rerun ' + [char]0x4E0B + [char]0x8F7D
    $complete = New-OperationRoot -Parent $temporaryRoot -Name $unicodeFolder
    $compatibleDirectory = Join-Path $complete.Root 'rejected-dir.windshare-dddddd'
    [void](New-Item -ItemType Directory -Path $compatibleDirectory)
    $compatibleFile = Join-Path $compatibleDirectory 'pyvenv.windshare-dddddd'
    New-ContractFile -Path $compatibleFile -Contents 'nested-content'
    $completeRecords = @(
        [PSCustomObject]@{
            Ordinal = 1
            Kind = 'directory'
            LogicalPath = 'rejected-dir'
            PhysicalComponent = 'rejected-dir.windshare-dddddd'
        },
        [PSCustomObject]@{
            Ordinal = 2
            Kind = 'file'
            LogicalPath = 'rejected-dir/pyvenv.cfg'
            PhysicalComponent = 'pyvenv.windshare-dddddd'
        }
    )
    $ignoredTail = "M`t3`tfile`t" + (ConvertTo-Base64Utf8 -Value 'ignored.txt')
    Write-Sidecar `
        -Path $complete.Sidecar `
        -Records $completeRecords `
        -State completed `
        -IncompleteTail $ignoredTail
    Invoke-DisplayedRestorationCommandFromRestrictedParent `
        -ScriptPath $complete.Script | Out-Null
    Assert-Equal `
        -Expected 'nested-content' `
        -Actual ([IO.File]::ReadAllText((Join-Path $complete.Root 'rejected-dir/pyvenv.cfg'))) `
        -Message 'deepest-first restoration rebases through a renamed ancestor'
    Assert-True `
        -Condition (-not (Test-Path -LiteralPath $compatibleDirectory)) `
        -Message 'the compatible ancestor name is gone after restoration'
    Invoke-DisplayedRestorationCommand `
        -ScriptPath $complete.Script | Out-Null
    Assert-Equal `
        -Expected 'nested-content' `
        -Actual ([IO.File]::ReadAllText((Join-Path $complete.Root 'rejected-dir/pyvenv.cfg'))) `
        -Message 'rerun treats restored path state as complete'

    $missingPair = New-OperationRoot -Parent $temporaryRoot -Name 'missing exact pair'
    $pairSource = Join-Path $missingPair.Root 'payload.windshare-bbbbbb'
    New-ContractFile -Path $pairSource -Contents 'must-remain-compatible'
    $pairRecords = @(
        [PSCustomObject]@{
            Ordinal = 1
            Kind = 'file'
            LogicalPath = 'payload.txt'
            PhysicalComponent = 'payload.windshare-bbbbbb'
        }
    )
    foreach ($hasNeighbor in @($false, $true)) {
        if ($hasNeighbor) {
            Write-Sidecar `
                -Path (Join-Path $missingPair.Root 'restore.windshare-bbbbbb.data') `
                -Records $pairRecords `
                -State completed
        }
        $missingPairBefore = Get-TreeSnapshot -Path $missingPair.Root
        $missingPairError = $null
        try {
            & $missingPair.Script | Out-Null
        } catch {
            $missingPairError = $_.Exception.Message
        }
        Assert-True `
            -Condition ($null -ne $missingPairError -and $missingPairError.Contains($missingPair.Sidecar)) `
            -Message 'missing exact partner reports its expected absolute path, even with another valid sidecar nearby'
        Assert-Equal `
            -Expected $missingPairBefore `
            -Actual (Get-TreeSnapshot -Path $missingPair.Root) `
            -Message 'missing exact pair does not use a neighboring operation or mutate content'
    }

    $active = New-OperationRoot -Parent $temporaryRoot -Name 'active confirmation'
    New-ContractFile -Path (Join-Path $active.Root 'payload.windshare-bbbbbb') -Contents 'active-content'
    Write-Sidecar -Path $active.Sidecar -Records $pairRecords -State active
    $activeBefore = Get-TreeSnapshot -Path $active.Root
    foreach ($response in @('', 'restore', 'RESTORE')) {
        # Scoped host injection exercises the exact production function without interactive input.
        $activeError = $null
        try {
            & {
                param($Operation, $Response)
                . $Operation.Script
                function Read-Host {
                    param([string]$Prompt)
                    Assert-True `
                        -Condition ($Prompt.Contains('no longer receiving') -and $Prompt.Contains('will not resume')) `
                        -Message 'active confirmation explains receiving and resume consequences'
                    return $Response
                }
                Invoke-WindShareRestoration -Path $Operation.Sidecar | Out-Null
            } $active $response
        } catch {
            $activeError = $_.Exception.Message
        }
        if ($response -cne 'RESTORE') {
            Assert-True `
                -Condition ($null -ne $activeError -and $activeError.Contains('was cancelled')) `
                -Message 'only exact RESTORE confirms an active checkpoint'
            Assert-Equal `
                -Expected $activeBefore `
                -Actual (Get-TreeSnapshot -Path $active.Root) `
                -Message 'cancelled active restoration preserves the complete output tree'
        } else {
            Assert-True -Condition ($null -eq $activeError) -Message "confirmed active restoration succeeds: $activeError"
            Assert-Equal `
                -Expected 'active-content' `
                -Actual ([IO.File]::ReadAllText((Join-Path $active.Root 'payload.txt'))) `
                -Message 'confirmed active restoration preserves content'
        }
    }

    $conflict = New-OperationRoot -Parent $temporaryRoot -Name 'four-state-conflict'
    $conflictSource = Join-Path $conflict.Root 'conflict.windshare-eeeeee'
    $conflictTarget = Join-Path $conflict.Root 'conflict.txt'
    New-ContractFile -Path $conflictSource -Contents 'compatible-source'
    New-ContractFile -Path $conflictTarget -Contents 'occupied-original'
    $conflictRecords = @(
        [PSCustomObject]@{
            Ordinal = 1
            Kind = 'file'
            LogicalPath = 'conflict.txt'
            PhysicalComponent = 'conflict.windshare-eeeeee'
        }
    )
    Write-Sidecar -Path $conflict.Sidecar -Records $conflictRecords -State stopped
    $conflictBefore = Get-TreeSnapshot -Path $conflict.Root
    Assert-Throws `
        -Action {
            Invoke-DisplayedRestorationCommand `
                -ScriptPath $conflict.Script | Out-Null
        } `
        -Message 'both-present restoration state must stop'
    Assert-Equal `
        -Expected $conflictBefore `
        -Actual (Get-TreeSnapshot -Path $conflict.Root) `
        -Message 'both-present restoration state preserves source and target'

    $missing = New-OperationRoot -Parent $temporaryRoot -Name 'four-state-missing'
    $missingRecords = @(
        [PSCustomObject]@{
            Ordinal = 1
            Kind = 'file'
            LogicalPath = 'missing.txt'
            PhysicalComponent = 'missing.windshare-ffffff'
        }
    )
    Write-Sidecar -Path $missing.Sidecar -Records $missingRecords -State failed
    $missingBefore = Get-TreeSnapshot -Path $missing.Root
    Assert-Throws `
        -Action {
            Invoke-DisplayedRestorationCommand `
                -ScriptPath $missing.Script | Out-Null
        } `
        -Message 'both-absent restoration state must stop'
    Assert-Equal `
        -Expected $missingBefore `
        -Actual (Get-TreeSnapshot -Path $missing.Root) `
        -Message 'both-absent restoration state performs no mutation'

    $escape = New-OperationRoot -Parent $temporaryRoot -Name 'path-confinement'
    $escapeRecords = @(
        [PSCustomObject]@{
            Ordinal = 1
            Kind = 'file'
            LogicalPath = '../outside.txt'
            PhysicalComponent = 'outside.windshare-gggggg'
        }
    )
    Write-Sidecar -Path $escape.Sidecar -Records $escapeRecords -State completed
    $escapeBefore = Get-TreeSnapshot -Path $escape.Root
    Assert-Throws `
        -Action {
            Invoke-DisplayedRestorationCommand `
                -ScriptPath $escape.Script | Out-Null
        } `
        -Message 'escaping logical paths must be rejected before restoration'
    Assert-Equal `
        -Expected $escapeBefore `
        -Actual (Get-TreeSnapshot -Path $escape.Root) `
        -Message 'path-confinement rejection performs no mutation'

    Write-Output 'browser compatible-name Windows restoration contract: PASS'
} finally {
    $resolvedTemporaryRoot = [IO.Path]::GetFullPath($temporaryRoot)
    $temporaryPrefix = $temporaryBase.TrimEnd('\') + '\'
    if (-not $resolvedTemporaryRoot.StartsWith(
        $temporaryPrefix,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw "Refusing to clean an unexpected contract path: '$resolvedTemporaryRoot'."
    }
    if (Test-Path -LiteralPath $resolvedTemporaryRoot) {
        Remove-Item -LiteralPath $resolvedTemporaryRoot -Recurse -Force
    }
}
