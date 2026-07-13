Set-StrictMode -Version Latest

$script:D5RunnerGuardConnectionTimeout = [timespan]::FromSeconds(10)
$script:D5RunnerGuardCapacity = 32
$script:D5LaunchAuthorizationTimeout = [timespan]::FromSeconds(10)

if ($null -eq ('D5NamedPipeIdentity' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class D5NamedPipeIdentity
{
    [DllImport("kernel32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool GetNamedPipeClientProcessId(
        IntPtr pipe,
        out uint clientProcessId
    );
}
'@
}

if ($null -eq ('D5ProcessIdentity' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Text;

public static class D5ProcessIdentity
{
    public const uint ProcessNameNative = 1;

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool QueryFullProcessImageNameW(
        IntPtr process,
        uint flags,
        StringBuilder exeName,
        ref uint size
    );

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern uint QueryDosDeviceW(
        string deviceName,
        StringBuilder targetPath,
        uint capacity
    );
}
'@
}

function Get-D5ProcessNativeImagePath([Parameter(Mandatory)] [Diagnostics.Process]$Process) {
    # Process.MainModule (and the Win32-format image query) stop answering the
    # moment a child exits, a race fast short-lived probes lose. The NT-native
    # image name is served from the kernel process object, which the
    # parent-held handle keeps alive even after exit.
    $capacity = [uint32]32768
    $builder = [Text.StringBuilder]::new([int]$capacity)
    if (-not [D5ProcessIdentity]::QueryFullProcessImageNameW(
            $Process.Handle,
            [D5ProcessIdentity]::ProcessNameNative,
            $builder,
            [ref]$capacity
        )) {
        throw ("Could not resolve the executable image path of PID $($Process.Id): " +
            [Runtime.InteropServices.Marshal]::GetLastWin32Error())
    }
    return $builder.ToString(0, [int]$capacity)
}

function ConvertTo-D5NativeImagePath([Parameter(Mandatory)] [string]$Path) {
    # Identity is compared in NT-native form: drive letter to volume device is
    # a function, while the reverse mapping can be ambiguous.
    $fullPath = [IO.Path]::GetFullPath($Path)
    $root = [IO.Path]::GetPathRoot($fullPath)
    if ($root -notmatch '^[A-Za-z]:\\$') {
        throw "Executable identity requires a drive-rooted path: $fullPath"
    }
    $capacity = [uint32]32768
    $builder = [Text.StringBuilder]::new([int]$capacity)
    if ([D5ProcessIdentity]::QueryDosDeviceW(
            $fullPath.Substring(0, 2),
            $builder,
            $capacity
        ) -eq 0) {
        throw ("Could not resolve the volume device of $($fullPath.Substring(0, 2)): " +
            [Runtime.InteropServices.Marshal]::GetLastWin32Error())
    }
    # QueryDosDevice yields a MULTI_SZ; StringBuilder marshaling stops at the
    # first terminator, which is the current mapping.
    return $builder.ToString() + $fullPath.Substring(2)
}

function Assert-D5ProcessImagePath(
    [Parameter(Mandatory)] [Diagnostics.Process]$Process,
    [Parameter(Mandatory)] [string]$ExpectedPath,
    [Parameter(Mandatory)] [string]$Context
) {
    $expected = ConvertTo-D5NativeImagePath $ExpectedPath
    $actual = Get-D5ProcessNativeImagePath $Process
    if (-not $actual.Equals($expected, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Context PID $($Process.Id) executable $actual does not match $expected"
    }
}

function New-D5RunnerGuard {
    $name = "windshare-d5-$PID-$([guid]::NewGuid().ToString('N'))"
    $servers = [Collections.Generic.List[IO.Pipes.NamedPipeServerStream]]::new()
    $connections = [Collections.Generic.List[Threading.Tasks.Task]]::new()
    try {
        foreach ($slot in 1..$script:D5RunnerGuardCapacity) {
            # Playwright can create distinct worker-fixture lifetimes even with one
            # configured worker. Pre-listening slots keep every lifetime attached to
            # a wrapper-owned handle without reopening or reusing an old identity.
            $server = [IO.Pipes.NamedPipeServerStream]::new(
                $name,
                [IO.Pipes.PipeDirection]::InOut,
                $script:D5RunnerGuardCapacity,
                [IO.Pipes.PipeTransmissionMode]::Byte,
                [IO.Pipes.PipeOptions]::Asynchronous
            )
            $servers.Add($server)
            $connections.Add($server.WaitForConnectionAsync())
        }
        return [pscustomobject]@{
            Name = $name
            Servers = @($servers)
            Connections = @($connections)
            ConnectedCount = 0
        }
    } catch {
        foreach ($server in $servers) {
            $server.Dispose()
        }
        throw
    }
}

function Assert-D5RunnerGuardConnected([object]$Guard) {
    if ($null -eq $Guard) {
        throw 'Browser worker did not connect to the auditing runner guard'
    }
    $deadline = [datetimeoffset]::Now.Add($script:D5RunnerGuardConnectionTimeout)
    while ([datetimeoffset]::Now -lt $deadline) {
        $connected = @($Guard.Connections | Where-Object IsCompletedSuccessfully).Count
        if ($connected -gt 0) {
            $Guard.ConnectedCount = $connected
            return
        }
        Start-Sleep -Milliseconds 50
    }
    throw 'Browser worker did not connect to the auditing runner guard'
}

function Assert-D5RunnerGuardAlive([object]$Guard) {
    if ($null -eq $Guard -or $null -eq $Guard.Servers) {
        throw 'The D5 runner guard is no longer alive'
    }
}

function New-D5LaunchAuthorization(
    [Parameter(Mandatory)] [string]$RunID,
    [Parameter(Mandatory)] [object[]]$Programs
) {
    if ([string]::IsNullOrWhiteSpace($RunID) -or $Programs.Count -eq 0) {
        throw 'A launch authorization requires a run identity and parent-owned program set'
    }
    $name = "windshare-d5-auth-$PID-$([guid]::NewGuid().ToString('N'))"
    $server = [IO.Pipes.NamedPipeServerStream]::new(
        $name,
        [IO.Pipes.PipeDirection]::InOut,
        1,
        [IO.Pipes.PipeTransmissionMode]::Byte,
        [IO.Pipes.PipeOptions]::Asynchronous
    )
    try {
        return [pscustomobject]@{
            Name = $name
            RunID = $RunID
            Programs = @($Programs)
            Server = $server
            Connection = $server.WaitForConnectionAsync()
            AuthorizedPID = $null
            AuthorizedExecutable = $null
            State = 'AwaitingConnection'
        }
    } catch {
        $server.Dispose()
        throw
    }
}

function Get-D5LaunchProgram([object]$Authorization, [string]$Path) {
    $fullPath = [IO.Path]::GetFullPath($Path)
    $matches = @($Authorization.Programs | Where-Object {
        [IO.Path]::GetFullPath([string]$_.Path).Equals(
            $fullPath,
            [StringComparison]::OrdinalIgnoreCase
        )
    })
    if ($matches.Count -ne 1) {
        throw "The parent-owned launch set does not contain exactly one record for $fullPath"
    }
    return $matches[0]
}

function Complete-D5LaunchAuthorization(
    [Parameter(Mandatory)] [object]$Authorization,
    [Parameter(Mandatory)] [Diagnostics.Process]$Process,
    [Parameter(Mandatory)] [string]$ExpectedExecutable,
    [timespan]$Timeout = $script:D5LaunchAuthorizationTimeout
) {
    if ([string]$Authorization.State -ne 'AwaitingConnection') {
        throw 'The one-use launch authorization was already consumed or released'
    }
    # A failed handshake is consumed too: retrying it would turn a process-bound
    # grant into a reusable capability after its original launch context is gone.
    $Authorization.State = 'Completing'
    if (-not $Authorization.Connection.Wait($Timeout)) {
        $Authorization.State = 'Rejected'
        throw "Process $($Process.Id) did not connect to its one-use launch authorization"
    }
    if (-not $Authorization.Connection.IsCompletedSuccessfully) {
        $Authorization.State = 'Rejected'
        throw "Process $($Process.Id) failed to connect to its one-use launch authorization"
    }
    [uint32]$clientPID = 0
    $handle = $Authorization.Server.SafePipeHandle.DangerousGetHandle()
    if (-not [D5NamedPipeIdentity]::GetNamedPipeClientProcessId($handle, [ref]$clientPID)) {
        $Authorization.State = 'Rejected'
        throw "Could not resolve the launch authorization client PID: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
    }
    if ([int]$clientPID -ne $Process.Id) {
        $Authorization.State = 'Rejected'
        throw "Launch authorization client PID $clientPID does not match parent-started PID $($Process.Id)"
    }
    $expectedPath = [IO.Path]::GetFullPath($ExpectedExecutable)
    try {
        Assert-D5ProcessImagePath $Process $expectedPath 'Parent-started'
    } catch {
        $Authorization.State = 'Rejected'
        throw
    }
    $program = Get-D5LaunchProgram $Authorization $expectedPath
    $item = Get-Item -LiteralPath $expectedPath
    $hash = (Get-FileHash -LiteralPath $expectedPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ([long]$program.Bytes -ne [long]$item.Length -or
        [string]$program.SHA256 -ne $hash) {
        $Authorization.State = 'Rejected'
        throw "Parent-started executable differs from the parent-owned launch set: $expectedPath"
    }
    $payload = [ordered]@{
        SchemaVersion = 1
        RunID = $Authorization.RunID
        Programs = @($Authorization.Programs)
    } | ConvertTo-Json -Depth 8 -Compress
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($payload)
    $length = [BitConverter]::GetBytes([int]$bytes.Length)
    $Authorization.Server.Write($length, 0, $length.Length)
    $Authorization.Server.Write($bytes, 0, $bytes.Length)
    $Authorization.Server.Flush()
    $Authorization.AuthorizedPID = $Process.Id
    $Authorization.AuthorizedExecutable = $expectedPath
    $Authorization.State = 'Consumed'
}

function Release-D5LaunchAuthorization([object]$Authorization) {
    if ($null -eq $Authorization -or $null -eq $Authorization.Server) {
        return
    }
    try {
        $Authorization.Server.Dispose()
    } finally {
        $Authorization.Server = $null
        $Authorization.State = 'Released'
    }
}

function Release-D5RunnerGuard([object]$Guard) {
    if ($null -eq $Guard -or $null -eq $Guard.Servers) {
        return
    }
    $servers = @($Guard.Servers)
    try {
        foreach ($server in $servers) {
            $server.Dispose()
        }
    } finally {
        $Guard.Servers = $null
    }
}
