Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:RequiredWindowsNativeTests = @(
    'TestWindowsNTFSNativeCertification',
    'TestWindowsNTFSProcessRestartRecovery',
    'TestWindowsNTFSProbeMutexIsProcessExclusiveAndRecoversAbandonment'
)
$script:AdministratorsSID = 'S-1-5-32-544'
$script:UsersSID = 'S-1-5-32-545'
$script:SystemSID = 'S-1-5-18'
$script:CoordinatorReleaseRootLeafPattern = '^windshare-core-release-[0-9a-f]{32}$'
$script:WindowsNativeMutationDenyRights = `
    [Security.AccessControl.FileSystemRights]::WriteData -bor `
    [Security.AccessControl.FileSystemRights]::AppendData -bor `
    [Security.AccessControl.FileSystemRights]::WriteExtendedAttributes -bor `
    [Security.AccessControl.FileSystemRights]::WriteAttributes -bor `
    [Security.AccessControl.FileSystemRights]::Delete -bor `
    [Security.AccessControl.FileSystemRights]::DeleteSubdirectoriesAndFiles -bor `
    [Security.AccessControl.FileSystemRights]::ChangePermissions -bor `
    [Security.AccessControl.FileSystemRights]::TakeOwnership
$script:ForbiddenServiceSIDs = @(
    'S-1-5-18',
    'S-1-5-19',
    'S-1-5-20'
)
# CreateProcessWithLogonW limits lpCommandLine to 1024 UTF-16 characters.
# Reserve one character for its terminating NUL so every accepted launch is
# inside the documented boundary rather than relying on edge interpretation.
$script:MaximumCredentialCommandLineCharacters = 1023
$script:WindowsNativeSIDPattern = '^S-1-(?:[0-9]+-)+[0-9]+$'
$script:Win32ErrorFileNotFound = 2
$script:Win32ErrorPathNotFound = 3
$script:Win32ErrorSharingViolation = 32
$script:Win32ErrorLockViolation = 33
$script:Win32ErrorBusy = 170
$script:WindowsNativeProfileAbsentErrors = @(
    $script:Win32ErrorFileNotFound,
    $script:Win32ErrorPathNotFound
)
$script:WindowsNativeProfileRetryableErrors = @(
    $script:Win32ErrorSharingViolation,
    $script:Win32ErrorLockViolation,
    $script:Win32ErrorBusy
)

# Profile cleanup is a lifecycle obligation for the one account this gate
# creates, not host-management telemetry. Calling Userenv directly keeps WBEM
# availability and policy outside the certification verdict.
function Initialize-WindowsNativeUserProfileInterop {
    if ($null -eq ('WindShare.CoreRelease.NativeUserProfile' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

namespace WindShare.CoreRelease
{
    public static class NativeUserProfile
    {
        private const int ErrorGenFailure = 31;

        [DllImport("userenv.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool DeleteProfileW(
            string sid,
            string profilePath,
            string computerName);

        public static int Delete(string sid, string profilePath)
        {
            if (DeleteProfileW(sid, profilePath, null))
            {
                return 0;
            }
            int error = Marshal.GetLastWin32Error();
            return error == 0 ? ErrorGenFailure : error;
        }
    }
}
'@
    }
}

function Get-WindowsNativeEphemeralUserProfileRegistration([string]$UserSID) {
    $profileList = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey(
        'SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList',
        $false
    )
    if ($null -eq $profileList) {
        throw 'Windows profile registry root is unavailable'
    }
    try {
        $profileKey = $profileList.OpenSubKey($UserSID, $false)
        if ($null -eq $profileKey) {
            return [pscustomobject]@{ Registered = $false; Path = '' }
        }
        try {
            $rawPath = [string]$profileKey.GetValue(
                'ProfileImagePath',
                $null,
                [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
            )
            if ([string]::IsNullOrWhiteSpace($rawPath)) {
                throw "ephemeral profile $UserSID has no registered image path"
            }
            return [pscustomobject]@{
                Registered = $true
                Path = [IO.Path]::GetFullPath(
                    [Environment]::ExpandEnvironmentVariables($rawPath)
                )
            }
        } finally {
            $profileKey.Dispose()
        }
    } finally {
        $profileList.Dispose()
    }
}

function Invoke-WindowsNativeProfileDelete([string]$UserSID, [string]$ProfilePath) {
    Initialize-WindowsNativeUserProfileInterop
    $nativeProfilePath = if ([string]::IsNullOrWhiteSpace($ProfilePath)) {
        $null
    } else {
        $ProfilePath
    }
    return [WindShare.CoreRelease.NativeUserProfile]::Delete($UserSID, $nativeProfilePath)
}

function Remove-WindowsNativeEphemeralUserProfile {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$UserSID,

        [Parameter(Mandatory)]
        [ValidateRange(0, [int]::MaxValue)]
        [int]$TimeoutMilliseconds,

        [Parameter(Mandatory)]
        [ValidateRange(1, [int]::MaxValue)]
        [int]$PollMilliseconds,

        [scriptblock]$DeleteAttempt,

        [scriptblock]$Delay
    )

    if ($UserSID -cnotmatch $script:WindowsNativeSIDPattern) {
        throw "invalid ephemeral Windows user SID: $UserSID"
    }
    if ($null -eq $DeleteAttempt) {
        $DeleteAttempt = {
            param([string]$SID, [string]$Path)
            Invoke-WindowsNativeProfileDelete -UserSID $SID -ProfilePath $Path
        }
    }
    if ($null -eq $Delay) {
        $Delay = {
            param([int]$Milliseconds)
            Start-Sleep -Milliseconds $Milliseconds
        }
    }

    $registration = Get-WindowsNativeEphemeralUserProfileRegistration -UserSID $UserSID
    $stopwatch = [Diagnostics.Stopwatch]::StartNew()
    while ($true) {
        $errorCode = [int](& $DeleteAttempt $UserSID ([string]$registration.Path))
        if ($errorCode -eq 0 -or $errorCode -in $script:WindowsNativeProfileAbsentErrors) {
            break
        }
        if ($errorCode -notin $script:WindowsNativeProfileRetryableErrors) {
            throw "delete ephemeral Windows user profile failed with Win32 error $errorCode"
        }
        if ($stopwatch.ElapsedMilliseconds -ge $TimeoutMilliseconds) {
            throw "ephemeral Windows user profile remained busy for $TimeoutMilliseconds milliseconds (Win32 error $errorCode)"
        }
        [void](& $Delay $PollMilliseconds)
    }

    $remaining = Get-WindowsNativeEphemeralUserProfileRegistration -UserSID $UserSID
    if ($remaining.Registered) {
        throw 'ephemeral Windows user profile remains registered after deletion'
    }
    if ($registration.Registered -and (Test-Path -LiteralPath $registration.Path)) {
        throw "ephemeral Windows user profile directory remains after deletion: $($registration.Path)"
    }
}

# DirectorySecurity.Create does not report whether a directory already existed.
# The native release root must instead be born with its final protected DACL and
# fail on a name collision, so the coordinator never hardens an attacker-created
# object after the fact.
function Initialize-WindowsNativeDirectoryInterop {
    if ($null -eq ('WindShare.CoreRelease.NativeDirectory' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

namespace WindShare.CoreRelease
{
    public static class NativeDirectory
    {
        [StructLayout(LayoutKind.Sequential)]
        private struct SecurityAttributes
        {
            public int Length;
            public IntPtr SecurityDescriptor;
            [MarshalAs(UnmanagedType.Bool)]
            public bool InheritHandle;
        }

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool CreateDirectoryW(
            string path,
            ref SecurityAttributes securityAttributes);

        public static int CreateExclusive(string path, byte[] securityDescriptor)
        {
            if (String.IsNullOrWhiteSpace(path) ||
                securityDescriptor == null ||
                securityDescriptor.Length == 0)
            {
                return 87; // ERROR_INVALID_PARAMETER
            }

            GCHandle pinnedDescriptor = GCHandle.Alloc(
                securityDescriptor,
                GCHandleType.Pinned);
            try
            {
                SecurityAttributes attributes = new SecurityAttributes
                {
                    Length = Marshal.SizeOf<SecurityAttributes>(),
                    SecurityDescriptor = pinnedDescriptor.AddrOfPinnedObject(),
                    InheritHandle = false
                };
                if (CreateDirectoryW(path, ref attributes))
                {
                    return 0;
                }
                return Marshal.GetLastWin32Error();
            }
            finally
            {
                pinnedDescriptor.Free();
            }
        }
    }
}
'@
    }
}

function Resolve-WindowsNativeLocalFixedNTFSDirectoryLocation {
    [CmdletBinding()]
    [OutputType([string])]
    param(
        [Parameter(Mandatory)]
        [string]$Path,

        [Parameter(Mandatory)]
        [string]$Label
    )

    if (-not [IO.Path]::IsPathFullyQualified($Path)) {
        throw "$Label must be an absolute path"
    }
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "$Label does not exist or is not a directory"
    }
    $resolvedPath = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $Path).Path)
    $volumeRoot = [IO.Path]::GetPathRoot($resolvedPath)
    if ([string]::IsNullOrWhiteSpace($volumeRoot)) {
        throw "$Label has no volume root"
    }
    $drive = [IO.DriveInfo]::new($volumeRoot)
    if (-not $drive.IsReady -or $drive.DriveType -ne [IO.DriveType]::Fixed) {
        throw "$Label must be on a ready local fixed drive"
    }
    if (-not [string]::Equals($drive.DriveFormat, 'NTFS', [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label must be on NTFS, found $($drive.DriveFormat)"
    }

    return $resolvedPath
}

function Resolve-WindowsNativeLocalFixedNTFSDirectory {
    [CmdletBinding()]
    [OutputType([string])]
    param(
        [Parameter(Mandatory)]
        [string]$Path,

        [Parameter(Mandatory)]
        [string]$Label
    )

    $resolvedPath = Resolve-WindowsNativeLocalFixedNTFSDirectoryLocation `
        -Path $Path `
        -Label $Label
    $currentDirectory = [IO.DirectoryInfo]::new($resolvedPath)
    while ($null -ne $currentDirectory) {
        $attributes = [IO.File]::GetAttributes($currentDirectory.FullName)
        if (($attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "$Label ancestry contains a reparse point: $($currentDirectory.FullName)"
        }
        $currentDirectory = $currentDirectory.Parent
    }
    return $resolvedPath
}

function Assert-WindowsNativeTreeHasNoReparsePoints {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$RootPath,

        [Parameter(Mandatory)]
        [string]$Label
    )

    $resolvedRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path $RootPath `
        -Label $Label
    foreach ($entry in Get-ChildItem -LiteralPath $resolvedRoot -Force -Recurse) {
        if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "$Label contains a reparse point: $($entry.FullName)"
        }
    }
}

function Get-WindowsNativeFileSystemAccessControl([IO.FileSystemInfo]$Entry) {
    if ($Entry -is [IO.DirectoryInfo]) {
        return [IO.FileSystemAclExtensions]::GetAccessControl([IO.DirectoryInfo]$Entry)
    }
    if ($Entry -is [IO.FileInfo]) {
        return [IO.FileSystemAclExtensions]::GetAccessControl([IO.FileInfo]$Entry)
    }
    throw "native immutable-tree entry has an unsupported type: $($Entry.GetType().FullName)"
}

function Set-WindowsNativeFileSystemAccessControl(
    [IO.FileSystemInfo]$Entry,
    [Security.AccessControl.FileSystemSecurity]$Security
) {
    if ($Entry -is [IO.DirectoryInfo]) {
        [IO.FileSystemAclExtensions]::SetAccessControl(
            [IO.DirectoryInfo]$Entry,
            [Security.AccessControl.DirectorySecurity]$Security
        )
        return
    }
    if ($Entry -is [IO.FileInfo]) {
        [IO.FileSystemAclExtensions]::SetAccessControl(
            [IO.FileInfo]$Entry,
            [Security.AccessControl.FileSecurity]$Security
        )
        return
    }
    throw "native immutable-tree entry has an unsupported type: $($Entry.GetType().FullName)"
}

function Assert-WindowsNativeStoredMutationDeny(
    [IO.FileSystemInfo]$Entry,
    [Security.Principal.SecurityIdentifier]$UserSID
) {
    $security = Get-WindowsNativeFileSystemAccessControl -Entry $Entry
    $denyRules = @($security.GetAccessRules(
        $true,
        $false,
        [Security.Principal.SecurityIdentifier]
    ) | Where-Object {
        $_.IdentityReference.Value -ceq $UserSID.Value -and
            $_.AccessControlType -eq [Security.AccessControl.AccessControlType]::Deny
    })
    if ($denyRules.Count -ne 1) {
        throw "native immutable-tree entry has $($denyRules.Count) direct mutation-deny rules: $($Entry.FullName)"
    }

    $denyRule = $denyRules[0]
    if ([int64]$denyRule.FileSystemRights -ne
        [int64]$script:WindowsNativeMutationDenyRights -or
        $denyRule.IsInherited -or
        $denyRule.InheritanceFlags -ne [Security.AccessControl.InheritanceFlags]::None -or
        $denyRule.PropagationFlags -ne [Security.AccessControl.PropagationFlags]::None) {
        throw "native immutable-tree entry did not persist the exact direct mutation-deny rule: $($Entry.FullName)"
    }
}

function Set-WindowsNativeTreeMutationDeny {
    [CmdletBinding()]
    [OutputType([object])]
    param(
        [Parameter(Mandatory)]
        [string]$RootPath,

        [Parameter(Mandatory)]
        [string]$UserSID,

        [Parameter(Mandatory)]
        [string]$Label
    )

    $resolvedRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path $RootPath `
        -Label $Label
    Assert-WindowsNativeTreeHasNoReparsePoints `
        -RootPath $resolvedRoot `
        -Label $Label
    $sid = [Security.Principal.SecurityIdentifier]::new($UserSID)
    $denyRule = [Security.AccessControl.FileSystemAccessRule]::new(
        $sid,
        $script:WindowsNativeMutationDenyRights,
        [Security.AccessControl.InheritanceFlags]::None,
        [Security.AccessControl.PropagationFlags]::None,
        [Security.AccessControl.AccessControlType]::Deny
    )
    if ([int64]$denyRule.FileSystemRights -ne
        [int64]$script:WindowsNativeMutationDenyRights) {
        throw 'Windows normalized the mutation-deny rule to a broader access mask'
    }

    $entries = [Collections.Generic.List[IO.FileSystemInfo]]::new()
    foreach ($entry in Get-ChildItem -LiteralPath $resolvedRoot -Force -Recurse) {
        if ($entry -isnot [IO.FileSystemInfo]) {
            throw "$Label contains a non-filesystem entry"
        }
        $entries.Add($entry)
    }
    # Direct non-inheriting rules make every current input independent of copied
    # inheritance state without denying SYNCHRONIZE and breaking read handles.
    $entries.Add([IO.DirectoryInfo]::new($resolvedRoot))
    foreach ($entry in $entries) {
        $security = Get-WindowsNativeFileSystemAccessControl -Entry $entry
        $security.SetAccessRule($denyRule)
        Set-WindowsNativeFileSystemAccessControl -Entry $entry -Security $security
        Assert-WindowsNativeStoredMutationDeny -Entry $entry -UserSID $sid
    }

    return [pscustomobject]@{
        PSTypeName = 'WindShare.WindowsNativeTreeMutationDeny'
        RootPath = $resolvedRoot
        EntryCount = $entries.Count
        DeniedRights = [int64]$script:WindowsNativeMutationDenyRights
    }
}

function Select-WindowsNativeCoordinatorGoApplication {
    [CmdletBinding()]
    [OutputType([object])]
    param(
        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [object[]]$Candidates
    )

    if ($Candidates.Count -eq 0) {
        throw 'no Go application command is available on PATH'
    }
    $selected = $Candidates[0]
    if ($selected -isnot [Management.Automation.ApplicationInfo]) {
        throw 'first Go command candidate is not an ApplicationInfo'
    }
    $selectedPath = [string]$selected.Path
    if ([string]::IsNullOrWhiteSpace($selectedPath) -or
        -not [IO.Path]::IsPathFullyQualified($selectedPath)) {
        throw 'first Go application candidate has no absolute executable path'
    }
    return [pscustomobject]@{
        CandidateCount = $Candidates.Count
        GoExecutable = [IO.Path]::GetFullPath($selectedPath)
    }
}

function Get-WindowsNativeCoordinatorGoToolchain {
    [CmdletBinding()]
    [OutputType([object])]
    param()

    # Get-Command may return every matching ApplicationInfo even without -All.
    # Select the first explicit PATH-ordered candidate, which is the application
    # PowerShell itself invokes for the bare `go` command.
    $applicationCandidates = @(
        Get-Command go -CommandType Application -All -ErrorAction Stop
    )
    $selection = Select-WindowsNativeCoordinatorGoApplication `
        -Candidates $applicationCandidates
    $resolvedGoExecutable = $selection.GoExecutable
    if (-not (Test-Path -LiteralPath $resolvedGoExecutable -PathType Leaf)) {
        throw 'selected coordinator Go executable is not an existing file'
    }
    $goRootOutput = @(& $resolvedGoExecutable env GOROOT)
    $goRootExitCode = $LASTEXITCODE
    if ($goRootExitCode -ne 0) {
        throw "resolve coordinator GOROOT failed with code $goRootExitCode"
    }
    $goRootValues = @($goRootOutput | ForEach-Object {
        ([string]$_).Trim()
    } | Where-Object {
        -not [string]::IsNullOrWhiteSpace($_)
    })
    if ($goRootValues.Count -ne 1) {
        throw "coordinator Go executable reported $($goRootValues.Count) GOROOT values, want exactly one"
    }

    # Hosted setup-go installations may themselves sit behind a trusted cache
    # junction. Only the copied destination is part of the worker boundary.
    $resolvedGoRoot = Resolve-WindowsNativeLocalFixedNTFSDirectoryLocation `
        -Path $goRootValues[0] `
        -Label 'coordinator GOROOT'
    $expectedGoExecutable = Join-Path $resolvedGoRoot 'bin\go.exe'
    if (-not (Test-Path -LiteralPath $expectedGoExecutable -PathType Leaf)) {
        throw 'coordinator GOROOT has no bin\go.exe'
    }
    $resolvedExpectedGoExecutable = [IO.Path]::GetFullPath(
        (Resolve-Path -LiteralPath $expectedGoExecutable).Path
    )
    if (-not [string]::Equals(
        $resolvedGoExecutable,
        $resolvedExpectedGoExecutable,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'coordinator Go executable is not the bin\go.exe reported by its GOROOT'
    }

    $goVersionOutput = @(& $resolvedGoExecutable version)
    $goVersionExitCode = $LASTEXITCODE
    $goVersionValues = @($goVersionOutput | ForEach-Object {
        ([string]$_).Trim()
    } | Where-Object {
        -not [string]::IsNullOrWhiteSpace($_)
    })
    if ($goVersionExitCode -ne 0 -or $goVersionValues.Count -ne 1) {
        throw "resolve coordinator Go version failed with code $goVersionExitCode"
    }

    return [pscustomobject]@{
        PSTypeName = 'WindShare.WindowsNativeCoordinatorGoToolchain'
        CandidateCount = $selection.CandidateCount
        SelectedVersion = $goVersionValues[0]
        GoRoot = $resolvedGoRoot
        GoExecutable = $resolvedGoExecutable
    }
}

function Copy-WindowsNativeGoToolchain {
    [CmdletBinding()]
    [OutputType([object])]
    param(
        [Parameter(Mandatory)]
        [object]$Toolchain,

        [Parameter(Mandatory)]
        [string]$DestinationRoot
    )

    $sourceRoot = Resolve-WindowsNativeLocalFixedNTFSDirectoryLocation `
        -Path ([string]$Toolchain.GoRoot) `
        -Label 'coordinator GOROOT'
    $sourceGoExecutable = [IO.Path]::GetFullPath(
        (Resolve-Path -LiteralPath ([string]$Toolchain.GoExecutable)).Path
    )
    $expectedSourceGoExecutable = [IO.Path]::GetFullPath(
        (Resolve-Path -LiteralPath (Join-Path $sourceRoot 'bin\go.exe')).Path
    )
    if (-not [string]::Equals(
        $sourceGoExecutable,
        $expectedSourceGoExecutable,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'coordinator toolchain executable does not belong to its GOROOT'
    }
    if (-not [IO.Path]::IsPathFullyQualified($DestinationRoot)) {
        throw 'staged GOROOT must be an absolute path'
    }
    $resolvedDestinationRoot = [IO.Path]::GetFullPath($DestinationRoot)
    $destinationParentInfo = [IO.Directory]::GetParent($resolvedDestinationRoot)
    if ($null -eq $destinationParentInfo) {
        throw 'staged GOROOT has no parent directory'
    }
    $destinationParent = Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path $destinationParentInfo.FullName `
        -Label 'staged GOROOT parent'
    if (-not [string]::Equals(
        $destinationParent,
        $destinationParentInfo.FullName,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'staged GOROOT parent does not resolve to its canonical path'
    }
    $directorySeparator = [IO.Path]::DirectorySeparatorChar
    if ([string]::Equals(
        $sourceRoot,
        $resolvedDestinationRoot,
        [StringComparison]::OrdinalIgnoreCase
    ) -or $resolvedDestinationRoot.StartsWith(
        $sourceRoot + $directorySeparator,
        [StringComparison]::OrdinalIgnoreCase
    ) -or $sourceRoot.StartsWith(
        $resolvedDestinationRoot + $directorySeparator,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'coordinator and staged GOROOT paths must be disjoint'
    }
    if (Test-Path -LiteralPath $resolvedDestinationRoot) {
        throw 'staged GOROOT destination already exists'
    }

    # Copying into the already protected release root gives every toolchain file
    # the release boundary's DACL without weakening the setup-go toolcache.
    Copy-Item `
        -LiteralPath $sourceRoot `
        -Destination $resolvedDestinationRoot `
        -Recurse `
        -Force
    $stagedGoRoot = Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path $resolvedDestinationRoot `
        -Label 'staged GOROOT'
    Assert-WindowsNativeTreeHasNoReparsePoints `
        -RootPath $stagedGoRoot `
        -Label 'staged GOROOT'
    $stagedGoExecutable = Join-Path $stagedGoRoot 'bin\go.exe'
    if (-not (Test-Path -LiteralPath $stagedGoExecutable -PathType Leaf)) {
        throw 'staged GOROOT has no bin\go.exe'
    }
    $sourceGoLength = ([IO.FileInfo]::new($sourceGoExecutable)).Length
    $stagedGoLength = ([IO.FileInfo]::new($stagedGoExecutable)).Length
    if ($sourceGoLength -ne $stagedGoLength) {
        throw 'staged Go executable length differs from the coordinator executable'
    }

    return [pscustomobject]@{
        PSTypeName = 'WindShare.WindowsNativeStagedGoToolchain'
        GoRoot = $stagedGoRoot
        GoExecutable = [IO.Path]::GetFullPath($stagedGoExecutable)
    }
}

function Get-WindowsNativeCommonApplicationDataRoot {
    [CmdletBinding()]
    [OutputType([string])]
    param()

    $commonApplicationData = [Environment]::GetFolderPath(
        [Environment+SpecialFolder]::CommonApplicationData
    )
    if ([string]::IsNullOrWhiteSpace($commonApplicationData)) {
        throw 'Windows did not resolve CommonApplicationData'
    }
    return Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path $commonApplicationData `
        -Label 'canonical CommonApplicationData root'
}

function New-WindowsNativeCoordinatorDirectorySecurity(
    [Security.Principal.SecurityIdentifier]$CoordinatorSID
) {
    $security = [Security.AccessControl.DirectorySecurity]::new()
    $security.SetOwner($CoordinatorSID)
    $security.SetAccessRuleProtection($true, $false)
    $inheritanceFlags = [Security.AccessControl.InheritanceFlags]::ContainerInherit -bor `
        [Security.AccessControl.InheritanceFlags]::ObjectInherit
    foreach ($sidValue in @(
        $CoordinatorSID.Value,
        $script:SystemSID,
        $script:AdministratorsSID
    ) | Select-Object -Unique) {
        $rule = [Security.AccessControl.FileSystemAccessRule]::new(
            [Security.Principal.SecurityIdentifier]::new($sidValue),
            [Security.AccessControl.FileSystemRights]::FullControl,
            $inheritanceFlags,
            [Security.AccessControl.PropagationFlags]::None,
            [Security.AccessControl.AccessControlType]::Allow
        )
        [void]$security.AddAccessRule($rule)
    }
    return $security
}

function Assert-WindowsNativeCoordinatorReleaseRoot {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [object]$Ownership,

        [switch]$RequireEmpty
    )

    $basePath = Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path ([string]$Ownership.BasePath) `
        -Label 'native release root parent'
    $rootPath = Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path ([string]$Ownership.RootPath) `
        -Label 'native release root'
    if (-not [string]::Equals(
        $basePath,
        [IO.Path]::GetFullPath([string]$Ownership.BasePath),
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'native release root parent no longer resolves to its canonical path'
    }
    if (-not [string]::Equals(
        $rootPath,
        [IO.Path]::GetFullPath([string]$Ownership.RootPath),
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'native release root no longer resolves to its owned path'
    }

    $rootDirectory = [IO.DirectoryInfo]::new($rootPath)
    if ($null -eq $rootDirectory.Parent -or
        -not [string]::Equals(
            $rootDirectory.Parent.FullName,
            $basePath,
            [StringComparison]::OrdinalIgnoreCase
        )) {
        throw 'native release root is not a direct child of its canonical parent'
    }
    if ($rootDirectory.Name -cne [string]$Ownership.LeafName -or
        $rootDirectory.Name -cnotmatch $script:CoordinatorReleaseRootLeafPattern) {
        throw 'native release root has an unexpected random leaf name'
    }

    $coordinatorSID = [string]$Ownership.CoordinatorSID
    $acl = [IO.FileSystemAclExtensions]::GetAccessControl($rootDirectory)
    $ownerSID = $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value
    if ($ownerSID -cne $coordinatorSID) {
        throw "native release root owner is $ownerSID, want $coordinatorSID"
    }
    if (-not $acl.AreAccessRulesProtected) {
        throw 'native release root DACL is not protected'
    }

    $expectedRights = @{}
    foreach ($sidValue in @(
        $coordinatorSID,
        $script:SystemSID,
        $script:AdministratorsSID
    ) | Select-Object -Unique) {
        $expectedRights[$sidValue] = [Security.AccessControl.FileSystemRights]::FullControl
    }
    $workerSIDProperty = $Ownership.PSObject.Properties['WorkerSID']
    if ($null -ne $workerSIDProperty -and
        -not [string]::IsNullOrWhiteSpace([string]$workerSIDProperty.Value)) {
        $expectedRights[[string]$workerSIDProperty.Value] = `
            [Security.AccessControl.FileSystemRights]::ReadAndExecute -bor `
            [Security.AccessControl.FileSystemRights]::Synchronize
    }

    $rules = @($acl.GetAccessRules(
        $true,
        $true,
        [Security.Principal.SecurityIdentifier]
    ))
    if ($rules.Count -ne $expectedRights.Count) {
        throw "native release root has $($rules.Count) access rules, want $($expectedRights.Count)"
    }
    $requiredInheritance = [Security.AccessControl.InheritanceFlags]::ContainerInherit -bor `
        [Security.AccessControl.InheritanceFlags]::ObjectInherit
    foreach ($rule in $rules) {
        $ruleSID = $rule.IdentityReference.Value
        if (-not $expectedRights.ContainsKey($ruleSID) -or
            $rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
            $rule.IsInherited -or
            $rule.InheritanceFlags -ne $requiredInheritance -or
            $rule.PropagationFlags -ne [Security.AccessControl.PropagationFlags]::None -or
            [int64]$rule.FileSystemRights -ne [int64]$expectedRights[$ruleSID]) {
            throw "native release root has an unexpected access rule for $ruleSID"
        }
    }

    if ($RequireEmpty -and @(Get-ChildItem -LiteralPath $rootPath -Force).Count -ne 0) {
        throw 'new native release root is not empty'
    }
}

function New-WindowsNativeCoordinatorReleaseRootAt(
    [string]$BasePath,
    [string]$LeafName = ''
) {
    $resolvedBasePath = Resolve-WindowsNativeLocalFixedNTFSDirectory `
        -Path $BasePath `
        -Label 'native release root parent'
    $leafName = if ([string]::IsNullOrWhiteSpace($LeafName)) {
        'windshare-core-release-{0}' -f [Guid]::NewGuid().ToString('N')
    } else {
        $LeafName
    }
    if ($leafName -cnotmatch $script:CoordinatorReleaseRootLeafPattern) {
        throw 'native release root leaf name does not satisfy the ownership contract'
    }
    $rootPath = Join-Path $resolvedBasePath $leafName
    $coordinatorSID = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $security = New-WindowsNativeCoordinatorDirectorySecurity -CoordinatorSID $coordinatorSID
    $securityDescriptor = $security.GetSecurityDescriptorBinaryForm()
    Initialize-WindowsNativeDirectoryInterop
    $nativeError = [WindShare.CoreRelease.NativeDirectory]::CreateExclusive(
        $rootPath,
        $securityDescriptor
    )
    if ($nativeError -eq 183) {
        throw "native release root collision: $rootPath"
    }
    if ($nativeError -ne 0) {
        $nativeException = [ComponentModel.Win32Exception]::new($nativeError)
        throw "create protected native release root failed: $($nativeException.Message) ($nativeError)"
    }

    $ownership = [pscustomobject]@{
        PSTypeName = 'WindShare.WindowsNativeCoordinatorReleaseRoot'
        BasePath = $resolvedBasePath
        RootPath = $rootPath
        LeafName = $leafName
        CoordinatorSID = $coordinatorSID.Value
        WorkerSID = ''
    }
    try {
        Assert-WindowsNativeCoordinatorReleaseRoot -Ownership $ownership -RequireEmpty
    } catch {
        # CreateExclusive proves this exact empty object was created by this call;
        # avoid recursive cleanup if validation nevertheless detects corruption.
        if (Test-Path -LiteralPath $rootPath -PathType Container) {
            [IO.Directory]::Delete($rootPath, $false)
        }
        throw
    }
    return $ownership
}

function New-WindowsNativeCoordinatorReleaseRoot {
    [CmdletBinding()]
    [OutputType([object])]
    param()

    return New-WindowsNativeCoordinatorReleaseRootAt `
        -BasePath (Get-WindowsNativeCommonApplicationDataRoot)
}

function Remove-WindowsNativeCoordinatorReleaseRoot {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [object]$Ownership
    )

    if (-not (Test-Path -LiteralPath ([string]$Ownership.RootPath))) {
        return
    }
    Assert-WindowsNativeCoordinatorReleaseRoot -Ownership $Ownership
    Remove-Item -LiteralPath ([string]$Ownership.RootPath) -Recurse -Force
    if (Test-Path -LiteralPath ([string]$Ownership.RootPath)) {
        throw 'native release root still exists after cleanup'
    }
}

function ConvertTo-WindowsNativeArgument([string]$Value) {
    $argument = [Text.StringBuilder]::new($Value.Length + 2)
    [void]$argument.Append('"')
    $backslashCount = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -ceq [char]'\') {
            $backslashCount++
            continue
        }
        if ($character -ceq [char]'"') {
            if ($backslashCount -gt 0) {
                [void]$argument.Append(('\' * ($backslashCount * 2)))
                $backslashCount = 0
            }
            [void]$argument.Append('\"')
            continue
        }
        if ($backslashCount -gt 0) {
            [void]$argument.Append(('\' * $backslashCount))
            $backslashCount = 0
        }
        [void]$argument.Append($character)
    }
    # Backslashes before the closing quote must be doubled so Windows argv
    # parsing preserves a trailing directory separator instead of escaping it.
    if ($backslashCount -gt 0) {
        [void]$argument.Append(('\' * ($backslashCount * 2)))
    }
    [void]$argument.Append('"')
    return $argument.ToString()
}

function Assert-WindowsNativeWorkerArgumentValue([string]$Value, [string]$Label) {
    if ([string]::IsNullOrWhiteSpace($Value)) {
        throw "$Label is empty"
    }
    foreach ($character in $Value.ToCharArray()) {
        if ([char]::IsControl($character)) {
            throw "$Label contains a control character"
        }
    }
    # These inputs are filesystem paths or a SID, none of which can contain a
    # double quote on Windows. Rejecting one exposes malformed launch state
    # instead of allowing it to acquire command-line syntax accidentally.
    if ($Value.Contains('"', [StringComparison]::Ordinal)) {
        throw "$Label contains a double quote"
    }
}

function New-WindowsNativeWorkerArgumentLine {
    [CmdletBinding()]
    [OutputType([string])]
    param(
        [Parameter(Mandatory)]
        [string]$PowerShellExecutable,

        [Parameter(Mandatory)]
        [string]$WorkerScript,

        [Parameter(Mandatory)]
        [string]$ArtifactRoot,

        [Parameter(Mandatory)]
        [string]$WorkRoot,

        [Parameter(Mandatory)]
        [string]$GoExecutable,

        [Parameter(Mandatory)]
        [string]$ExpectedUserSID
    )

    $pathArguments = @(
        [pscustomobject]@{ Label = 'PowerShell executable'; Value = $PowerShellExecutable },
        [pscustomobject]@{ Label = 'native worker script'; Value = $WorkerScript },
        [pscustomobject]@{ Label = 'extracted artifact root'; Value = $ArtifactRoot },
        [pscustomobject]@{ Label = 'native worker root'; Value = $WorkRoot },
        [pscustomobject]@{ Label = 'Go executable'; Value = $GoExecutable }
    )
    foreach ($pathArgument in $pathArguments) {
        Assert-WindowsNativeWorkerArgumentValue `
            -Value $pathArgument.Value `
            -Label $pathArgument.Label
        if (-not [IO.Path]::IsPathFullyQualified($pathArgument.Value)) {
            throw "$($pathArgument.Label) must be an absolute path"
        }
    }
    Assert-WindowsNativeWorkerArgumentValue `
        -Value $ExpectedUserSID `
        -Label 'expected native worker SID'
    if ($ExpectedUserSID -cnotmatch '^S-[0-9]+(?:-[0-9]+)+$') {
        throw 'expected native worker SID is malformed'
    }

    $argumentLine = @(
        '-NoLogo',
        '-NoProfile',
        '-NonInteractive',
        '-ExecutionPolicy',
        'Bypass',
        '-File',
        (ConvertTo-WindowsNativeArgument $WorkerScript),
        '-ArtifactRoot',
        (ConvertTo-WindowsNativeArgument $ArtifactRoot),
        '-WorkRoot',
        (ConvertTo-WindowsNativeArgument $WorkRoot),
        '-GoExecutable',
        (ConvertTo-WindowsNativeArgument $GoExecutable),
        '-ExpectedUserSID',
        (ConvertTo-WindowsNativeArgument $ExpectedUserSID)
    ) -join ' '
    $fullCommandLine = '{0} {1}' -f @(
        (ConvertTo-WindowsNativeArgument $PowerShellExecutable),
        $argumentLine
    )
    if ($fullCommandLine.Length -gt $script:MaximumCredentialCommandLineCharacters) {
        throw ('standard-user native worker command line is {0} characters; ' +
            'CreateProcessWithLogonW permits at most {1} before its terminating NUL') -f @(
                $fullCommandLine.Length,
                $script:MaximumCredentialCommandLineCharacters
            )
    }
    return $argumentLine
}

function Get-WindowsNativeRequiredTestNames {
    return @($script:RequiredWindowsNativeTests)
}

function Get-WindowsNativeRequiredTestExpression {
    return '^({0})$' -f (($script:RequiredWindowsNativeTests | ForEach-Object {
        [Regex]::Escape($_)
    }) -join '|')
}

function Get-WindowsNativeEventField([object]$Event, [string]$Name) {
    $property = $Event.PSObject.Properties[$Name]
    if ($null -eq $property -or $null -eq $property.Value) {
        return ''
    }
    return [string]$property.Value
}

function Assert-WindowsNativeReadOnlyDirectory {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$Path,

        [Parameter(Mandatory)]
        [string]$Label
    )

    $probePath = Join-Path $Path ('write-denial-{0}.probe' -f [Guid]::NewGuid().ToString('N'))
    $probeStream = $null
    $creationDenied = $false
    try {
        try {
            $probeStream = [IO.File]::Open(
                $probePath,
                [IO.FileMode]::CreateNew,
                [IO.FileAccess]::Write,
                [IO.FileShare]::None
            )
        } catch [UnauthorizedAccessException] {
            $creationDenied = $true
        } catch {
            throw "$Label write-denial probe failed for an unexpected reason: $($_.Exception.Message)"
        }

        if ($creationDenied) {
            return
        }
        throw "$Label is writable by the native worker token"
    } finally {
        if ($null -ne $probeStream) {
            $probeStream.Dispose()
        }
        if (Test-Path -LiteralPath $probePath) {
            Remove-Item -LiteralPath $probePath -Force
        }
    }
}

function ConvertFrom-WindowsNativeTestJSONLines {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [AllowEmptyString()]
        [string[]]$Lines
    )

    $events = [Collections.Generic.List[object]]::new()
    for ($lineIndex = 0; $lineIndex -lt $Lines.Count; $lineIndex++) {
        if ([string]::IsNullOrWhiteSpace($Lines[$lineIndex])) {
            throw "required Windows/NTFS native tests emitted an empty JSON line at index $lineIndex"
        }
        try {
            $event = ConvertFrom-Json -InputObject $Lines[$lineIndex] -ErrorAction Stop
        } catch {
            throw "required Windows/NTFS native tests emitted invalid JSON at line index $lineIndex"
        }
        $events.Add($event)
    }
    return @($events)
}

function Assert-WindowsNativeStandardUserIdentity {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$ActualUserSID,

        [Parameter(Mandatory)]
        [string]$ExpectedUserSID,

        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [string[]]$GroupSIDs,

        [Parameter(Mandatory)]
        [bool]$IsAdministrator
    )

    if ($ActualUserSID -cne $ExpectedUserSID) {
        throw "native worker token SID $ActualUserSID does not match its ephemeral account SID"
    }
    if ($ActualUserSID -notmatch '^S-[0-9]+(?:-[0-9]+)+$') {
        throw 'native worker token has a malformed user SID'
    }
    if ($ActualUserSID -in $script:ForbiddenServiceSIDs -or $ActualUserSID -match '-500$') {
        throw 'native worker must not use a built-in administrator or service identity'
    }
    if ($IsAdministrator -or $GroupSIDs -contains $script:AdministratorsSID) {
        throw 'native worker token must not carry the built-in Administrators group'
    }
    if ($GroupSIDs -notcontains $script:UsersSID) {
        throw 'native worker token does not carry the built-in Users group'
    }
}

function Assert-WindowsNativeTestEvents {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [int]$ExitCode,

        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [object[]]$Events,

        [string[]]$RequiredTests = $script:RequiredWindowsNativeTests
    )

    if ($ExitCode -ne 0) {
        throw "required Windows/NTFS native tests exited with code $ExitCode"
    }
    if ($Events.Count -eq 0) {
        throw 'required Windows/NTFS native tests produced no JSON events'
    }

    $skippedEvents = @($Events | Where-Object {
        (Get-WindowsNativeEventField $_ 'Action') -ceq 'skip'
    })
    if ($skippedEvents.Count -ne 0) {
        $skipNames = @($skippedEvents | ForEach-Object {
            $testName = Get-WindowsNativeEventField $_ 'Test'
            $packageName = Get-WindowsNativeEventField $_ 'Package'
            if (-not [string]::IsNullOrWhiteSpace($testName)) {
                $testName
            } elseif (-not [string]::IsNullOrWhiteSpace($packageName)) {
                $packageName
            } else {
                '<unknown>'
            }
        })
        throw "required Windows/NTFS native test suite reported SKIP: $($skipNames -join ', ')"
    }

    $failedEvents = @($Events | Where-Object {
        (Get-WindowsNativeEventField $_ 'Action') -ceq 'fail'
    })
    if ($failedEvents.Count -ne 0) {
        $failureNames = @($failedEvents | ForEach-Object {
            $testName = Get-WindowsNativeEventField $_ 'Test'
            $packageName = Get-WindowsNativeEventField $_ 'Package'
            if (-not [string]::IsNullOrWhiteSpace($testName)) {
                $testName
            } elseif (-not [string]::IsNullOrWhiteSpace($packageName)) {
                $packageName
            } else {
                '<unknown>'
            }
        })
        throw "required Windows/NTFS native test suite reported FAIL: $($failureNames -join ', ')"
    }

    foreach ($requiredTest in $RequiredTests) {
        $topLevelRuns = @($Events | Where-Object {
            (Get-WindowsNativeEventField $_ 'Action') -ceq 'run' -and
                (Get-WindowsNativeEventField $_ 'Test') -ceq $requiredTest
        })
        if ($topLevelRuns.Count -ne 1) {
            throw "required native test reported $($topLevelRuns.Count) top-level RUN events, want exactly one: $requiredTest"
        }

        $topLevelPasses = @($Events | Where-Object {
            (Get-WindowsNativeEventField $_ 'Action') -ceq 'pass' -and
                (Get-WindowsNativeEventField $_ 'Test') -ceq $requiredTest
        })
        if ($topLevelPasses.Count -ne 1) {
            throw "required native test reported $($topLevelPasses.Count) top-level PASS events, want exactly one: $requiredTest"
        }
    }
}

Export-ModuleMember -Function @(
    'Assert-WindowsNativeCoordinatorReleaseRoot',
    'Assert-WindowsNativeReadOnlyDirectory',
    'Assert-WindowsNativeStandardUserIdentity',
    'Assert-WindowsNativeTestEvents',
    'Assert-WindowsNativeTreeHasNoReparsePoints',
    'ConvertFrom-WindowsNativeTestJSONLines',
    'Copy-WindowsNativeGoToolchain',
    'Get-WindowsNativeCoordinatorGoToolchain',
    'Get-WindowsNativeCommonApplicationDataRoot',
    'Get-WindowsNativeRequiredTestExpression',
    'Get-WindowsNativeRequiredTestNames',
    'New-WindowsNativeCoordinatorReleaseRoot',
    'New-WindowsNativeWorkerArgumentLine',
    'Remove-WindowsNativeEphemeralUserProfile',
    'Remove-WindowsNativeCoordinatorReleaseRoot',
    'Resolve-WindowsNativeLocalFixedNTFSDirectory',
    'Set-WindowsNativeTreeMutationDeny'
)
