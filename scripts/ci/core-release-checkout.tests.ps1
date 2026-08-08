Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (Test-Path Variable:PSNativeCommandUseErrorActionPreference) {
    $PSNativeCommandUseErrorActionPreference = $false
}

$checkoutModule = Import-Module `
    (Join-Path $PSScriptRoot 'core-release-checkout.psm1') `
    -Force `
    -PassThru

$testRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-core-checkout-contract-{0}' -f [Guid]::NewGuid().ToString('N')
)
$fixtureRepository = Join-Path $testRoot 'repository'
$linkedWorktree = Join-Path $testRoot 'linked-worktree'
$exactCheckout = Join-Path $testRoot 'exact-checkout'
$commitSHA = ''
$gitVariableNames = @('GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE')
$originalGitVariables = [ordered]@{}
foreach ($name in $gitVariableNames) {
    $originalGitVariables[$name] = [Environment]::GetEnvironmentVariable(
        $name,
        [EnvironmentVariableTarget]::Process
    )
}

function Assert-CheckoutThrows(
    [string]$RepositoryRoot,
    [string]$Label,
    [string]$ExpectedMessage
) {
    try {
        Assert-ExactCoreReleaseCheckout `
            -RepositoryRoot $RepositoryRoot `
            -ExpectedCommit $commitSHA
    } catch {
        if (-not $_.Exception.Message.Contains($ExpectedMessage, [StringComparison]::Ordinal)) {
            throw "$Label failed for the wrong reason: $($_.Exception.Message)"
        }
        return
    }
    throw "$Label did not fail closed"
}

function Resolve-TestGitExecutable([object[]]$Candidates) {
    $resolved = @(& $checkoutModule {
        Resolve-CoreReleaseGitExecutable -CommandCandidates $args[0]
    } $Candidates)
    if ($resolved.Count -ne 1) {
        throw "test Git resolution returned $($resolved.Count) paths"
    }
    return [string]$resolved[0]
}

function Assert-GitResolutionThrows(
    [string]$Label,
    [object[]]$Candidates,
    [string]$ExpectedMessage
) {
    try {
        Resolve-TestGitExecutable -Candidates $Candidates | Out-Null
    } catch {
        if (-not $_.Exception.Message.Contains($ExpectedMessage, [StringComparison]::Ordinal)) {
            throw "$Label failed for the wrong reason: $($_.Exception.Message)"
        }
        return
    }
    throw "$Label did not fail closed"
}

try {
    New-Item -ItemType Directory -Path $fixtureRepository | Out-Null
    $gitApplication = @(Get-Command git -CommandType Application -All -ErrorAction Stop)[0]
    $gitPath = [IO.Path]::GetFullPath([string]$gitApplication.Source)
    $powerShellPath = [IO.Path]::GetFullPath((Get-Process -Id $PID).Path)
    $multipleApplications = @(
        [pscustomobject]@{
            CommandType = [Management.Automation.CommandTypes]::Application
            Source = $gitPath
        },
        [pscustomobject]@{
            CommandType = [Management.Automation.CommandTypes]::Application
            Source = $powerShellPath
        }
    )
    $selectedGit = Resolve-TestGitExecutable -Candidates $multipleApplications
    if ($selectedGit -cne $gitPath) {
        throw "multiple Git Applications selected $selectedGit, want first candidate $gitPath"
    }
    Assert-GitResolutionThrows `
        -Label 'alias Git candidate' `
        -Candidates @([pscustomobject]@{
            CommandType = [Management.Automation.CommandTypes]::Alias
            Source = $gitPath
        }) `
        -ExpectedMessage 'did not select an Application command'
    Assert-GitResolutionThrows `
        -Label 'function Git candidate' `
        -Candidates @([pscustomobject]@{
            CommandType = [Management.Automation.CommandTypes]::Function
            Source = $gitPath
        }) `
        -ExpectedMessage 'did not select an Application command'
    Assert-GitResolutionThrows `
        -Label 'array Git source' `
        -Candidates @([pscustomobject]@{
            CommandType = [Management.Automation.CommandTypes]::Application
            Source = @($gitPath, $powerShellPath)
        }) `
        -ExpectedMessage 'exactly one executable path'
    Assert-GitResolutionThrows `
        -Label 'missing Git executable' `
        -Candidates @([pscustomobject]@{
            CommandType = [Management.Automation.CommandTypes]::Application
            Source = Join-Path $testRoot 'missing-git.exe'
        }) `
        -ExpectedMessage 'not an existing .exe file'

    Invoke-CoreReleaseGit -Arguments @('-C', $fixtureRepository, 'init', '--quiet') | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'tracked.txt'),
        "committed release input`n",
        [Text.UTF8Encoding]::new($false)
    )
    Invoke-CoreReleaseGit -Arguments @('-C', $fixtureRepository, 'add', '--', 'tracked.txt') | Out-Null
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository,
        '-c', 'user.name=WindShare',
        '-c', 'user.email=release-contract.invalid',
        'commit', '--quiet', '-m', 'fixture'
    ) | Out-Null
    $commitSHA = [string](Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'rev-parse', 'HEAD'
    ))
    $commitSHA = $commitSHA.Trim()
    if ($commitSHA -cnotmatch '^[0-9a-f]{40}$') {
        throw 'fixture commit is not an exact SHA'
    }
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository,
        'worktree', 'add', '--quiet', '--detach', $linkedWorktree, $commitSHA
    ) | Out-Null

    # -C does not override these process redirects. The checker must ignore them
    # while preserving the caller's environment after every Git invocation.
    $hostileGitValues = [ordered]@{
        GIT_DIR       = Join-Path $testRoot 'redirected.git'
        GIT_WORK_TREE = Join-Path $testRoot 'redirected-worktree'
        GIT_INDEX_FILE = Join-Path $testRoot 'redirected-index'
    }
    foreach ($entry in $hostileGitValues.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable(
            $entry.Key,
            $entry.Value,
            [EnvironmentVariableTarget]::Process
        )
    }
    Assert-ExactCoreReleaseCheckout `
        -RepositoryRoot $fixtureRepository `
        -ExpectedCommit $commitSHA
    Assert-ExactCoreReleaseCheckout `
        -RepositoryRoot $linkedWorktree `
        -ExpectedCommit $commitSHA
    Assert-ExactCoreReleaseCheckout `
        -RepositoryRoot $linkedWorktree.ToLowerInvariant() `
        -ExpectedCommit $commitSHA
    foreach ($entry in $hostileGitValues.GetEnumerator()) {
        $actual = [Environment]::GetEnvironmentVariable(
            $entry.Key,
            [EnvironmentVariableTarget]::Process
        )
        if ($actual -cne $entry.Value) {
            throw "$($entry.Key) was not restored after isolated Git"
        }
        [Environment]::SetEnvironmentVariable(
            $entry.Key,
            $null,
            [EnvironmentVariableTarget]::Process
        )
    }

    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'tracked.txt'),
        "ordinary tracked mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $fixtureRepository 'tracked mutation' 'release checkout is not clean'
    Invoke-CoreReleaseGit -Arguments @('-C', $fixtureRepository, 'checkout', '--', 'tracked.txt') | Out-Null

    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'untracked.txt'),
        "untracked mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $fixtureRepository 'untracked mutation' 'release checkout is not clean'
    Remove-Item -LiteralPath (Join-Path $fixtureRepository 'untracked.txt')

    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'update-index', '--assume-unchanged', 'tracked.txt'
    ) | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'tracked.txt'),
        "assume-unchanged mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $fixtureRepository 'assume-unchanged mutation' 'non-default Git index state'
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'update-index', '--no-assume-unchanged', 'tracked.txt'
    ) | Out-Null
    Invoke-CoreReleaseGit -Arguments @('-C', $fixtureRepository, 'checkout', '--', 'tracked.txt') | Out-Null

    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'update-index', '--skip-worktree', 'tracked.txt'
    ) | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'tracked.txt'),
        "skip-worktree mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $fixtureRepository 'skip-worktree mutation' 'non-default Git index state'
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'update-index', '--no-skip-worktree', 'tracked.txt'
    ) | Out-Null
    Invoke-CoreReleaseGit -Arguments @('-C', $fixtureRepository, 'checkout', '--', 'tracked.txt') | Out-Null

    Assert-ExactCoreReleaseFileProjection `
        -RepositoryRoot $linkedWorktree `
        -ExpectedCommit $commitSHA `
        -VerifierPaths @('tracked.txt')
    [IO.File]::WriteAllText(
        (Join-Path $linkedWorktree 'tracked.txt'),
        "linked tracked mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $linkedWorktree 'linked tracked mutation' 'release checkout is not clean'
    Invoke-CoreReleaseGit -Arguments @('-C', $linkedWorktree, 'checkout', '--', 'tracked.txt') | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $linkedWorktree 'untracked.txt'),
        "linked untracked mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $linkedWorktree 'linked untracked mutation' 'release checkout is not clean'
    Remove-Item -LiteralPath (Join-Path $linkedWorktree 'untracked.txt')
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $linkedWorktree, 'update-index', '--assume-unchanged', 'tracked.txt'
    ) | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $linkedWorktree 'tracked.txt'),
        "linked assume-unchanged mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $linkedWorktree 'linked assume-unchanged' 'non-default Git index state'
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $linkedWorktree, 'update-index', '--no-assume-unchanged', 'tracked.txt'
    ) | Out-Null
    Invoke-CoreReleaseGit -Arguments @('-C', $linkedWorktree, 'checkout', '--', 'tracked.txt') | Out-Null
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $linkedWorktree, 'update-index', '--skip-worktree', 'tracked.txt'
    ) | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $linkedWorktree 'tracked.txt'),
        "linked skip-worktree mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-CheckoutThrows $linkedWorktree 'linked skip-worktree' 'non-default Git index state'
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $linkedWorktree, 'update-index', '--no-skip-worktree', 'tracked.txt'
    ) | Out-Null
    Invoke-CoreReleaseGit -Arguments @('-C', $linkedWorktree, 'checkout', '--', 'tracked.txt') | Out-Null

    New-ExactCoreReleaseCheckout `
        -SourceRepository $linkedWorktree `
        -ExpectedCommit $commitSHA `
        -Destination $exactCheckout `
        -VerifierPaths @('tracked.txt')
    [IO.File]::WriteAllText(
        (Join-Path $linkedWorktree 'tracked.txt'),
        "late source mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        (Join-Path $linkedWorktree 'untracked.txt'),
        "late untracked mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    Assert-ExactCoreReleaseCheckout `
        -RepositoryRoot $exactCheckout `
        -ExpectedCommit $commitSHA
    $expectedObject = [string](Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'rev-parse', "${commitSHA}:tracked.txt"
    ))
    $actualObject = [string](Invoke-CoreReleaseGit -Arguments @(
        'hash-object', '--', (Join-Path $exactCheckout 'tracked.txt')
    ))
    if ($actualObject.Trim() -cne $expectedObject.Trim()) {
        throw 'private checkout consumed late source bytes'
    }
    [IO.File]::WriteAllText(
        (Join-Path $exactCheckout 'tracked.txt'),
        "post-projection mutation`n",
        [Text.UTF8Encoding]::new($false)
    )
    $postProjectionRejected = $false
    try {
        Assert-ExactCoreReleaseFileProjection `
            -RepositoryRoot $exactCheckout `
            -ExpectedCommit $commitSHA `
            -VerifierPaths @('tracked.txt')
    } catch {
        if (-not $_.Exception.Message.Contains(
            'differs from its commit blob',
            [StringComparison]::Ordinal
        )) {
            throw "post-projection mutation failed for the wrong reason: $($_.Exception.Message)"
        }
        $postProjectionRejected = $true
    }
    if (-not $postProjectionRejected) {
        throw 'post-projection mutation escaped raw revalidation'
    }

    Remove-Item -LiteralPath (Join-Path $linkedWorktree 'untracked.txt')
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository 'tracked.txt'),
        "`$Id`$`n",
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        (Join-Path $fixtureRepository '.gitattributes'),
        "tracked.txt ident`n",
        [Text.UTF8Encoding]::new($false)
    )
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'add', '--', '.gitattributes', 'tracked.txt'
    ) | Out-Null
    Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository,
        '-c', 'user.name=WindShare',
        '-c', 'user.email=release-contract.invalid',
        'commit', '--quiet', '-m', 'ident-filter'
    ) | Out-Null
    $identCommit = [string](Invoke-CoreReleaseGit -Arguments @(
        '-C', $fixtureRepository, 'rev-parse', 'HEAD'
    ))
    $identRejected = $false
    try {
        New-ExactCoreReleaseCheckout `
            -SourceRepository $fixtureRepository `
            -ExpectedCommit $identCommit.Trim() `
            -Destination (Join-Path $testRoot 'ident-checkout') `
            -VerifierPaths @('tracked.txt')
    } catch {
        if (-not $_.Exception.Message.Contains(
            'differs from its commit blob',
            [StringComparison]::Ordinal
        )) {
            throw "checkout transformation failed for the wrong reason: $($_.Exception.Message)"
        }
        $identRejected = $true
    }
    if (-not $identRejected) {
        throw 'checkout transformation passed raw verifier projection'
    }
} finally {
    foreach ($name in $gitVariableNames) {
        [Environment]::SetEnvironmentVariable(
            $name,
            $originalGitVariables[$name],
            [EnvironmentVariableTarget]::Process
        )
    }
    if (Test-Path -LiteralPath $testRoot) {
        $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
        $resolvedTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
        $ownedPrefix = Join-Path $resolvedTemp 'windshare-core-checkout-contract-'
        if (-not $resolvedRoot.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "refusing to remove unowned checkout-test path: $resolvedRoot"
        }
        Remove-Item -LiteralPath $resolvedRoot -Recurse -Force
    }
}

Write-Output 'core release checkout contract: PASS'
