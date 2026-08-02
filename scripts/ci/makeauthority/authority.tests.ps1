[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-Throws {
    param(
        [Parameter(Mandatory)][scriptblock]$Action,
        [Parameter(Mandatory)][string]$Pattern
    )
    try {
        & $Action
    } catch {
        if ($_.Exception.Message -notmatch $Pattern) {
            throw "expected error matching $Pattern, received: $($_.Exception.Message)"
        }
        return
    }
    throw "expected action to fail with $Pattern"
}

$selectionNames = @('MAKEFLAGS', 'MFLAGS', 'GNUMAKEFLAGS', 'MAKEFILES', 'MAKEOVERRIDES', 'MAKE_RESTARTS', 'MAKELEVEL', 'MAKESHELL')
$previous = @{}
foreach ($name in $selectionNames) {
    $previous[$name] = [pscustomobject]@{
        Exists = Test-Path "Env:$name"
        Value = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    Remove-Item "Env:$name" -ErrorAction SilentlyContinue
}

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\..'))
$fixture = Join-Path ([IO.Path]::GetTempPath()) "windshare-make-authority-$([guid]::NewGuid().ToString('n'))"
$snapshotName = "windshare-make-authority-test-$([guid]::NewGuid().ToString('n'))"
$snapshotDirectory = Join-Path $repositoryRoot ".git\$snapshotName"
$snapshotPath = Join-Path $snapshotDirectory 'Makefile'
$snapshotRelativePath = ".git/$snapshotName/Makefile"
[IO.Directory]::CreateDirectory($fixture) | Out-Null
[IO.Directory]::CreateDirectory($snapshotDirectory) | Out-Null
$originalLocation = Get-Location
try {
    Set-Location $repositoryRoot
    Import-Module (Join-Path $PSScriptRoot 'authority.psm1') -Force

    $fakeScript = Join-Path $fixture 'make.cmd'
    [IO.File]::WriteAllText($fakeScript, "@exit /b 0`r`n")
    Assert-Throws { Enter-WindShareMakeAuthority -CandidatePath $fakeScript } 'native PE application'

    $fakeNative = Join-Path $fixture 'fake-native.exe'
    [IO.File]::Copy([Diagnostics.Process]::GetCurrentProcess().MainModule.FileName, $fakeNative)
    Assert-Throws { Enter-WindShareMakeAuthority -CandidatePath $fakeNative } 'identify itself as GNU Make'

    $actualMake = (Get-Command make -CommandType Application | Select-Object -First 1).Source
    $retainedMake = Join-Path $fixture 'make.exe'
    New-Item -ItemType HardLink -Path $retainedMake -Target $actualMake | Out-Null
    $makeAuthority = Enter-WindShareMakeAuthority -CandidatePath $retainedMake
    if (-not $makeAuthority.RetainedStream.CanRead) { throw 'Make authority did not retain its application handle' }
    Assert-Throws { [IO.File]::Move($retainedMake, "$retainedMake.swapped") } 'used by another process'

    $makefileBytes = [Text.Encoding]::UTF8.GetBytes("probe:`n`t@:`n")
    [IO.File]::WriteAllBytes($snapshotPath, $makefileBytes)
    $makefileHash = ([BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($makefileBytes))).Replace('-', '').ToLowerInvariant()
    $makefileAuthority = Enter-WindShareMakefileAuthority `
        -CanonicalMakefile $snapshotRelativePath `
        -ExpectedSHA256 $makefileHash
    if (-not $makefileAuthority.RetainedStream.CanRead -or $makefileAuthority.Path -cne $snapshotRelativePath) {
        throw 'Makefile authority did not retain its safe relative parser snapshot'
    }
    Assert-Throws { [IO.File]::Move($snapshotPath, "$snapshotPath.swapped") } 'used by another process'

    $actualGit = (Get-Command git -CommandType Application | Select-Object -First 1).Source
    $gitAuthority = Enter-WindShareGitAuthority -CandidatePath $actualGit
    if (-not $gitAuthority.RetainedStream.CanRead) { throw 'Git authority did not retain its application handle' }

    $actualShell = (Get-Command sh -CommandType Application | Select-Object -First 1).Source
    $retainedShell = Join-Path $fixture 'sh.exe'
    New-Item -ItemType HardLink -Path $retainedShell -Target $actualShell | Out-Null
    $shellAuthority = Enter-WindShareRecipeShellAuthority -CandidatePath $retainedShell
    if (-not $shellAuthority.RetainedStream.CanRead) { throw 'recipe shell authority did not retain its application handle' }
    Assert-Throws { [IO.File]::Move($retainedShell, "$retainedShell.swapped") } 'used by another process'

    $pwshAuthority = Enter-WindSharePwshAuthority
    if (-not $pwshAuthority.RetainedStream.CanRead) { throw 'pwsh authority did not retain its application handle' }

    $versionOutput = @(Invoke-WindShareMake --version)
    if ($LASTEXITCODE -ne 0 -or $versionOutput.Count -lt 1 -or $versionOutput[0] -notmatch '^GNU Make ') {
        throw 'retained Make application did not execute'
    }
    Exit-WindShareMakeAuthority

    [IO.File]::Move($retainedMake, "$retainedMake.released")
    [IO.File]::Move($retainedShell, "$retainedShell.released")
} finally {
    Set-Location $originalLocation
    Exit-WindShareMakeAuthority -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $snapshotDirectory -Recurse -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $fixture -Recurse -Force -ErrorAction SilentlyContinue
    foreach ($name in $selectionNames) {
        if ($previous[$name].Exists) {
            [Environment]::SetEnvironmentVariable($name, $previous[$name].Value, 'Process')
        } else {
            Remove-Item "Env:$name" -ErrorAction SilentlyContinue
        }
    }
}

Write-Output 'make authority PowerShell tests: PASS'
