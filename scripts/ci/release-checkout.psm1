Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:ReleaseVerifierPaths = @(
    'go.mod',
    'go.sum',
    'scripts/ci/_modulezip/main.go',
    'scripts/ci/native-output/windows/certify.psm1',
    'scripts/ci/native-output/windows/worker.ps1'
)

function Get-ReleaseVerifierPaths {
    return @($script:ReleaseVerifierPaths)
}

function Resolve-ReleaseGitExecutable {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [object[]]$CommandCandidates
    )

    $candidates = @($CommandCandidates)
    if ($candidates.Count -eq 0) {
        throw 'Git executable discovery returned no Application candidates'
    }

    # PATH precedence is part of command resolution. Selecting candidate zero
    # preserves it while preventing PowerShell from coercing every -All result
    # into one invalid ProcessStartInfo filename.
    $candidate = $candidates[0]
    $commandType = $candidate.PSObject.Properties['CommandType']
    if ($null -eq $commandType -or
        $commandType.Value -ne [Management.Automation.CommandTypes]::Application) {
        throw 'Git executable discovery did not select an Application command'
    }
    $source = $candidate.PSObject.Properties['Source']
    $sourceValues = @()
    if ($null -ne $source) {
        $sourceValues = @($source.Value)
    }
    if ($sourceValues.Count -ne 1 -or
        $sourceValues[0] -isnot [string] -or
        [string]::IsNullOrWhiteSpace([string]$sourceValues[0])) {
        throw 'Git Application candidate must expose exactly one executable path'
    }

    $executable = [string]$sourceValues[0]
    if (-not [IO.Path]::IsPathFullyQualified($executable)) {
        throw 'Git Application executable path must be fully qualified'
    }
    $executable = [IO.Path]::GetFullPath($executable)
    if ([IO.Path]::GetExtension($executable) -ine '.exe' -or
        -not [IO.File]::Exists($executable)) {
        throw "Git Application executable path is not an existing .exe file: $executable"
    }
    return $executable
}

function Invoke-ReleaseGit {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string[]]$Arguments
    )

    $gitCommands = @(Get-Command git -CommandType Application -All -ErrorAction Stop)
    $resolvedExecutables = @(Resolve-ReleaseGitExecutable -CommandCandidates $gitCommands)
    if ($resolvedExecutables.Count -ne 1) {
        throw 'Git executable discovery did not resolve exactly one executable'
    }
    $gitExecutable = [string]$resolvedExecutables[0]
    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $gitExecutable
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    foreach ($argument in $Arguments) {
        $startInfo.ArgumentList.Add($argument)
    }
    foreach ($name in @($startInfo.Environment.Keys)) {
        if ($name.StartsWith('GIT_', [StringComparison]::OrdinalIgnoreCase)) {
            $null = $startInfo.Environment.Remove($name)
        }
    }
    $startInfo.Environment['GIT_CONFIG_NOSYSTEM'] = '1'
    $startInfo.Environment['GIT_CONFIG_GLOBAL'] = 'NUL'
    $startInfo.Environment['GIT_NO_REPLACE_OBJECTS'] = '1'
    $startInfo.Environment['GIT_TERMINAL_PROMPT'] = '0'
    $startInfo.Environment['LC_ALL'] = 'C'

    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    try {
        if (-not $process.Start()) {
            throw 'isolated Git process did not start'
        }
        $standardOutput = $process.StandardOutput.ReadToEndAsync()
        $standardError = $process.StandardError.ReadToEndAsync()
        $process.WaitForExit()
        $output = $standardOutput.GetAwaiter().GetResult()
        $errorOutput = $standardError.GetAwaiter().GetResult().Trim()
        if ($process.ExitCode -ne 0) {
            throw "git $($Arguments -join ' ') exited with code $($process.ExitCode): $errorOutput"
        }
        $output = $output.TrimEnd([char[]]"`r`n")
        if ($output.Length -eq 0) {
            return @()
        }
        return @($output -split '\r?\n')
    } finally {
        $process.Dispose()
    }
}

function Read-ReleaseGitValue {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string[]]$Arguments,

        [Parameter(Mandatory)]
        [string]$Label
    )

    $values = @(Invoke-ReleaseGit -Arguments $Arguments)
    if ($values.Count -ne 1 -or [string]::IsNullOrWhiteSpace([string]$values[0])) {
        throw "release checkout Git $Label resolution was not singular"
    }
    return ([string]$values[0]).Trim()
}

function Resolve-ReleaseNoFollowDirectory([string]$Path, [string]$Label) {
    if (-not [IO.Path]::IsPathFullyQualified($Path) -or
        -not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "release checkout $Label is not an existing absolute directory: $Path"
    }
    $normalized = [IO.Path]::GetFullPath($Path)
    $info = Get-Item -LiteralPath $normalized -Force
    if (($info.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "release checkout $Label must not be a reparse point: $normalized"
    }
    return [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $normalized).Path)
}

function Resolve-ReleaseNoFollowFile([string]$Path, [string]$Label) {
    if (-not [IO.Path]::IsPathFullyQualified($Path) -or
        -not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "release checkout $Label is not an existing absolute file: $Path"
    }
    $normalized = [IO.Path]::GetFullPath($Path)
    $info = Get-Item -LiteralPath $normalized -Force
    if (($info.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "release checkout $Label must not be a reparse point: $normalized"
    }
    return [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $normalized).Path)
}

function Resolve-ReleaseRepositoryIdentity {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$RepositoryRoot
    )

    if (-not [IO.Path]::IsPathFullyQualified($RepositoryRoot) -or
        -not (Test-Path -LiteralPath $RepositoryRoot -PathType Container)) {
        throw 'release checkout requires an existing absolute repository root'
    }
    $normalizedRoot = [IO.Path]::GetFullPath($RepositoryRoot)
    $rootInfo = Get-Item -LiteralPath $normalizedRoot -Force
    if (($rootInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'release checkout repository root must not be a reparse point'
    }
    $resolvedRoot = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $normalizedRoot).Path)

    $gitEntry = Join-Path $resolvedRoot '.git'
    if (-not (Test-Path -LiteralPath $gitEntry -PathType Container) -and
        -not (Test-Path -LiteralPath $gitEntry -PathType Leaf)) {
        throw 'release checkout requires a Git metadata entry'
    }
    $gitInfo = Get-Item -LiteralPath $gitEntry -Force
    if (($gitInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'release checkout Git metadata must not be a reparse point'
    }

    $discoveryArguments = @(
        '-C', $resolvedRoot,
        '-c', 'core.fsmonitor=false',
        '-c', 'core.untrackedCache=false'
    )
    $insideWorkTree = Read-ReleaseGitValue `
        -Arguments ($discoveryArguments + @('rev-parse', '--is-inside-work-tree')) `
        -Label 'worktree membership'
    if ($insideWorkTree -cne 'true') {
        throw 'release checkout repository root is not inside a Git worktree'
    }

    $topLevel = Read-ReleaseGitValue `
        -Arguments ($discoveryArguments + @('rev-parse', '--show-toplevel')) `
        -Label 'top-level'
    $resolvedTopLevel = Resolve-ReleaseNoFollowDirectory $topLevel 'top-level'
    if (-not [string]::Equals(
        $resolvedTopLevel,
        $resolvedRoot,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw "release checkout top-level is $resolvedTopLevel instead of $resolvedRoot"
    }

    $gitDirectory = Resolve-ReleaseNoFollowDirectory `
        (Read-ReleaseGitValue `
            -Arguments ($discoveryArguments + @('rev-parse', '--absolute-git-dir')) `
            -Label 'Git directory') `
        'Git directory'
    $commonDirectory = Resolve-ReleaseNoFollowDirectory `
        (Read-ReleaseGitValue `
            -Arguments ($discoveryArguments + @(
                'rev-parse', '--path-format=absolute', '--git-common-dir'
            )) `
            -Label 'common Git directory') `
        'common Git directory'
    $indexPath = Resolve-ReleaseNoFollowFile `
        (Read-ReleaseGitValue `
            -Arguments ($discoveryArguments + @(
                'rev-parse', '--path-format=absolute', '--git-path', 'index'
            )) `
            -Label 'index') `
        'index'

    return [pscustomobject]@{
        RepositoryRoot = $resolvedRoot
        GitDirectory = $gitDirectory
        CommonDirectory = $commonDirectory
        IndexPath = $indexPath
    }
}

function Assert-ExactReleaseCheckout {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$RepositoryRoot,

        [Parameter(Mandatory)]
        [string]$ExpectedCommit
    )

    if ($ExpectedCommit -cnotmatch '^[0-9a-f]{40}$') {
        throw 'release checkout requires an exact lowercase 40-character SHA'
    }
    $repository = Resolve-ReleaseRepositoryIdentity -RepositoryRoot $RepositoryRoot
    # The resolved per-worktree Git directory binds every check to the index that
    # was inspected above; the common object store is validated but never guessed.
    $gitArguments = @(
        "--git-dir=$($repository.GitDirectory)",
        "--work-tree=$($repository.RepositoryRoot)",
        '-c', 'core.fsmonitor=false',
        '-c', 'core.untrackedCache=false'
    )

    $actualCommit = [string](Invoke-ReleaseGit -Arguments (
        $gitArguments + @('rev-parse', 'HEAD')
    ))
    $objectType = [string](Invoke-ReleaseGit -Arguments (
        $gitArguments + @('cat-file', '-t', $ExpectedCommit)
    ))
    if ($actualCommit.Trim() -cne $ExpectedCommit -or $objectType.Trim() -cne 'commit') {
        throw "release checkout does not directly equal commit $ExpectedCommit"
    }

    # Porcelain deliberately trusts these index bits. Rejecting every non-default
    # tag prevents assume-unchanged, skip-worktree, and fsmonitor state from
    # concealing a verifier mutation.
    foreach ($indexView in @('-v', '-f')) {
        $indexEntries = @(Invoke-ReleaseGit -Arguments (
            $gitArguments + @('ls-files', $indexView)
        ))
        foreach ($indexEntry in $indexEntries) {
            if (-not ([string]$indexEntry).StartsWith('H ', [StringComparison]::Ordinal)) {
                throw "release checkout has non-default Git index state: $indexEntry"
            }
        }
    }

    $status = @(Invoke-ReleaseGit -Arguments (
        $gitArguments + @('status', '--porcelain=v1', '--untracked-files=all')
    ))
    if ($status.Count -ne 0) {
        throw "release checkout is not clean: $($status -join '; ')"
    }
}

function Assert-ExactReleaseFileProjection {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$RepositoryRoot,

        [Parameter(Mandatory)]
        [string]$ExpectedCommit,

        [Parameter(Mandatory)]
        [ValidateNotNullOrEmpty()]
        [string[]]$VerifierPaths
    )

    $repository = Resolve-ReleaseRepositoryIdentity -RepositoryRoot $RepositoryRoot
    foreach ($relativePath in $VerifierPaths) {
        $segments = @($relativePath.Split('/'))
        if ([IO.Path]::IsPathRooted($relativePath) -or
            $relativePath.Contains('\', [StringComparison]::Ordinal) -or
            $segments.Count -eq 0 -or
            @($segments | Where-Object { $_ -ceq '' -or $_ -ceq '.' -or $_ -ceq '..' }).Count -ne 0) {
            throw "invalid exact release verifier path: $relativePath"
        }
        $filePath = Join-Path $repository.RepositoryRoot $relativePath.Replace('/', [IO.Path]::DirectorySeparatorChar)
        if (-not (Test-Path -LiteralPath $filePath -PathType Leaf)) {
            throw "exact release verifier input is not a regular file: $relativePath"
        }
        $fileInfo = Get-Item -LiteralPath $filePath -Force
        if (($fileInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "exact release verifier input is a reparse point: $relativePath"
        }
        $expectedObject = [string](Invoke-ReleaseGit -Arguments @(
            "--git-dir=$($repository.GitDirectory)",
            'rev-parse', '--verify', "${ExpectedCommit}:$relativePath"
        ))
        $actualObject = [string](Invoke-ReleaseGit -Arguments @(
            'hash-object', '--no-filters', '--', $filePath
        ))
        if ($actualObject.Trim() -cne $expectedObject.Trim()) {
            throw "exact release verifier input differs from its commit blob: $relativePath"
        }
    }
}

function New-ExactReleaseCheckout {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$SourceRepository,

        [Parameter(Mandatory)]
        [string]$ExpectedCommit,

        [Parameter(Mandatory)]
        [string]$Destination,

        [Parameter(Mandatory)]
        [ValidateNotNullOrEmpty()]
        [string[]]$VerifierPaths
    )

    if ($ExpectedCommit -cnotmatch '^[0-9a-f]{40}$') {
        throw 'exact release checkout requires an exact lowercase 40-character SHA'
    }
    if (-not [IO.Path]::IsPathFullyQualified($Destination) -or
        (Test-Path -LiteralPath $Destination)) {
        throw "exact release checkout destination must be an absent absolute path: $Destination"
    }
    $source = Resolve-ReleaseRepositoryIdentity -RepositoryRoot $SourceRepository
    Invoke-ReleaseGit -Arguments @(
        'clone', '--quiet', '--no-hardlinks', '--no-checkout', '--',
        $source.RepositoryRoot, $Destination
    ) | Out-Null
    $destinationGit = Join-Path $Destination '.git'
    Invoke-ReleaseGit -Arguments @(
        "--git-dir=$destinationGit",
        "--work-tree=$Destination",
        '-c', 'core.fsmonitor=false',
        '-c', 'core.untrackedCache=false',
        'checkout', '--quiet', '--detach', $ExpectedCommit
    ) | Out-Null
    Assert-ExactReleaseCheckout `
        -RepositoryRoot $Destination `
        -ExpectedCommit $ExpectedCommit
    Assert-ExactReleaseFileProjection `
        -RepositoryRoot $Destination `
        -ExpectedCommit $ExpectedCommit `
        -VerifierPaths $VerifierPaths
}

Export-ModuleMember -Function @(
    'Get-ReleaseVerifierPaths',
    'Invoke-ReleaseGit',
    'Assert-ExactReleaseCheckout',
    'Assert-ExactReleaseFileProjection',
    'New-ExactReleaseCheckout'
)
