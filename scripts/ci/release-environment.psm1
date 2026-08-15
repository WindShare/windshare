Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:ReleaseFixedEnvironment = [ordered]@{
    GOENV       = 'off'
    GOFLAGS     = ''
    GOTOOLCHAIN = 'local'
    GOWORK      = 'off'
    GOPROXY     = 'https://proxy.golang.org'
    GOSUMDB     = 'sum.golang.org'
    GOPRIVATE   = ''
    GONOSUMDB   = ''
    GONOPROXY   = ''
    GOINSECURE  = ''
    GOTELEMETRY = 'off'
}
$script:ReleaseClearedEnvironment = @(
    'GOOS',
    'GOARCH',
    'CGO_ENABLED',
    'GOEXPERIMENT'
)

function Set-ReleaseProcessEnvironmentValue {
    param(
        [Parameter(Mandatory)]
        [string]$Name,

        [AllowNull()]
        [object]$Value
    )

    if ($null -eq $Value) {
        # .NET may materialize a null process value as NAME= on Windows. The Env
        # provider removes the entry, which is required for Go's host defaults.
        if (Test-Path -LiteralPath "Env:$Name") {
            Remove-Item -LiteralPath "Env:$Name"
        }
        return
    }
    [Environment]::SetEnvironmentVariable(
        $Name,
        [string]$Value,
        [EnvironmentVariableTarget]::Process
    )
}

function Enter-ReleaseGoEnvironment {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$ReleaseRoot
    )

    if (-not [IO.Path]::IsPathFullyQualified($ReleaseRoot) -or
        -not (Test-Path -LiteralPath $ReleaseRoot -PathType Container)) {
        throw 'release environment requires an existing absolute release root'
    }
    $resolvedRoot = [IO.Path]::GetFullPath((Resolve-Path -LiteralPath $ReleaseRoot).Path)
    $rootInfo = Get-Item -LiteralPath $resolvedRoot -Force
    if (($rootInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'release environment root must not be a reparse point'
    }

    $cacheValues = [ordered]@{
        GOMODCACHE = Join-Path $resolvedRoot 'go-module-cache'
        GOCACHE    = Join-Path $resolvedRoot 'go-build-cache'
        GOPATH     = Join-Path $resolvedRoot 'go-path'
    }
    foreach ($cachePath in $cacheValues.Values) {
        if (Test-Path -LiteralPath $cachePath) {
            throw "release cache path must be fresh: $cachePath"
        }
    }

    $variableNames = @($cacheValues.Keys) +
        @($script:ReleaseFixedEnvironment.Keys) +
        @($script:ReleaseClearedEnvironment)
    $originalValues = [ordered]@{}
    foreach ($name in $variableNames) {
        $originalValues[$name] = [Environment]::GetEnvironmentVariable(
            $name,
            [EnvironmentVariableTarget]::Process
        )
    }

    foreach ($cachePath in $cacheValues.Values) {
        New-Item -ItemType Directory -Path $cachePath | Out-Null
    }
    foreach ($entry in $cacheValues.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable(
            $entry.Key,
            $entry.Value,
            [EnvironmentVariableTarget]::Process
        )
    }
    foreach ($entry in $script:ReleaseFixedEnvironment.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable(
            $entry.Key,
            $entry.Value,
            [EnvironmentVariableTarget]::Process
        )
    }
    # These values select a cross target or non-default compiler behavior even
    # when GOENV is disabled, so host certification requires them to be absent.
    foreach ($name in $script:ReleaseClearedEnvironment) {
        Set-ReleaseProcessEnvironmentValue -Name $name -Value $null
    }

    return [pscustomobject]@{
        ReleaseRoot    = $resolvedRoot
        CachePaths     = [pscustomobject]$cacheValues
        OriginalValues = $originalValues
        VariableNames  = $variableNames
    }
}

function Exit-ReleaseGoEnvironment {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [object]$State
    )

    foreach ($name in $State.VariableNames) {
        Set-ReleaseProcessEnvironmentValue `
            -Name $name `
            -Value $State.OriginalValues[$name]
    }
}

Export-ModuleMember -Function @(
    'Enter-ReleaseGoEnvironment',
    'Exit-ReleaseGoEnvironment'
)
