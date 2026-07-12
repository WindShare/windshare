Set-StrictMode -Version Latest

$script:D5HarnessLeaseTimeout = [timespan]::FromSeconds(180)
$script:D5HarnessLeaseRetryMilliseconds = 100
$script:D5HarnessQuiescenceTimeout = [timespan]::FromSeconds(30)
$script:D5HarnessQuiescenceRetryMilliseconds = 100

function Get-D5HarnessTokenDigest([string]$Token) {
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($Token)
    return [Convert]::ToHexString(
        [Security.Cryptography.SHA256]::HashData($bytes)
    ).ToLowerInvariant()
}

function New-D5HarnessLeaseStream(
    [string]$Path,
    [IO.FileMode]$Mode,
    [IO.FileShare]$Share
) {
    return [IO.FileStream]::new(
        $Path,
        $Mode,
        [IO.FileAccess]::ReadWrite,
        $Share,
        4096,
        [IO.FileOptions]::DeleteOnClose
    )
}

function Acquire-D5HarnessLease(
    [Parameter(Mandatory)] [string]$Directory,
    [Parameter(Mandatory)] [string]$Contract,
    [timespan]$Timeout = $script:D5HarnessLeaseTimeout
) {
    if ([string]::IsNullOrWhiteSpace($Contract)) {
        throw 'The stable Windows E2E lease contract must be named.'
    }

    New-Item -ItemType Directory -Force -Path $Directory | Out-Null
    $lockPath = Join-Path $Directory '.owner.lock'
    $deadline = [datetimeoffset]::Now.Add($Timeout)
    while ([datetimeoffset]::Now -lt $deadline) {
        $stream = $null
        try {
            $stream = New-D5HarnessLeaseStream $lockPath CreateNew Read
            $tokenBytes = [byte[]]::new(16)
            [Security.Cryptography.RandomNumberGenerator]::Fill($tokenBytes)
            $token = [Convert]::ToHexString($tokenBytes).ToLowerInvariant()
            $metadata = [ordered]@{
                contract = $Contract
                ownerPid = $PID
                acquiredAt = [datetimeoffset]::Now.ToString('o')
                tokenSha256 = Get-D5HarnessTokenDigest $token
            } | ConvertTo-Json -Compress
            $bytes = [Text.UTF8Encoding]::new($false).GetBytes($metadata)
            $stream.Write($bytes, 0, $bytes.Length)
            $stream.Flush($true)
            $stream.Position = 0
            return [pscustomobject]@{
                Path = $lockPath
                Token = $token
                Stream = $stream
            }
        } catch [IO.IOException] {
            if ($null -ne $stream) {
                $stream.Dispose()
                throw
            }

            # Recovery owns the exact stale file identity before deleting it. A live
            # owner's FileShare.Read handle rejects this write/exclusive open, so no
            # waiter can unlink a successor after an ABA path reuse.
            $staleStream = $null
            try {
                $staleStream = New-D5HarnessLeaseStream $lockPath Open None
            } catch [IO.IOException] {
                Start-Sleep -Milliseconds $script:D5HarnessLeaseRetryMilliseconds
                continue
            } finally {
                if ($null -ne $staleStream) {
                    $staleStream.Dispose()
                }
            }
        } catch {
            if ($null -ne $stream) {
                $stream.Dispose()
            }
            throw
        }
    }
    throw "Timed out waiting for the global D5 Windows harness lease at $lockPath"
}

function Assert-D5HarnessLeaseHeld([object]$Lease) {
    if ($null -eq $Lease -or
        $null -eq $Lease.Stream -or
        -not $Lease.Stream.CanRead -or
        -not $Lease.Stream.CanWrite -or
        -not (Test-Path -LiteralPath $Lease.Path -PathType Leaf)) {
        throw 'The global D5 Windows harness lease is no longer held'
    }
}

function Get-D5HarnessProcesses([string]$HarnessRoot) {
    $root = [IO.Path]::GetFullPath($HarnessRoot).TrimEnd(
        [IO.Path]::DirectorySeparatorChar,
        [IO.Path]::AltDirectorySeparatorChar
    )
    return @(
        Get-CimInstance Win32_Process -ErrorAction Stop |
            Where-Object {
                if ([string]::IsNullOrWhiteSpace([string]$_.ExecutablePath)) {
                    return $false
                }
                $path = [IO.Path]::GetFullPath([string]$_.ExecutablePath)
                return $path.StartsWith(
                    $root + [IO.Path]::DirectorySeparatorChar,
                    [StringComparison]::OrdinalIgnoreCase
                )
            } |
            ForEach-Object {
                [pscustomobject]@{
                    ProcessID = [int]$_.ProcessID
                    ExecutablePath = [IO.Path]::GetFullPath([string]$_.ExecutablePath)
                }
            }
    )
}

function Wait-D5HarnessNamespaceQuiescent(
    [object]$Lease,
    [string]$HarnessRoot,
    [timespan]$Timeout = $script:D5HarnessQuiescenceTimeout
) {
    $deadline = [datetimeoffset]::Now.Add($Timeout)
    do {
        Assert-D5HarnessLeaseHeld $Lease
        $processes = @(Get-D5HarnessProcesses $HarnessRoot)
        if ($processes.Count -eq 0) {
            return
        }
        Start-Sleep -Milliseconds $script:D5HarnessQuiescenceRetryMilliseconds
    } while ([datetimeoffset]::Now -lt $deadline)
    $description = @($processes | ForEach-Object {
        "$($_.ProcessID):$($_.ExecutablePath)"
    }) -join ', '
    throw "The fixed D5 Windows namespace is still in use after prior-owner loss: $description"
}

function Release-D5HarnessLease([object]$Lease) {
    if ($null -eq $Lease -or $null -eq $Lease.Stream) {
        return
    }
    $stream = $Lease.Stream
    try {
        $stream.Dispose()
    } finally {
        $Lease.Stream = $null
    }
}
