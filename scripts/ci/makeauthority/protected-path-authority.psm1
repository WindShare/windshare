Set-StrictMode -Version Latest

$script:ProtectedPathAuthority = $null

# A retained file handle denies mutation and deletion between the launcher's
# identity check and the token-free completion consumer.
if ($null -eq ('WindShareMakeAuthorityNativePath' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;
using Microsoft.Win32.SafeHandles;

public static class WindShareMakeAuthorityNativePath
{
    private const uint FileReadAttributes = 0x00000080;
    private const uint FileShareRead = 0x00000001;
    private const uint OpenExisting = 3;
    private const uint FileFlagBackupSemantics = 0x02000000;
    private const uint FileFlagOpenReparsePoint = 0x00200000;

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern SafeFileHandle CreateFileW(
        string name,
        uint access,
        uint share,
        IntPtr securityAttributes,
        uint creationDisposition,
        uint flags,
        IntPtr template);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern uint GetFinalPathNameByHandleW(
        SafeFileHandle handle,
        StringBuilder path,
        uint pathLength,
        uint flags);

    public static SafeFileHandle OpenFile(string path)
    {
        SafeFileHandle handle = CreateFileW(
            path,
            FileReadAttributes,
            FileShareRead,
            IntPtr.Zero,
            OpenExisting,
            FileFlagOpenReparsePoint,
            IntPtr.Zero);
        if (handle.IsInvalid) {
            int error = Marshal.GetLastWin32Error();
            handle.Dispose();
            throw new Win32Exception(error, "protected completion authority could not be retained");
        }
        return handle;
    }

    public static string FinalPath(SafeFileHandle handle)
    {
        var buffer = new StringBuilder(32768);
        uint length = GetFinalPathNameByHandleW(handle, buffer, (uint)buffer.Capacity, 0);
        if (length == 0 || length >= buffer.Capacity) {
            throw new Win32Exception(Marshal.GetLastWin32Error(), "protected path identity could not be resolved");
        }
        return buffer.ToString();
    }
}
'@
}

function Enter-WindShareProtectedPathAuthority {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Completion
    )

    if ($null -ne $script:ProtectedPathAuthority) {
        throw 'WindShare protected path authority may be settled only once per process'
    }
    $completionPath = Resolve-WindShareAuthorityPath -Value $Completion -Label 'browser network completion' -PathType Leaf

    $completionStream = $null
    try {
        $completionStream = [IO.FileStream]::new(
            $completionPath,
            [IO.FileMode]::Open,
            [IO.FileAccess]::Read,
            [IO.FileShare]::Read
        )
        Assert-WindShareRetainedPath -Expected $completionPath -Handle $completionStream.SafeFileHandle -Label 'browser network completion'
        Assert-WindShareNoReparsePointInPath -Path $completionPath -Label 'browser network completion'
        $script:ProtectedPathAuthority = [pscustomobject]@{
            Completion = $completionPath
            CompletionStream = $completionStream
        }
        return $script:ProtectedPathAuthority
    } catch {
        if ($null -ne $completionStream) { $completionStream.Dispose() }
        throw
    }
}

function Exit-WindShareProtectedPathAuthority {
    if ($null -eq $script:ProtectedPathAuthority) { return }
    $script:ProtectedPathAuthority.CompletionStream.Dispose()
    $script:ProtectedPathAuthority = $null
}

function Resolve-WindShareAuthorityPath {
    param(
        [Parameter(Mandatory)][AllowEmptyString()][string]$Value,
        [Parameter(Mandatory)][string]$Label,
        [Parameter(Mandatory)][ValidateSet('Leaf', 'Container')][string]$PathType
    )

    if ([string]::IsNullOrWhiteSpace($Value) -or -not [IO.Path]::IsPathRooted($Value)) {
        throw "$Label must be an absolute canonical path"
    }
    $canonical = [IO.Path]::GetFullPath($Value)
    if (-not [StringComparer]::OrdinalIgnoreCase.Equals($canonical, $Value) -or
        -not (Test-Path -LiteralPath $canonical -PathType $PathType)) {
        throw "$Label must name an existing canonical $($PathType.ToLowerInvariant())"
    }
    Assert-WindShareNoReparsePointInPath -Path $canonical -Label $Label
    return $canonical
}

function Assert-WindShareNoReparsePointInPath {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )

    $pathRoot = [IO.Path]::GetPathRoot($Path)
    if ([string]::IsNullOrEmpty($pathRoot)) { throw "$Label must have a filesystem root" }
    $cursor = $pathRoot
    $components = $Path.Substring($pathRoot.Length) -split '[\\/]'
    foreach ($component in (@('') + @($components))) {
        if (-not [string]::IsNullOrEmpty($component)) { $cursor = Join-Path $cursor $component }
        $item = Get-Item -LiteralPath $cursor -Force
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "$Label must not traverse a reparse-point authority"
        }
    }
}

function Assert-WindShareRetainedPath {
    param(
        [Parameter(Mandatory)][string]$Expected,
        [Parameter(Mandatory)][Microsoft.Win32.SafeHandles.SafeFileHandle]$Handle,
        [Parameter(Mandatory)][string]$Label
    )

    $actual = [WindShareMakeAuthorityNativePath]::FinalPath($Handle)
    if ($actual.StartsWith('\\?\UNC\', [StringComparison]::OrdinalIgnoreCase)) {
        $actual = '\\' + $actual.Substring(8)
    } elseif ($actual.StartsWith('\\?\', [StringComparison]::OrdinalIgnoreCase)) {
        $actual = $actual.Substring(4)
    }
    $actual = [IO.Path]::GetFullPath($actual)
    if (-not [StringComparer]::OrdinalIgnoreCase.Equals($actual, $Expected)) {
        throw "$Label changed before its exact object could be retained"
    }
}

Export-ModuleMember -Function @(
    'Enter-WindShareProtectedPathAuthority',
    'Exit-WindShareProtectedPathAuthority'
)
