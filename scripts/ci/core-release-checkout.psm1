Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:CoreReleaseVerifierPaths = @(
    'go.mod',
    'go.sum',
    'scripts/ci/_coremodulezip/main.go',
    'scripts/ci/_corevulnerability/main.go',
    'scripts/ci/core-release-windows-native.psm1',
    'scripts/ci/core-release-windows-native-worker.ps1'
)

function Get-CoreReleaseVerifierPaths {
    return @($script:CoreReleaseVerifierPaths)
}

function Resolve-CoreReleaseGitExecutable {
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

function Invoke-CoreReleaseGit {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string[]]$Arguments
    )

    $gitCommands = @(Get-Command git -CommandType Application -All -ErrorAction Stop)
    $resolvedExecutables = @(Resolve-CoreReleaseGitExecutable -CommandCandidates $gitCommands)
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

function Assert-ExactCoreReleaseCheckout {
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
    if (-not [IO.Path]::IsPathFullyQualified($RepositoryRoot) -or
        -not (Test-Path -LiteralPath $RepositoryRoot -PathType Container)) {
        throw 'release checkout requires an existing absolute repository root'
    }
    $resolvedRoot = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $RepositoryRoot).Path)
    $rootInfo = Get-Item -LiteralPath $resolvedRoot -Force
    if (($rootInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'release checkout repository root must not be a reparse point'
    }
    $gitDirectory = Join-Path $resolvedRoot '.git'
    if (-not (Test-Path -LiteralPath $gitDirectory -PathType Container)) {
        throw 'release checkout requires a standalone Git directory'
    }
    $gitInfo = Get-Item -LiteralPath $gitDirectory -Force
    if (($gitInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'release checkout Git directory must not be a reparse point'
    }
    $gitArguments = @(
        "--git-dir=$gitDirectory",
        "--work-tree=$resolvedRoot",
        '-c', 'core.fsmonitor=false',
        '-c', 'core.untrackedCache=false'
    )

    $actualCommit = [string](Invoke-CoreReleaseGit -Arguments (
        $gitArguments + @('rev-parse', 'HEAD')
    ))
    $objectType = [string](Invoke-CoreReleaseGit -Arguments (
        $gitArguments + @('cat-file', '-t', $ExpectedCommit)
    ))
    if ($actualCommit.Trim() -cne $ExpectedCommit -or $objectType.Trim() -cne 'commit') {
        throw "release checkout does not directly equal commit $ExpectedCommit"
    }

    # Porcelain deliberately trusts these index bits. Rejecting every non-default
    # tag prevents assume-unchanged, skip-worktree, and fsmonitor state from
    # concealing a verifier mutation.
    foreach ($indexView in @('-v', '-f')) {
        $indexEntries = @(Invoke-CoreReleaseGit -Arguments (
            $gitArguments + @('ls-files', $indexView)
        ))
        foreach ($indexEntry in $indexEntries) {
            if (-not ([string]$indexEntry).StartsWith('H ', [StringComparison]::Ordinal)) {
                throw "release checkout has non-default Git index state: $indexEntry"
            }
        }
    }

    $status = @(Invoke-CoreReleaseGit -Arguments (
        $gitArguments + @('status', '--porcelain=v1', '--untracked-files=all')
    ))
    if ($status.Count -ne 0) {
        throw "release checkout is not clean: $($status -join '; ')"
    }
}

function Assert-ExactCoreReleaseFileProjection {
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

    foreach ($relativePath in $VerifierPaths) {
        $segments = @($relativePath.Split('/'))
        if ([IO.Path]::IsPathRooted($relativePath) -or
            $relativePath.Contains('\', [StringComparison]::Ordinal) -or
            $segments.Count -eq 0 -or
            @($segments | Where-Object { $_ -ceq '' -or $_ -ceq '.' -or $_ -ceq '..' }).Count -ne 0) {
            throw "invalid exact release verifier path: $relativePath"
        }
        $filePath = Join-Path $RepositoryRoot $relativePath.Replace('/', [IO.Path]::DirectorySeparatorChar)
        if (-not (Test-Path -LiteralPath $filePath -PathType Leaf)) {
            throw "exact release verifier input is not a regular file: $relativePath"
        }
        $fileInfo = Get-Item -LiteralPath $filePath -Force
        if (($fileInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "exact release verifier input is a reparse point: $relativePath"
        }
        $expectedObject = [string](Invoke-CoreReleaseGit -Arguments @(
            "--git-dir=$(Join-Path $RepositoryRoot '.git')",
            'rev-parse', '--verify', "${ExpectedCommit}:$relativePath"
        ))
        $actualObject = [string](Invoke-CoreReleaseGit -Arguments @(
            'hash-object', '--no-filters', '--', $filePath
        ))
        if ($actualObject.Trim() -cne $expectedObject.Trim()) {
            throw "exact release verifier input differs from its commit blob: $relativePath"
        }
    }
}

function New-ExactCoreReleaseCheckout {
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
    Invoke-CoreReleaseGit -Arguments @(
        'clone', '--quiet', '--no-hardlinks', '--no-checkout', '--',
        $SourceRepository, $Destination
    ) | Out-Null
    $destinationGit = Join-Path $Destination '.git'
    Invoke-CoreReleaseGit -Arguments @(
        "--git-dir=$destinationGit",
        "--work-tree=$Destination",
        '-c', 'core.fsmonitor=false',
        '-c', 'core.untrackedCache=false',
        'checkout', '--quiet', '--detach', $ExpectedCommit
    ) | Out-Null
    Assert-ExactCoreReleaseCheckout `
        -RepositoryRoot $Destination `
        -ExpectedCommit $ExpectedCommit
    Assert-ExactCoreReleaseFileProjection `
        -RepositoryRoot $Destination `
        -ExpectedCommit $ExpectedCommit `
        -VerifierPaths $VerifierPaths
}

Export-ModuleMember -Function @(
    'Get-CoreReleaseVerifierPaths',
    'Invoke-CoreReleaseGit',
    'Assert-ExactCoreReleaseCheckout',
    'Assert-ExactCoreReleaseFileProjection',
    'New-ExactCoreReleaseCheckout'
)
