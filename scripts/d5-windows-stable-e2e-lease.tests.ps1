Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    Write-Output 'D5 stable Windows E2E lease tests SKIP: Windows-only semantics'
    return
}

. (Join-Path $PSScriptRoot 'd5-windows-stable-e2e-lease.ps1')
. (Join-Path $PSScriptRoot 'd5-windows-runner-guard.ps1')

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

function Read-D5LiveLease([string]$Path) {
    $share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
    $stream = [IO.FileStream]::new(
        $Path,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        $share
    )
    try {
        $reader = [IO.StreamReader]::new(
            $stream,
            [Text.UTF8Encoding]::new($false),
            $true,
            1024,
            $true
        )
        try {
            return $reader.ReadToEnd()
        } finally {
            $reader.Dispose()
        }
    } finally {
        $stream.Dispose()
    }
}

$contract = 'stable-harness-v3'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-d5-stable-lease-' + [guid]::NewGuid().ToString('N')
)
$lockPath = Join-Path $testRoot '.owner.lock'
$first = $null
$successor = $null
$child = $null
$guard = $null
$client = $null
$client2 = $null
$lingering = $null
$quiescenceLease = $null
try {
    New-Item -ItemType Directory -Force -Path $testRoot | Out-Null

    # An abandoned path is recovered only after an exclusive handle owns that
    # exact file identity; no PID/mtime inference or path-only removal is used.
    [IO.File]::WriteAllText($lockPath, '{"abandoned":true}', [Text.UTF8Encoding]::new($false))
    $first = Acquire-D5HarnessLease $testRoot $contract
    $raw = Read-D5LiveLease $lockPath
    $record = $raw | ConvertFrom-Json
    if ($record.contract -ne $contract -or
        $record.ownerPid -ne $PID -or
        $record.tokenSha256 -ne (Get-D5HarnessTokenDigest $first.Token) -or
        $raw.Contains($first.Token, [StringComparison]::Ordinal)) {
        throw 'Stable lease metadata did not preserve the token-digest boundary'
    }

    Assert-Throws {
        Acquire-D5HarnessLease $testRoot $contract ([timespan]::FromMilliseconds(250))
    } 'Timed out waiting'

    Release-D5HarnessLease $first
    if (Test-Path -LiteralPath $lockPath) {
        throw 'DeleteOnClose did not remove the released lease identity'
    }

    $successor = Acquire-D5HarnessLease $testRoot $contract
    Release-D5HarnessLease $first
    if (-not (Test-Path -LiteralPath $lockPath)) {
        throw 'An old owner release deleted its successor lease'
    }
    Release-D5HarnessLease $successor
    if (Test-Path -LiteralPath $lockPath) {
        throw 'Successor lease identity remained after handle release'
    }

    $helperPath = Join-Path $testRoot 'crash-owner.ps1'
    $modulePath = Join-Path $PSScriptRoot 'd5-windows-stable-e2e-lease.ps1'
    $helper = @"
`$ErrorActionPreference = 'Stop'
. '$($modulePath.Replace("'", "''"))'
`$lease = Acquire-D5HarnessLease '$($testRoot.Replace("'", "''"))' '$contract'
[Console]::Out.WriteLine('READY')
[Console]::Out.Flush()
Start-Sleep -Seconds 60
"@
    [IO.File]::WriteAllText($helperPath, $helper, [Text.UTF8Encoding]::new($false))
    $start = [Diagnostics.ProcessStartInfo]::new()
    $start.FileName = Join-Path $PSHOME 'pwsh.exe'
    $start.ArgumentList.Add('-NoProfile')
    $start.ArgumentList.Add('-NonInteractive')
    $start.ArgumentList.Add('-File')
    $start.ArgumentList.Add($helperPath)
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.RedirectStandardOutput = $true
    $start.RedirectStandardError = $true
    $child = [Diagnostics.Process]::new()
    $child.StartInfo = $start
    if (-not $child.Start()) {
        throw 'Could not start the crash-cleanup lease owner'
    }
    $readyTask = $child.StandardOutput.ReadLineAsync()
    if (-not $readyTask.Wait([timespan]::FromSeconds(10)) -or $readyTask.Result -ne 'READY') {
        throw "Crash-cleanup lease owner did not become ready: $($child.StandardError.ReadToEnd())"
    }
    if (-not (Test-Path -LiteralPath $lockPath)) {
        throw 'Crash-cleanup lease owner did not materialize its identity'
    }
    Assert-Throws {
        Acquire-D5HarnessLease $testRoot $contract ([timespan]::FromMilliseconds(250))
    } 'Timed out waiting'
    $child.Kill($true)
    $child.WaitForExit()
    foreach ($attempt in 1..50) {
        if (-not (Test-Path -LiteralPath $lockPath)) {
            break
        }
        Start-Sleep -Milliseconds 100
    }
    if (Test-Path -LiteralPath $lockPath) {
        throw 'The OS did not delete the lease identity after its owner crashed'
    }

    # A successor may acquire the path immediately after owner death, but it must
    # not reuse fixed binaries until every prior-generation executable is gone.
    $timeoutCopy = Join-Path $testRoot 'prior-owner.test.exe'
    Copy-Item -LiteralPath (Join-Path $env:SystemRoot 'System32\timeout.exe') -Destination $timeoutCopy
    $lingering = Start-Process `
        -FilePath $timeoutCopy `
        -ArgumentList @('/t', '60', '/nobreak') `
        -PassThru `
        -WindowStyle Hidden
    $quiescenceLease = Acquire-D5HarnessLease $testRoot $contract
    Assert-Throws {
        Wait-D5HarnessNamespaceQuiescent `
            $quiescenceLease `
            $testRoot `
            ([timespan]::FromMilliseconds(500))
    } 'still in use after prior-owner loss'
    $lingering.Kill($true)
    $lingering.WaitForExit()
    Wait-D5HarnessNamespaceQuiescent $quiescenceLease $testRoot ([timespan]::FromSeconds(5))
    Release-D5HarnessLease $quiescenceLease

    $guard = New-D5RunnerGuard
    $client = [IO.Pipes.NamedPipeClientStream]::new(
        '.',
        $guard.Name,
        [IO.Pipes.PipeDirection]::InOut,
        [IO.Pipes.PipeOptions]::Asynchronous
    )
    $client.Connect(5000)
    $client.Dispose()
    $client = $null
    $client2 = [IO.Pipes.NamedPipeClientStream]::new(
        '.',
        $guard.Name,
        [IO.Pipes.PipeDirection]::InOut,
        [IO.Pipes.PipeOptions]::Asynchronous
    )
    $client2.Connect(5000)
    $connectionDeadline = [datetimeoffset]::Now.AddSeconds(5)
    while ([datetimeoffset]::Now -lt $connectionDeadline -and
        @($guard.Connections | Where-Object IsCompletedSuccessfully).Count -lt 2) {
        Start-Sleep -Milliseconds 50
    }
    Assert-D5RunnerGuardConnected $guard
    if ($guard.ConnectedCount -lt 2) {
        throw 'Runner guard did not admit two sequential browser worker lifetimes'
    }
    $buffer = [byte[]]::new(1)
    $disconnect = $client2.ReadAsync($buffer, 0, 1)
    Release-D5RunnerGuard $guard
    if (-not $disconnect.Wait([timespan]::FromSeconds(5)) -or
        (-not $disconnect.IsFaulted -and $disconnect.Result -ne 0)) {
        throw 'Runner guard disconnect was not observable by its client'
    }

    Write-Output 'D5 global Windows harness lease/guard tests PASS'
} finally {
    Release-D5HarnessLease $first
    Release-D5HarnessLease $successor
    Release-D5HarnessLease $quiescenceLease
    if ($null -ne $child -and -not $child.HasExited) {
        $child.Kill($true)
        $child.WaitForExit()
    }
    if ($null -ne $child) {
        $child.Dispose()
    }
    if ($null -ne $lingering -and -not $lingering.HasExited) {
        $lingering.Kill($true)
        $lingering.WaitForExit()
    }
    if ($null -ne $lingering) {
        $lingering.Dispose()
    }
    Release-D5RunnerGuard $guard
    if ($null -ne $client) {
        $client.Dispose()
    }
    if ($null -ne $client2) {
        $client2.Dispose()
    }
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}
