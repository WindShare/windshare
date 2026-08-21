# WindShare compatible-name restoration template: windows-powershell-v1
#
# This file is an immutable product asset. It deliberately contains no receive-specific
# paths; the adjacent sidecar is the only operation-specific input.
[CmdletBinding()]
param(
    [Parameter()]
    [string]$SidecarPath
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$script:WindShareRestorationTemplateId = 'windows-powershell-v1'
$script:WindShareSidecarVersion = 'windshare-name-restoration/v1'
$script:WindShareTerminalStates = @('completed', 'stopped', 'failed')
$script:WindShareAllowedStates = @('active') + $script:WindShareTerminalStates
$script:WindShareAllowedPlacements = @('inside', 'beside')

if ($null -eq ('WindShare.Restoration.NativeMethods' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

namespace WindShare.Restoration
{
    public static class NativeMethods
    {
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true, EntryPoint = "MoveFileExW")]
        [return: MarshalAs(UnmanagedType.Bool)]
        public static extern bool MoveFileExW(
            string existingFileName,
            string newFileName,
            UInt32 flags);
    }
}
'@
}

function ConvertTo-WindShareExtendedPath {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    $fullPath = [IO.Path]::GetFullPath($Path)
    if ($fullPath.StartsWith('\\?\', [StringComparison]::Ordinal)) {
        return $fullPath
    }
    if ($fullPath.StartsWith('\\', [StringComparison]::Ordinal)) {
        return '\\?\UNC\' + $fullPath.Substring(2)
    }
    return '\\?\' + $fullPath
}

function Invoke-WindShareNoReplaceMove {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$SourcePath,

        [Parameter(Mandatory = $true)]
        [string]$TargetPath
    )

    $sourceFullPath = [IO.Path]::GetFullPath($SourcePath)
    $targetFullPath = [IO.Path]::GetFullPath($TargetPath)
    $sourceParent = [IO.Path]::GetDirectoryName($sourceFullPath)
    $targetParent = [IO.Path]::GetDirectoryName($targetFullPath)

    # Sibling-only restoration keeps the operation on one volume and prevents this
    # low-frequency repair tool from becoming a general-purpose move primitive.
    if (-not [string]::Equals(
        $sourceParent,
        $targetParent,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw "WindShare restoration requires sibling source and target paths: '$sourceFullPath' and '$targetFullPath'."
    }
    if ([string]::Equals(
        $sourceFullPath,
        $targetFullPath,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw "WindShare restoration source and target resolve to the same path: '$sourceFullPath'."
    }

    $nativeSource = ConvertTo-WindShareExtendedPath -Path $sourceFullPath
    $nativeTarget = ConvertTo-WindShareExtendedPath -Path $targetFullPath

    # Flags must remain exactly zero. Replacement and cross-volume copy are opt-in
    # MoveFileEx behaviors and would violate the restoration no-replace contract.
    $moved = [WindShare.Restoration.NativeMethods]::MoveFileExW(
        $nativeSource,
        $nativeTarget,
        [uint32]0
    )
    if (-not $moved) {
        $nativeError = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
        $exception = New-Object ComponentModel.Win32Exception(
            $nativeError,
            "WindShare refused to rename '$sourceFullPath' to '$targetFullPath'."
        )
        $errorRecord = New-Object Management.Automation.ErrorRecord(
            $exception,
            'WindShareNoReplaceMoveFailed',
            [Management.Automation.ErrorCategory]::WriteError,
            $sourceFullPath
        )
        $PSCmdlet.ThrowTerminatingError($errorRecord)
    }
}

function ConvertFrom-WindShareBase64Utf8 {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [AllowEmptyString()]
        [string]$Value,

        [Parameter(Mandatory = $true)]
        [string]$FieldName
    )

    try {
        $bytes = [Convert]::FromBase64String($Value)
        $strictUtf8 = New-Object Text.UTF8Encoding($false, $true)
        $decoded = $strictUtf8.GetString($bytes)
    } catch {
        throw "WindShare sidecar field '$FieldName' is not strict Base64-encoded UTF-8."
    }

    # Canonical Base64 prevents multiple textual encodings from naming the same
    # path and keeps duplicate detection unambiguous.
    if ([Convert]::ToBase64String($strictUtf8.GetBytes($decoded)) -cne $Value) {
        throw "WindShare sidecar field '$FieldName' is not canonical Base64."
    }
    return $decoded
}

function Test-WindShareContainsControlCharacter {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [AllowEmptyString()]
        [string]$Value
    )

    foreach ($character in $Value.ToCharArray()) {
        if ([char]::IsControl($character)) {
            return $true
        }
    }
    return $false
}

function Assert-WindSharePathComponent {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [AllowEmptyString()]
        [string]$Component,

        [Parameter(Mandatory = $true)]
        [string]$FieldName
    )

    if (
        [string]::IsNullOrEmpty($Component) -or
        $Component -eq '.' -or
        $Component -eq '..' -or
        $Component.IndexOfAny([IO.Path]::GetInvalidFileNameChars()) -ge 0 -or
        (Test-WindShareContainsControlCharacter -Value $Component)
    ) {
        throw "WindShare sidecar field '$FieldName' contains an invalid path component."
    }
}

function Get-WindShareLogicalPathParts {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [AllowEmptyString()]
        [string]$LogicalPath
    )

    if (
        [string]::IsNullOrEmpty($LogicalPath) -or
        [IO.Path]::IsPathRooted($LogicalPath) -or
        $LogicalPath.Contains('\')
    ) {
        throw "WindShare sidecar logical path '$LogicalPath' is not a confined relative path."
    }

    $parts = @($LogicalPath.Split(
        [char]'/', [StringSplitOptions]::None
    ))
    foreach ($part in $parts) {
        Assert-WindSharePathComponent `
            -Component $part `
            -FieldName 'logicalPath'
    }
    return $parts
}

function ConvertTo-WindShareNonNegativeInteger {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$Value,

        [Parameter(Mandatory = $true)]
        [string]$FieldName
    )

    $number = 0
    $parsed = [int]::TryParse(
        $Value,
        [Globalization.NumberStyles]::None,
        [Globalization.CultureInfo]::InvariantCulture,
        [ref]$number
    )
    if (-not $parsed -or $number -lt 0 -or $number.ToString(
        [Globalization.CultureInfo]::InvariantCulture
    ) -cne $Value) {
        throw "WindShare sidecar field '$FieldName' is not a canonical non-negative integer."
    }
    return $number
}

function Read-WindShareCheckpointCandidate {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$Lines,

        [Parameter(Mandatory = $true)]
        [int]$FooterIndex
    )

    if ($Lines.Count -eq 0) {
        throw 'WindShare sidecar has no complete lines.'
    }

    $headerFields = @($Lines[0].Split([char]"`t"))
    if (
        $headerFields.Count -ne 4 -or
        $headerFields[0] -cne 'H' -or
        $headerFields[1] -cne $script:WindShareSidecarVersion -or
        $script:WindShareAllowedPlacements -notcontains $headerFields[3]
    ) {
        throw 'WindShare sidecar header is malformed or unsupported.'
    }

    $operationId = ConvertFrom-WindShareBase64Utf8 `
        -Value $headerFields[2] `
        -FieldName 'operationId'
    if (
        [string]::IsNullOrEmpty($operationId) -or
        (Test-WindShareContainsControlCharacter -Value $operationId)
    ) {
        throw 'WindShare sidecar operation ID is empty or contains control characters.'
    }

    $records = New-Object 'Collections.Generic.List[object]'
    $recordsByLogicalPath = @{}
    $footerState = $null

    for ($lineIndex = 1; $lineIndex -le $FooterIndex; $lineIndex++) {
        $fields = @($Lines[$lineIndex].Split([char]"`t"))
        if ($fields[0] -ceq 'M') {
            if ($fields.Count -ne 5) {
                throw "WindShare sidecar mapping at line $($lineIndex + 1) is malformed."
            }

            $ordinal = ConvertTo-WindShareNonNegativeInteger `
                -Value $fields[1] `
                -FieldName 'ordinal'
            $expectedOrdinal = $records.Count + 1
            if ($ordinal -ne $expectedOrdinal) {
                throw "WindShare sidecar mapping ordinal $ordinal is not contiguous at $expectedOrdinal."
            }

            $kind = $fields[2]
            if ($kind -cne 'file' -and $kind -cne 'directory') {
                throw "WindShare sidecar mapping kind '$kind' is unsupported."
            }

            $logicalPath = ConvertFrom-WindShareBase64Utf8 `
                -Value $fields[3] `
                -FieldName 'logicalPath'
            $logicalParts = @(Get-WindShareLogicalPathParts -LogicalPath $logicalPath)
            $physicalComponent = ConvertFrom-WindShareBase64Utf8 `
                -Value $fields[4] `
                -FieldName 'physicalComponent'
            Assert-WindSharePathComponent `
                -Component $physicalComponent `
                -FieldName 'physicalComponent'

            $logicalLeaf = $logicalParts[$logicalParts.Count - 1]
            if ([string]::Equals(
                $logicalLeaf,
                $physicalComponent,
                [StringComparison]::OrdinalIgnoreCase
            )) {
                throw "WindShare sidecar mapping '$logicalPath' does not name a distinct physical component."
            }
            if ($recordsByLogicalPath.ContainsKey($logicalPath)) {
                throw "WindShare sidecar contains duplicate logical path '$logicalPath'."
            }

            $record = [PSCustomObject]@{
                Ordinal = $ordinal
                Kind = $kind
                LogicalPath = $logicalPath
                LogicalParts = $logicalParts
                PhysicalComponent = $physicalComponent
                Depth = $logicalParts.Count
            }
            $recordsByLogicalPath[$logicalPath] = $record
            [void]$records.Add($record)
            continue
        }

        if ($fields[0] -ceq 'F') {
            if ($fields.Count -ne 3) {
                throw "WindShare sidecar footer at line $($lineIndex + 1) is malformed."
            }

            $committedCount = ConvertTo-WindShareNonNegativeInteger `
                -Value $fields[1] `
                -FieldName 'committedCount'
            $footerState = $fields[2]
            if ($script:WindShareAllowedStates -notcontains $footerState) {
                throw "WindShare sidecar footer state '$footerState' is unsupported."
            }
            if ($committedCount -ne $records.Count) {
                throw "WindShare sidecar footer count $committedCount does not match $($records.Count) records."
            }
            if ($lineIndex -ne $FooterIndex -and $footerState -cne 'active') {
                throw 'WindShare sidecar has records after a terminal footer.'
            }
            continue
        }

        throw "WindShare sidecar line $($lineIndex + 1) has an unsupported record type."
    }

    if ($null -eq $footerState) {
        throw 'WindShare sidecar checkpoint has no footer.'
    }

    foreach ($record in $records) {
        for ($ancestorDepth = 1; $ancestorDepth -lt $record.Depth; $ancestorDepth++) {
            $ancestorPath = [string]::Join(
                '/',
                $record.LogicalParts[0..($ancestorDepth - 1)]
            )
            if (
                $recordsByLogicalPath.ContainsKey($ancestorPath) -and
                $recordsByLogicalPath[$ancestorPath].Kind -cne 'directory'
            ) {
                throw "WindShare sidecar path '$ancestorPath' is a file with mapped descendants."
            }
        }
    }

    return [PSCustomObject]@{
        OperationId = $operationId
        Placement = $headerFields[3]
        State = $footerState
        Records = $records.ToArray()
        RecordsByLogicalPath = $recordsByLogicalPath
    }
}

function Read-WindShareRestorationCheckpoint {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    try {
        $sidecarBytes = [IO.File]::ReadAllBytes($Path)
        $strictUtf8 = New-Object Text.UTF8Encoding($false, $true)
        $text = $strictUtf8.GetString($sidecarBytes)
    } catch {
        throw "WindShare could not read the sidecar as strict UTF-8: '$Path'."
    }

    $rawLines = @($text.Split([char]"`n"))
    $completeLineCount = $rawLines.Count
    if (-not $text.EndsWith("`n", [StringComparison]::Ordinal)) {
        # A writer crash may leave a partial record. Only newline-terminated records
        # can participate in a checkpoint.
        $completeLineCount--
    } elseif ($completeLineCount -gt 0 -and $rawLines[$completeLineCount - 1] -eq '') {
        $completeLineCount--
    }

    $completeLines = New-Object 'Collections.Generic.List[string]'
    for ($lineIndex = 0; $lineIndex -lt $completeLineCount; $lineIndex++) {
        $line = $rawLines[$lineIndex]
        if ($line.EndsWith("`r", [StringComparison]::Ordinal)) {
            $line = $line.Substring(0, $line.Length - 1)
        }
        [void]$completeLines.Add($line)
    }

    $lastValidCheckpoint = $null
    $lastCandidateError = $null
    for ($lineIndex = 1; $lineIndex -lt $completeLines.Count; $lineIndex++) {
        if (-not $completeLines[$lineIndex].StartsWith("F`t", [StringComparison]::Ordinal)) {
            continue
        }

        try {
            $candidate = Read-WindShareCheckpointCandidate `
                -Lines ($completeLines.ToArray()) `
                -FooterIndex $lineIndex
            $lastValidCheckpoint = $candidate
        } catch {
            $lastCandidateError = $_.Exception.Message + ' at ' + $_.ScriptStackTrace
            # A later torn or malformed batch cannot invalidate an earlier closed
            # checkpoint; without a valid later footer, none of that tail executes.
        }
    }

    if ($null -eq $lastValidCheckpoint) {
        throw "WindShare sidecar contains no structurally valid complete checkpoint. Last candidate: $lastCandidateError"
    }
    return $lastValidCheckpoint
}

function Test-WindSharePathPresent {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    return Test-Path -LiteralPath $Path
}

function Assert-WindShareTraversableDirectory {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$LogicalPath
    )

    $item = Get-Item -LiteralPath $Path -Force
    if (-not $item.PSIsContainer) {
        throw "WindShare restoration ancestor '$LogicalPath' is not a directory."
    }
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "WindShare restoration refuses reparse-point ancestor '$LogicalPath'."
    }
}

function Resolve-WindShareRestorationParent {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$AnchorPath,

        [Parameter(Mandatory = $true)]
        [object]$Record,

        [Parameter(Mandatory = $true)]
        [hashtable]$RecordsByLogicalPath
    )

    $parentPath = $AnchorPath
    $relativeParts = New-Object 'Collections.Generic.List[string]'

    for ($partIndex = 0; $partIndex -lt ($Record.LogicalParts.Count - 1); $partIndex++) {
        $logicalComponent = $Record.LogicalParts[$partIndex]
        [void]$relativeParts.Add($logicalComponent)
        $logicalAncestorPath = [string]::Join('/', $relativeParts.ToArray())
        $logicalCandidate = Join-Path $parentPath $logicalComponent

        if ($RecordsByLogicalPath.ContainsKey($logicalAncestorPath)) {
            $ancestorRecord = $RecordsByLogicalPath[$logicalAncestorPath]
            if ($ancestorRecord.Kind -cne 'directory') {
                throw "WindShare restoration ancestor '$logicalAncestorPath' is not mapped as a directory."
            }

            $physicalCandidate = Join-Path `
                $parentPath `
                $ancestorRecord.PhysicalComponent
            $physicalPresent = Test-WindSharePathPresent -Path $physicalCandidate
            $logicalPresent = Test-WindSharePathPresent -Path $logicalCandidate

            if ($physicalPresent -and $logicalPresent) {
                throw "WindShare restoration conflict at mapped ancestor '$logicalAncestorPath': both names exist."
            }
            if (-not $physicalPresent -and -not $logicalPresent) {
                throw "WindShare restoration is missing mapped ancestor '$logicalAncestorPath'."
            }
            $parentPath = if ($physicalPresent) {
                $physicalCandidate
            } else {
                $logicalCandidate
            }
        } else {
            if (-not (Test-WindSharePathPresent -Path $logicalCandidate)) {
                throw "WindShare restoration is missing ancestor '$logicalAncestorPath'."
            }
            $parentPath = $logicalCandidate
        }

        Assert-WindShareTraversableDirectory `
            -Path $parentPath `
            -LogicalPath $logicalAncestorPath
    }

    return $parentPath
}

function Invoke-WindShareRestorationRecord {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$AnchorPath,

        [Parameter(Mandatory = $true)]
        [object]$Record,

        [Parameter(Mandatory = $true)]
        [hashtable]$RecordsByLogicalPath
    )

    $parentPath = Resolve-WindShareRestorationParent `
        -AnchorPath $AnchorPath `
        -Record $Record `
        -RecordsByLogicalPath $RecordsByLogicalPath
    $logicalLeaf = $Record.LogicalParts[$Record.LogicalParts.Count - 1]
    $sourcePath = Join-Path $parentPath $Record.PhysicalComponent
    $targetPath = Join-Path $parentPath $logicalLeaf
    $sourcePresent = Test-WindSharePathPresent -Path $sourcePath
    $targetPresent = Test-WindSharePathPresent -Path $targetPath

    if ($sourcePresent -and -not $targetPresent) {
        $sourceItem = Get-Item -LiteralPath $sourcePath -Force
        if (
            ($Record.Kind -ceq 'directory' -and -not $sourceItem.PSIsContainer) -or
            ($Record.Kind -ceq 'file' -and $sourceItem.PSIsContainer)
        ) {
            throw "WindShare restoration source '$($Record.LogicalPath)' has the wrong entry kind."
        }
        if (
            $sourceItem.PSIsContainer -and
            (($sourceItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)
        ) {
            throw "WindShare restoration refuses reparse-point source '$($Record.LogicalPath)'."
        }

        Invoke-WindShareNoReplaceMove `
            -SourcePath $sourcePath `
            -TargetPath $targetPath
        Write-Output "restored: $($Record.LogicalPath)"
        return
    }

    if (-not $sourcePresent -and $targetPresent) {
        Write-Output "already restored: $($Record.LogicalPath)"
        return
    }

    if ($sourcePresent -and $targetPresent) {
        throw "WindShare restoration conflict at '$($Record.LogicalPath)': both names exist."
    }
    throw "WindShare restoration is missing both names for '$($Record.LogicalPath)'."
}

function Invoke-WindShareRestoration {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    if ([string]::IsNullOrWhiteSpace($Path)) {
        throw 'WindShare restoration requires -SidecarPath.'
    }

    $scriptDirectory = [IO.Path]::GetFullPath((Split-Path -Parent $PSCommandPath))
    $sidecarFullPath = [IO.Path]::GetFullPath($Path)
    $sidecarDirectory = [IO.Path]::GetDirectoryName($sidecarFullPath)
    if (-not [string]::Equals(
        $scriptDirectory,
        $sidecarDirectory,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'WindShare restoration requires the sidecar to be adjacent to this script.'
    }

    $checkpoint = Read-WindShareRestorationCheckpoint -Path $sidecarFullPath
    if ($checkpoint.State -ceq 'active') {
        $confirmation = Read-Host (
            'This checkpoint is active. Confirm WindShare is no longer receiving ' +
            'and will not resume this output by typing RESTORE'
        )
        if ($confirmation -cne 'RESTORE') {
            throw 'WindShare restoration was cancelled because the active checkpoint was not confirmed.'
        }
    }

    $orderedRecords = @($checkpoint.Records | Sort-Object `
        @{ Expression = { $_.Depth }; Descending = $true }, `
        @{ Expression = { $_.Ordinal }; Descending = $true })
    foreach ($record in $orderedRecords) {
        Invoke-WindShareRestorationRecord `
            -AnchorPath $scriptDirectory `
            -Record $record `
            -RecordsByLogicalPath $checkpoint.RecordsByLogicalPath
    }

    Write-Output (
        "WindShare restoration checkpoint '$($checkpoint.State)' completed. " +
        'Rerunning this script is safe; you may delete the script and sidecar manually.'
    )
}

# Dot-sourcing exposes Invoke-WindShareNoReplaceMove and the parser/state-machine
# functions so the matching Windows host contract exercises this exact asset.
if ($MyInvocation.InvocationName -ne '.') {
    if ([string]::IsNullOrWhiteSpace($SidecarPath)) {
        throw 'Usage: powershell.exe -NoProfile -ExecutionPolicy Bypass -File <script> -SidecarPath <adjacent-sidecar>'
    }
    Invoke-WindShareRestoration -Path $SidecarPath
}
