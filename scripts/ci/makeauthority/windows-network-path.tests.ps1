[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $IsWindows) {
    Write-Output 'windows-network-path tests: SKIP (non-Windows)'
    exit 0
}

$authorityModule = Join-Path $PSScriptRoot 'protected-path-authority.psm1'
Import-Module $authorityModule -Force
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) "windshare-network-path-$([Guid]::NewGuid().ToString('N'))"
$realAuthority = Join-Path $fixtureRoot 'real-authority'
$nestedAuthority = Join-Path $realAuthority 'nested'
$junctionAuthority = Join-Path $fixtureRoot 'junction-authority'

try {
    $null = New-Item -ItemType Directory -Path $nestedAuthority -Force
    $completion = Join-Path $nestedAuthority 'browser-network-completion.json'
    Set-Content -LiteralPath $completion -Value '{} ' -Encoding utf8NoBOM
    $null = New-Item -ItemType Junction -Path $junctionAuthority -Target $realAuthority
    try {
        $null = Enter-WindShareProtectedPathAuthority `
            -Completion (Join-Path $junctionAuthority 'nested\browser-network-completion.json')
    } catch {
        if ($_.Exception.Message -match 'reparse-point authority') {
            Write-Output 'windows-network-path tests: PASS'
            exit 0
        }
        throw "completion junction failed for the wrong reason: $($_.Exception.Message)"
    } finally {
        Exit-WindShareProtectedPathAuthority
    }
    throw 'completion junction unexpectedly crossed a reparse-point authority'
} finally {
    if (Test-Path -LiteralPath $junctionAuthority) {
        Remove-Item -LiteralPath $junctionAuthority -Force
    }
    $canonicalFixtureRoot = [IO.Path]::GetFullPath($fixtureRoot)
    $canonicalTemporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    if (-not $canonicalFixtureRoot.StartsWith($canonicalTemporaryRoot, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'refusing to remove a network-path fixture outside the temporary directory'
    }
    Remove-Item -LiteralPath $canonicalFixtureRoot -Recurse -Force -ErrorAction SilentlyContinue
}
