Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (Test-Path Variable:PSNativeCommandUseErrorActionPreference) {
    $PSNativeCommandUseErrorActionPreference = $false
}

Import-Module (Join-Path $PSScriptRoot 'release-environment.psm1') -Force

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$artifactVersion = 'v0.0.0-ci'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-release-archive-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
$fixtureRepository = Join-Path $testRoot 'repository'
$releaseEnvironment = Join-Path $testRoot 'release-environment'
$environmentState = $null

function Invoke-Git([string[]]$Arguments) {
    $output = @(& git @Arguments)
    if ($LASTEXITCODE -ne 0) {
        throw "git $($Arguments -join ' ') exited with code $LASTEXITCODE"
    }
    return $output
}

try {
    New-Item -ItemType Directory -Path @($testRoot, $releaseEnvironment) | Out-Null
    Invoke-Git -Arguments @('clone', '--quiet', '--no-hardlinks', '--', $repositoryRoot, $fixtureRepository) | Out-Null
    Invoke-Git -Arguments @(
        '-C', $fixtureRepository, 'rm', '--quiet', '--ignore-unmatch',
        'core/go.mod', 'core/go.sum', 'go.work', 'go.work.sum'
    ) | Out-Null
    Invoke-Git -Arguments @(
        '-C', $fixtureRepository,
        '-c', 'user.name=WindShare',
        '-c', 'user.email=release-contract.invalid',
        'commit', '--quiet', '--allow-empty', '-m', 'single-module-fixture'
    ) | Out-Null
    $commitSHA = [string](Invoke-Git -Arguments @('-C', $fixtureRepository, 'rev-parse', 'HEAD'))
    $commitSHA = $commitSHA.Trim()
    if ($commitSHA -cnotmatch '^[0-9a-f]{40}$') {
        throw 'fixture HEAD is not an exact commit SHA'
    }
    $initialStatus = @(Invoke-Git -Arguments @(
        '-C', $fixtureRepository, 'status', '--porcelain=v1', '--untracked-files=all'
    ))
    if ($initialStatus.Count -ne 0) {
        throw "fixture checkout was not initially clean: $($initialStatus -join '; ')"
    }
    $expectedREADMEObject = [string](Invoke-Git -Arguments @(
        '-C', $fixtureRepository, 'rev-parse', "${commitSHA}:core/README.md"
    ))
    $expectedREADMEObject = $expectedREADMEObject.Trim()

    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'core\README.md'),
        "tracked mutation after clean proof`n",
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'untracked-after-clean.txt'),
        "untracked`n",
        [Text.UTF8Encoding]::new($false)
    )
    Invoke-Git -Arguments @('-C', $fixtureRepository, 'add', '--', 'core/README.md') | Out-Null

    $environmentState = Enter-ReleaseGoEnvironment -ReleaseRoot $releaseEnvironment
    Set-Location $repositoryRoot
    & go run ./scripts/ci/_sourcebundle `
        -repo $fixtureRepository `
        -commit $commitSHA `
        -stage (Join-Path $testRoot 'committed-module') `
        -zip (Join-Path $testRoot 'source.zip') `
        -extract (Join-Path $testRoot 'extracted-module') `
        -version $artifactVersion | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "commit-bound archive helper exited with code $LASTEXITCODE"
    }

    $actualREADMEObject = [string](Invoke-Git -Arguments @(
        'hash-object', '--', (Join-Path $testRoot 'extracted-module\core\README.md')
    ))
    if ($actualREADMEObject.Trim() -cne $expectedREADMEObject) {
        throw 'archive consumed tracked worktree/index bytes'
    }
    if (Test-Path -LiteralPath (
        Join-Path $testRoot 'extracted-module\untracked-after-clean.txt'
    )) {
        throw 'archive consumed an untracked worktree file'
    }
} finally {
    if ($null -ne $environmentState) {
        Exit-ReleaseGoEnvironment -State $environmentState
    }
    if (Test-Path -LiteralPath $testRoot) {
        $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
        $resolvedTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
        $ownedPrefix = Join-Path $resolvedTemp 'windshare-release-archive-contract-'
        if (-not $resolvedRoot.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "refusing to remove unowned archive-test path: $resolvedRoot"
        }
        Remove-Item -LiteralPath $resolvedRoot -Recurse -Force
    }
}

Write-Output 'release committed archive contract: PASS'
