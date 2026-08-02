[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-Throws {
    param(
        [Parameter(Mandatory)][scriptblock]$Action,
        [Parameter(Mandatory)][string]$Pattern,
        [string]$Label = 'action'
    )
    try {
        & $Action
    } catch {
        if ($_.Exception.Message -notmatch $Pattern) {
            throw "expected error matching $Pattern, received: $($_.Exception.Message)"
        }
        return
    }
    throw "expected $Label to fail with $Pattern"
}

$fixture = Join-Path ([IO.Path]::GetTempPath()) "windshare-protected-path-$([Guid]::NewGuid().ToString('n'))"
$real = Join-Path $fixture 'real'
$nested = Join-Path $real 'nested'
$junction = Join-Path $fixture 'junction'
$completion = Join-Path $fixture 'completion.json'
[IO.Directory]::CreateDirectory($nested) | Out-Null
[IO.File]::WriteAllText($completion, '{}', [Text.UTF8Encoding]::new($false))
try {
    Import-Module (Join-Path $PSScriptRoot 'protected-path-authority.psm1') -Force
    $null = New-Item -ItemType Junction -Path $junction -Target $real
    Assert-Throws {
        Enter-WindShareProtectedPathAuthority `
            -Completion (Join-Path $junction 'nested\missing.json')
    } 'canonical|reparse-point'
    [IO.File]::WriteAllText((Join-Path $nested 'runtime.json'), '{}', [Text.UTF8Encoding]::new($false))
    Assert-Throws {
        Enter-WindShareProtectedPathAuthority `
            -Completion (Join-Path $junction 'nested\runtime.json')
    } 'reparse-point'

    $authority = Enter-WindShareProtectedPathAuthority -Completion $completion
    if (-not $authority.CompletionStream.CanRead) {
        throw 'protected completion was not retained as one exact live handle'
    }
    Assert-Throws { [IO.File]::WriteAllText($completion, '{"swapped":true}') } 'used by another process' 'completion write'
    Assert-Throws { [IO.File]::Move($completion, "$completion.swapped") } 'used by another process' 'completion move'

    Exit-WindShareProtectedPathAuthority
    [IO.File]::Move($completion, "$completion.released")
} finally {
    Exit-WindShareProtectedPathAuthority -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $junction) { [IO.Directory]::Delete($junction) }
    Remove-Item -LiteralPath $fixture -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'protected path authority PowerShell tests: PASS'
