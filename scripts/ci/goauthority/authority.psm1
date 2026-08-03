Set-StrictMode -Version Latest

$script:WindShareGoAuthority = $null
$script:WindShareStabilityHelperSemantics = '{"schema_version":"windshare.stability-helper-semantics/v1","operating_system":"windows","role":"go-executable-authority","revision":1,"command_plan":["reject-ambient-and-persisted-selection","retain-native-cmd-go","invoke-retained-go-with-owned-environment"]}'
$script:GoSelectionVariables = @('GOFLAGS', 'GOWORK', 'GOOS', 'GOARCH', 'GOENV', 'GOTOOLCHAIN', 'GOROOT')
$script:GoAuthorityVariables = @(
    'WINDSHARE_GO_EXECUTABLE',
    'WINDSHARE_GO_AUTHORITY_ACTIVE',
    'WINDSHARE_GO_HOST_OS',
    'WINDSHARE_GO_HOST_ARCH'
)

# Hosted runners export GOTOOLCHAIN=local through actions/setup-go for every
# step, and the retained Go is always invoked with that same value. Ambient
# local is therefore the owned default rather than caller selection; any other
# GOTOOLCHAIN value remains rejected alongside every other selection variable.
function Test-OwnedGoSelectionDefault {
    param([Parameter(Mandatory)][string]$Name)

    return ($Name -ceq 'GOTOOLCHAIN') -and (Test-Path 'Env:GOTOOLCHAIN') -and ($env:GOTOOLCHAIN -ceq 'local')
}

function Clear-ProcessEnvironmentVariable {
    param([Parameter(Mandatory)][string]$Name)

    if (Test-Path "Env:$Name") {
        Remove-Item "Env:$Name"
    }
}

function Get-PersistedGoEnvironmentPath {
    $configurationRoot = [Environment]::GetFolderPath([Environment+SpecialFolder]::ApplicationData)
    if ([string]::IsNullOrWhiteSpace($configurationRoot)) {
        throw 'WindShare Go authority cannot locate the persisted Go environment'
    }
    return Join-Path $configurationRoot 'go\env'
}

function Assert-NoPersistedGoSelection {
    param([Parameter(Mandatory)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return
    }
    foreach ($line in [IO.File]::ReadAllLines($Path)) {
        if ($line -match '^(GOFLAGS|GOWORK|GOOS|GOARCH|GOENV|GOTOOLCHAIN|GOROOT)=') {
            $name = $Matches[1]
            if ($name -ceq 'GOTOOLCHAIN' -and $line -cmatch '^GOTOOLCHAIN=local$') {
                continue
            }
            throw "$name must not be persisted outside WindShare Go authority"
        }
    }
}

function Invoke-WithOwnedGoEnvironment {
    param(
        [Parameter(Mandatory)]
        [scriptblock]$Body
    )

    $ownedNames = @('GOENV', 'GOTOOLCHAIN', 'GOROOT', 'WINDSHARE_GO_EXECUTABLE', 'PATH')
    $previous = @{}
    foreach ($name in $ownedNames) {
        $previous[$name] = [pscustomobject]@{
            Exists = Test-Path "Env:$name"
            Value = [Environment]::GetEnvironmentVariable($name, [EnvironmentVariableTarget]::Process)
        }
    }
    try {
        $env:GOENV = 'off'
        $env:GOTOOLCHAIN = 'local'
        Clear-ProcessEnvironmentVariable 'GOROOT'
        $env:WINDSHARE_GO_EXECUTABLE = $script:WindShareGoAuthority.Executable
        $env:PATH = "$($script:WindShareGoAuthority.BinDirectory)$([IO.Path]::PathSeparator)$($previous['PATH'].Value)"
        & $Body
    } finally {
        foreach ($name in $ownedNames) {
            if ($previous[$name].Exists) {
                [Environment]::SetEnvironmentVariable($name, $previous[$name].Value, [EnvironmentVariableTarget]::Process)
            } else {
                Clear-ProcessEnvironmentVariable $name
            }
        }
    }
}

function Enter-WindShareGoAuthority {
    [CmdletBinding()]
    param()

    if ($null -ne $script:WindShareGoAuthority) {
        throw 'WindShare Go authority may be settled only once per entrypoint'
    }
    foreach ($name in @($script:GoSelectionVariables) + @($script:GoAuthorityVariables)) {
        if ((Test-Path "Env:$name") -and -not (Test-OwnedGoSelectionDefault $name)) {
            throw "$name must be absent until WindShare Go authority is settled"
        }
    }

    $persistedPath = Get-PersistedGoEnvironmentPath
    Assert-NoPersistedGoSelection $persistedPath

    $application = Get-Command go -CommandType Application | Select-Object -First 1
    if ($null -eq $application -or -not [IO.Path]::IsPathFullyQualified($application.Source)) {
        throw 'WindShare Go authority requires one real executable Go application'
    }
    $candidate = [IO.Path]::GetFullPath($application.Source)
    $retainedStream = [IO.FileStream]::new(
        $candidate,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        [IO.FileShare]::Read
    )
    try {
        if ($retainedStream.ReadByte() -ne 0x4d -or $retainedStream.ReadByte() -ne 0x5a) {
            throw 'WindShare Go authority accepts only a native PE Go application'
        }
        $retainedStream.Position = 0
        $sha256 = [Security.Cryptography.SHA256]::Create()
        try {
            $applicationHash = [Convert]::ToHexString($sha256.ComputeHash($retainedStream)).ToLowerInvariant()
        } finally {
            $sha256.Dispose()
        }
        $retainedStream.Position = 0

        $previousEnvironment = @{}
        foreach ($name in $script:GoSelectionVariables) {
            $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        }
        try {
            foreach ($name in @('GOFLAGS', 'GOWORK', 'GOOS', 'GOARCH', 'GOROOT')) {
                Clear-ProcessEnvironmentVariable $name
            }
            $env:GOENV = 'off'
            $env:GOTOOLCHAIN = 'local'
            $metadata = @(& $candidate env GOROOT GOHOSTOS GOHOSTARCH)
            if ($LASTEXITCODE -ne 0 -or $metadata.Count -ne 3) {
                throw "Go host metadata exited with code $LASTEXITCODE"
            }
            $moduleIdentity = @(& $candidate version -m $candidate)
            if ($LASTEXITCODE -ne 0 -or -not ($moduleIdentity | Where-Object { $_ -match '^\s*path\s+cmd/go\s*$' })) {
                throw 'WindShare Go application does not identify itself as cmd/go'
            }
        } finally {
            foreach ($name in $script:GoSelectionVariables) {
                if ($null -eq $previousEnvironment[$name]) {
                    Clear-ProcessEnvironmentVariable $name
                } else {
                    [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process')
                }
            }
        }

        $goRoot = $metadata[0].Trim()
        $hostOS = $metadata[1].Trim()
        $hostArchitecture = $metadata[2].Trim()
        $expectedExecutable = [IO.Path]::GetFullPath((Join-Path $goRoot 'bin\go.exe'))
        if (-not [StringComparer]::OrdinalIgnoreCase.Equals($candidate, $expectedExecutable)) {
            throw 'WindShare Go application is not the bin\go.exe reported by its GOROOT'
        }
        if ($hostOS -cne 'windows') {
            throw "Windows validation requires a Windows Go host, received: $hostOS"
        }
        if ($hostArchitecture -cnotmatch '^[a-z0-9]+$') {
            throw 'WindShare Go host architecture is invalid'
        }

        $script:WindShareGoAuthority = [pscustomobject]@{
            Executable = $candidate
            BinDirectory = [IO.Path]::GetDirectoryName($candidate)
            Hash = $applicationHash
            HostOS = $hostOS
            HostArchitecture = $hostArchitecture
            RetainedStream = $retainedStream
        }
        return $script:WindShareGoAuthority
    } catch {
        $retainedStream.Dispose()
        throw
    }
}

function Assert-WindShareGoAuthorityActive {
    [CmdletBinding()]
    param()

    if ($null -eq $script:WindShareGoAuthority -or -not $script:WindShareGoAuthority.RetainedStream.CanRead) {
        throw 'WindShare Go authority was not retained before use'
    }
}

function Get-WindShareGoAuthority {
    [CmdletBinding()]
    param()

    Assert-WindShareGoAuthorityActive
    return $script:WindShareGoAuthority
}

function Invoke-WindShareGo {
    Assert-WindShareGoAuthorityActive
    $goArguments = @($args)
    Invoke-WithOwnedGoEnvironment { & $script:WindShareGoAuthority.Executable @goArguments }
}

# Go hides stdout from successful tests in ordinary mode. Central ownership of
# `test -json` keeps scenario JSONL visible without a buffering child process.
function Invoke-WindShareGoTestJSON {
    $testArguments = @($args)
    foreach ($name in $script:GoSelectionVariables) {
        if ((Test-Path "Env:$name") -and -not (Test-OwnedGoSelectionDefault $name)) {
            throw "$name must be absent when invoking Go JSON tests"
        }
    }
    if ($testArguments.Count -eq 0) {
        throw 'Go JSON test invocation requires an explicit test selection'
    }
    foreach ($argument in $testArguments) {
        $normalized = ([string]$argument).ToLowerInvariant()
        if ($normalized -eq 'test' -or $normalized -eq '-json' -or $normalized -eq '--json' -or
            $normalized.StartsWith('-json=', [StringComparison]::Ordinal) -or
            $normalized.StartsWith('--json=', [StringComparison]::Ordinal)) {
            throw "Go JSON test invocation owns the $argument argument"
        }
    }
    Invoke-WindShareGo test -json @testArguments
}

function Invoke-WindShareGoConsumer {
    Assert-WindShareGoAuthorityActive
    if ($args.Count -eq 0) {
        throw 'WindShare Go consumer requires a command'
    }
    $command = [string]$args[0]
    $commandArguments = @($args | Select-Object -Skip 1)
    Invoke-WithOwnedGoEnvironment { & $command @commandArguments }
}

Export-ModuleMember -Function @(
    'Enter-WindShareGoAuthority',
    'Assert-WindShareGoAuthorityActive',
    'Get-WindShareGoAuthority',
    'Invoke-WindShareGo',
    'Invoke-WindShareGoTestJSON',
    'Invoke-WindShareGoConsumer'
)
