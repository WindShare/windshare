Set-StrictMode -Version Latest

$script:WindShareMakeAuthority = $null
$script:WindShareMakefileAuthority = $null
$script:WindShareGitAuthority = $null
$script:WindShareRecipeShellAuthority = $null
$script:WindSharePwshAuthority = $null
$script:MakeSelectionVariables = @('MAKEFLAGS', 'MFLAGS', 'GNUMAKEFLAGS', 'MAKEFILES')
$script:WindShareGitProbeSHA1 = '162a4aaeeeb4392ac349fe67dc0178bc5ecaa60b'

function Enter-WindShareMakeAuthority {
    [CmdletBinding()]
    param([string]$CandidatePath)

    if ($null -ne $script:WindShareMakeAuthority) {
        throw 'WindShare Make authority may be settled only once per process'
    }
    foreach ($name in $script:MakeSelectionVariables) {
        if (Test-Path "Env:$name") {
            throw "$name must be absent until WindShare Make authority is settled"
        }
    }

    if ([string]::IsNullOrWhiteSpace($CandidatePath)) {
        $application = Get-Command make -CommandType Application | Select-Object -First 1
        if ($null -eq $application) {
            throw 'WindShare Make authority requires a GNU Make application'
        }
        $CandidatePath = $application.Source
    }
    if (-not [IO.Path]::IsPathRooted($CandidatePath)) {
        throw 'WindShare Make authority requires an absolute application path'
    }
    $candidate = [IO.Path]::GetFullPath($CandidatePath)
    $retainedStream = [IO.FileStream]::new(
        $candidate,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        [IO.FileShare]::Read
    )
    try {
        if ($retainedStream.ReadByte() -ne 0x4d -or $retainedStream.ReadByte() -ne 0x5a) {
            throw 'WindShare Make authority accepts only a native PE application'
        }
        $retainedStream.Position = 0
        $sha256 = [Security.Cryptography.SHA256]::Create()
        try {
            $applicationHash = ([BitConverter]::ToString($sha256.ComputeHash($retainedStream))).Replace('-', '').ToLowerInvariant()
        } finally {
            $sha256.Dispose()
        }
        $retainedStream.Position = 0

        $versionOutput = @(& $candidate --version)
        if ($LASTEXITCODE -ne 0 -or $versionOutput.Count -lt 1 -or
            $versionOutput[0] -cnotmatch '^GNU Make (?<Version>[0-9]+\.[0-9]+(?:\.[0-9]+)?)$') {
            throw 'WindShare Make application does not identify itself as GNU Make'
        }
        $version = $Matches['Version']
        $probeBytes = [byte[]]::new(16)
        $random = [Security.Cryptography.RandomNumberGenerator]::Create()
        try {
            $random.GetBytes($probeBytes)
        } finally {
            $random.Dispose()
        }
        $probeToken = -join ($probeBytes | ForEach-Object { $_.ToString('x2') })
        $probePath = [IO.Path]::GetTempFileName()
        try {
            $probeDocument = "windshare_make_identity:`n`t@printf '%s\n' 'windshare-make-identity:${probeToken}:`$(MAKE_VERSION)'`n"
            [IO.File]::WriteAllText($probePath, $probeDocument, [Text.UTF8Encoding]::new($false))
            $probeOutput = @(& $candidate -Rr --no-print-directory -f $probePath windshare_make_identity)
            if ($LASTEXITCODE -ne 0 -or $probeOutput.Count -ne 1 -or
                $probeOutput[0] -cne "windshare-make-identity:${probeToken}:${version}") {
                throw 'WindShare Make application failed the controlled GNU Make semantic probe'
            }
        } finally {
            Remove-Item -LiteralPath $probePath -Force -ErrorAction SilentlyContinue
        }

        $script:WindShareMakeAuthority = [pscustomobject]@{
            Executable = $candidate
            Hash = $applicationHash
            Version = $version
            RetainedStream = $retainedStream
        }
        return $script:WindShareMakeAuthority
    } catch {
        $retainedStream.Dispose()
        throw
    }
}

function Invoke-WindShareMake {
    [CmdletBinding()]
    param([Parameter(ValueFromRemainingArguments)][string[]]$MakeArguments)

    if ($null -eq $script:WindShareMakeAuthority -or -not $script:WindShareMakeAuthority.RetainedStream.CanRead -or
        $null -eq $script:WindShareMakefileAuthority -or -not $script:WindShareMakefileAuthority.RetainedStream.CanRead) {
        throw 'WindShare Make and Makefile authorities were not retained before use'
    }
    & $script:WindShareMakeAuthority.Executable -f $script:WindShareMakefileAuthority.Path @MakeArguments
}

function Enter-WindShareMakefileAuthority {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$CanonicalMakefile,
        [Parameter(Mandatory)][string]$ExpectedSHA256
    )

    if ($null -ne $script:WindShareMakefileAuthority) {
        throw 'WindShare Makefile authority may be settled only once per process'
    }
    if ($ExpectedSHA256 -cnotmatch '^[0-9a-f]{64}$' -or
        $CanonicalMakefile -cnotmatch '^\.git/[a-zA-Z0-9_-]+/Makefile$') {
        throw 'WindShare Makefile authority requires one launcher-owned safe relative snapshot and entry SHA-256'
    }
    $path = [IO.Path]::GetFullPath($CanonicalMakefile)
    $retainedStream = [IO.FileStream]::new(
        $path,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        [IO.FileShare]::Read
    )
    try {
        $actualSHA256 = Get-WindShareStreamSHA256 -Stream $retainedStream
        if ($actualSHA256 -cne $ExpectedSHA256) {
            throw 'WindShare Makefile bytes differ from the validated entry snapshot'
        }
        $script:WindShareMakefileAuthority = [pscustomobject]@{
            Path = $CanonicalMakefile
            AbsolutePath = $path
            Hash = $actualSHA256
            RetainedStream = $retainedStream
        }
        return $script:WindShareMakefileAuthority
    } catch {
        $retainedStream.Dispose()
        throw
    }
}

function Enter-WindShareGitAuthority {
    [CmdletBinding()]
    param([string]$CandidatePath)

    if ($null -ne $script:WindShareGitAuthority) {
        throw 'WindShare Git authority may be settled only once per process'
    }
    if ([string]::IsNullOrWhiteSpace($CandidatePath)) {
        $application = Get-Command git -CommandType Application | Select-Object -First 1
        if ($null -eq $application) {
            throw 'WindShare Git authority requires a Git application'
        }
        $CandidatePath = $application.Source
    }
    if (-not [IO.Path]::IsPathRooted($CandidatePath)) {
        throw 'WindShare Git authority requires an absolute application path'
    }
    $candidate = [IO.Path]::GetFullPath($CandidatePath)
    $retainedStream = [IO.FileStream]::new(
        $candidate,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        [IO.FileShare]::Read
    )
    try {
        if ($retainedStream.ReadByte() -ne 0x4d -or $retainedStream.ReadByte() -ne 0x5a) {
            throw 'WindShare Git authority accepts only a native PE application'
        }
        $retainedStream.Position = 0
        $applicationHash = Get-WindShareStreamSHA256 -Stream $retainedStream
        $versionOutput = @(& $candidate --version)
        if ($LASTEXITCODE -ne 0 -or $versionOutput.Count -ne 1 -or
            $versionOutput[0] -cnotmatch '^git version [0-9]+\.[0-9]+(?:\.[0-9]+)?(?:\.[^\s]+)?$') {
            throw 'WindShare Git application does not identify itself as Git'
        }
        $probePath = [IO.Path]::GetTempFileName()
        try {
            [IO.File]::WriteAllText(
                $probePath,
                "windshare-git-authority-v1`n",
                [Text.UTF8Encoding]::new($false)
            )
            $probeOutput = @(& $candidate hash-object -- $probePath)
            if ($LASTEXITCODE -ne 0 -or $probeOutput.Count -ne 1 -or
                $probeOutput[0] -cne $script:WindShareGitProbeSHA1) {
                throw 'WindShare Git application failed the controlled object semantic probe'
            }
        } finally {
            Remove-Item -LiteralPath $probePath -Force -ErrorAction SilentlyContinue
        }
        $script:WindShareGitAuthority = [pscustomobject]@{
            Executable = $candidate
            Hash = $applicationHash
            RetainedStream = $retainedStream
        }
        return $script:WindShareGitAuthority
    } catch {
        $retainedStream.Dispose()
        throw
    }
}

function Get-WindShareGitHeadCommit {
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$RepositoryRoot)

    if ($null -eq $script:WindShareGitAuthority -or -not $script:WindShareGitAuthority.RetainedStream.CanRead) {
        throw 'WindShare Git authority was not retained before checkout inspection'
    }
    if (-not [IO.Path]::IsPathRooted($RepositoryRoot)) {
        throw 'WindShare checkout root must be absolute'
    }
    $previousNoSystemConfig = [Environment]::GetEnvironmentVariable('GIT_CONFIG_NOSYSTEM', 'Process')
    $gitExitCode = $null
    try {
        [Environment]::SetEnvironmentVariable('GIT_CONFIG_NOSYSTEM', '1', 'Process')
        $commitOutput = @(& $script:WindShareGitAuthority.Executable --no-replace-objects `
            -C ([IO.Path]::GetFullPath($RepositoryRoot)) rev-parse --verify 'HEAD^{commit}')
        $gitExitCode = $LASTEXITCODE
    } finally {
        [Environment]::SetEnvironmentVariable('GIT_CONFIG_NOSYSTEM', $previousNoSystemConfig, 'Process')
    }
    if ($gitExitCode -ne 0 -or $commitOutput.Count -ne 1 -or
        $commitOutput[0] -cnotmatch '^[0-9a-f]{40}$') {
        throw 'WindShare Git authority did not resolve one SHA-1 commit object'
    }
    return $commitOutput[0]
}

function Get-WindShareStreamSHA256 {
    param([Parameter(Mandatory)][IO.FileStream]$Stream)

    $Stream.Position = 0
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString($sha256.ComputeHash($Stream))).Replace('-', '').ToLowerInvariant()
    } finally {
        $sha256.Dispose()
        $Stream.Position = 0
    }
}

function Enter-WindShareRecipeShellAuthority {
    [CmdletBinding()]
    param([string]$CandidatePath)

    if ($null -ne $script:WindShareRecipeShellAuthority) {
        throw 'WindShare recipe-shell authority may be settled only once per process'
    }
    if ([string]::IsNullOrWhiteSpace($CandidatePath)) {
        $application = Get-Command sh -CommandType Application | Select-Object -First 1
        if ($null -eq $application) { throw 'WindShare recipe authority requires a POSIX shell application' }
        $CandidatePath = $application.Source
    }
    $authority = Enter-WindShareLockedPEApplication -CandidatePath $CandidatePath -Label 'recipe shell'
    try {
        $probeOutput = @(& $authority.Executable -c "printf '%s' 'windshare-recipe-shell-authority-v1'")
        if ($LASTEXITCODE -ne 0 -or $probeOutput.Count -ne 1 -or
            $probeOutput[0] -cne 'windshare-recipe-shell-authority-v1') {
            throw 'WindShare recipe shell failed its controlled POSIX semantic probe'
        }
        $script:WindShareRecipeShellAuthority = $authority
        return $authority
    } catch {
        $authority.RetainedStream.Dispose()
        throw
    }
}

function Enter-WindSharePwshAuthority {
    [CmdletBinding()]
    param([string]$CandidatePath)

    if ($null -ne $script:WindSharePwshAuthority) {
        throw 'WindShare pwsh authority may be settled only once per process'
    }
    if ([string]::IsNullOrWhiteSpace($CandidatePath)) {
        $application = Get-Command pwsh -CommandType Application | Select-Object -First 1
        if ($null -eq $application) { throw 'WindShare validation requires a PowerShell Core application' }
        # Windows app-execution aliases are launch capabilities rather than
        # readable images. Resolve the native image from the child itself, then
        # retain and re-probe that exact PE for every recipe invocation.
        $resolvedPath = @(& $application.Source -NoLogo -NoProfile -NonInteractive -Command `
            "[Console]::Out.Write([Diagnostics.Process]::GetCurrentProcess().MainModule.FileName)")
        if ($LASTEXITCODE -ne 0 -or $resolvedPath.Count -ne 1 -or
            -not [IO.Path]::IsPathRooted($resolvedPath[0])) {
            throw 'WindShare pwsh bootstrap did not resolve one native application image'
        }
        $CandidatePath = $resolvedPath[0]
    }
    $authority = Enter-WindShareLockedPEApplication -CandidatePath $CandidatePath -Label 'pwsh'
    try {
        $probeOutput = @(& $authority.Executable -NoLogo -NoProfile -NonInteractive -Command `
            "[Console]::Out.Write('windshare-pwsh-authority-v1')")
        if ($LASTEXITCODE -ne 0 -or $probeOutput.Count -ne 1 -or
            $probeOutput[0] -cne 'windshare-pwsh-authority-v1') {
            throw 'WindShare pwsh authority failed its controlled PowerShell Core semantic probe'
        }
        $script:WindSharePwshAuthority = $authority
        return $authority
    } catch {
        $authority.RetainedStream.Dispose()
        throw
    }
}

function Enter-WindShareLockedPEApplication {
    param(
        [Parameter(Mandatory)][string]$CandidatePath,
        [Parameter(Mandatory)][string]$Label
    )

    if (-not [IO.Path]::IsPathRooted($CandidatePath)) {
        throw "WindShare $Label authority requires an absolute application path"
    }
    $candidate = [IO.Path]::GetFullPath($CandidatePath)
    if ($candidate -match '[\r\n`"$]') {
        throw "WindShare $Label path cannot be represented as inert Make recipe data"
    }
    $retainedStream = [IO.FileStream]::new(
        $candidate,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        [IO.FileShare]::Read
    )
    try {
        if ($retainedStream.ReadByte() -ne 0x4d -or $retainedStream.ReadByte() -ne 0x5a) {
            throw "WindShare $Label authority accepts only a native PE application"
        }
        $retainedStream.Position = 0
        return [pscustomobject]@{
            Executable = $candidate
            Hash = Get-WindShareStreamSHA256 -Stream $retainedStream
            RetainedStream = $retainedStream
        }
    } catch {
        $retainedStream.Dispose()
        throw
    }
}

function Exit-WindShareMakeAuthority {
    if ($null -ne $script:WindSharePwshAuthority) {
        $script:WindSharePwshAuthority.RetainedStream.Dispose()
        $script:WindSharePwshAuthority = $null
    }
    if ($null -ne $script:WindShareRecipeShellAuthority) {
        $script:WindShareRecipeShellAuthority.RetainedStream.Dispose()
        $script:WindShareRecipeShellAuthority = $null
    }
    if ($null -ne $script:WindShareGitAuthority) {
        $script:WindShareGitAuthority.RetainedStream.Dispose()
        $script:WindShareGitAuthority = $null
    }
    if ($null -ne $script:WindShareMakefileAuthority) {
        $script:WindShareMakefileAuthority.RetainedStream.Dispose()
        $script:WindShareMakefileAuthority = $null
    }
    if ($null -ne $script:WindShareMakeAuthority) {
        $script:WindShareMakeAuthority.RetainedStream.Dispose()
        $script:WindShareMakeAuthority = $null
    }
}

Export-ModuleMember -Function @(
    'Enter-WindShareMakeAuthority',
    'Enter-WindShareMakefileAuthority',
    'Enter-WindShareGitAuthority',
    'Enter-WindShareRecipeShellAuthority',
    'Enter-WindSharePwshAuthority',
    'Get-WindShareGitHeadCommit',
    'Invoke-WindShareMake',
    'Exit-WindShareMakeAuthority'
)
