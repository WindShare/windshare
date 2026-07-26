Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:RequiredWindowsNativeTests = @(
    'TestWindowsNTFSNativeCertification',
    'TestWindowsNTFSProcessRestartRecovery',
    'TestWindowsNTFSProbeMutexIsProcessExclusiveAndRecoversAbandonment'
)
$script:AdministratorsSID = 'S-1-5-32-544'
$script:UsersSID = 'S-1-5-32-545'
$script:ForbiddenServiceSIDs = @(
    'S-1-5-18',
    'S-1-5-19',
    'S-1-5-20'
)
# CreateProcessWithLogonW limits lpCommandLine to 1024 UTF-16 characters.
# Reserve one character for its terminating NUL so every accepted launch is
# inside the documented boundary rather than relying on edge interpretation.
$script:MaximumCredentialCommandLineCharacters = 1023

function ConvertTo-WindowsNativeArgument([string]$Value) {
    $argument = [Text.StringBuilder]::new($Value.Length + 2)
    [void]$argument.Append('"')
    $backslashCount = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -ceq [char]'\') {
            $backslashCount++
            continue
        }
        if ($character -ceq [char]'"') {
            if ($backslashCount -gt 0) {
                [void]$argument.Append(('\' * ($backslashCount * 2)))
                $backslashCount = 0
            }
            [void]$argument.Append('\"')
            continue
        }
        if ($backslashCount -gt 0) {
            [void]$argument.Append(('\' * $backslashCount))
            $backslashCount = 0
        }
        [void]$argument.Append($character)
    }
    # Backslashes before the closing quote must be doubled so Windows argv
    # parsing preserves a trailing directory separator instead of escaping it.
    if ($backslashCount -gt 0) {
        [void]$argument.Append(('\' * ($backslashCount * 2)))
    }
    [void]$argument.Append('"')
    return $argument.ToString()
}

function Assert-WindowsNativeWorkerArgumentValue([string]$Value, [string]$Label) {
    if ([string]::IsNullOrWhiteSpace($Value)) {
        throw "$Label is empty"
    }
    foreach ($character in $Value.ToCharArray()) {
        if ([char]::IsControl($character)) {
            throw "$Label contains a control character"
        }
    }
    # These inputs are filesystem paths or a SID, none of which can contain a
    # double quote on Windows. Rejecting one exposes malformed launch state
    # instead of allowing it to acquire command-line syntax accidentally.
    if ($Value.Contains('"', [StringComparison]::Ordinal)) {
        throw "$Label contains a double quote"
    }
}

function New-WindowsNativeWorkerArgumentLine {
    [CmdletBinding()]
    [OutputType([string])]
    param(
        [Parameter(Mandatory)]
        [string]$PowerShellExecutable,

        [Parameter(Mandatory)]
        [string]$WorkerScript,

        [Parameter(Mandatory)]
        [string]$ArtifactRoot,

        [Parameter(Mandatory)]
        [string]$WorkRoot,

        [Parameter(Mandatory)]
        [string]$GoExecutable,

        [Parameter(Mandatory)]
        [string]$ExpectedUserSID
    )

    $pathArguments = @(
        [pscustomobject]@{ Label = 'PowerShell executable'; Value = $PowerShellExecutable },
        [pscustomobject]@{ Label = 'native worker script'; Value = $WorkerScript },
        [pscustomobject]@{ Label = 'extracted artifact root'; Value = $ArtifactRoot },
        [pscustomobject]@{ Label = 'native worker root'; Value = $WorkRoot },
        [pscustomobject]@{ Label = 'Go executable'; Value = $GoExecutable }
    )
    foreach ($pathArgument in $pathArguments) {
        Assert-WindowsNativeWorkerArgumentValue `
            -Value $pathArgument.Value `
            -Label $pathArgument.Label
        if (-not [IO.Path]::IsPathFullyQualified($pathArgument.Value)) {
            throw "$($pathArgument.Label) must be an absolute path"
        }
    }
    Assert-WindowsNativeWorkerArgumentValue `
        -Value $ExpectedUserSID `
        -Label 'expected native worker SID'
    if ($ExpectedUserSID -cnotmatch '^S-[0-9]+(?:-[0-9]+)+$') {
        throw 'expected native worker SID is malformed'
    }

    $argumentLine = @(
        '-NoLogo',
        '-NoProfile',
        '-NonInteractive',
        '-ExecutionPolicy',
        'Bypass',
        '-File',
        (ConvertTo-WindowsNativeArgument $WorkerScript),
        '-ArtifactRoot',
        (ConvertTo-WindowsNativeArgument $ArtifactRoot),
        '-WorkRoot',
        (ConvertTo-WindowsNativeArgument $WorkRoot),
        '-GoExecutable',
        (ConvertTo-WindowsNativeArgument $GoExecutable),
        '-ExpectedUserSID',
        (ConvertTo-WindowsNativeArgument $ExpectedUserSID)
    ) -join ' '
    $fullCommandLine = '{0} {1}' -f @(
        (ConvertTo-WindowsNativeArgument $PowerShellExecutable),
        $argumentLine
    )
    if ($fullCommandLine.Length -gt $script:MaximumCredentialCommandLineCharacters) {
        throw ('standard-user native worker command line is {0} characters; ' +
            'CreateProcessWithLogonW permits at most {1} before its terminating NUL') -f @(
                $fullCommandLine.Length,
                $script:MaximumCredentialCommandLineCharacters
            )
    }
    return $argumentLine
}

function Get-WindowsNativeRequiredTestNames {
    return @($script:RequiredWindowsNativeTests)
}

function Get-WindowsNativeRequiredTestExpression {
    return '^({0})$' -f (($script:RequiredWindowsNativeTests | ForEach-Object {
        [Regex]::Escape($_)
    }) -join '|')
}

function Get-WindowsNativeEventField([object]$Event, [string]$Name) {
    $property = $Event.PSObject.Properties[$Name]
    if ($null -eq $property -or $null -eq $property.Value) {
        return ''
    }
    return [string]$property.Value
}

function Assert-WindowsNativeReadOnlyDirectory {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$Path,

        [Parameter(Mandatory)]
        [string]$Label
    )

    $probePath = Join-Path $Path ('write-denial-{0}.probe' -f [Guid]::NewGuid().ToString('N'))
    $probeStream = $null
    $creationDenied = $false
    try {
        try {
            $probeStream = [IO.File]::Open(
                $probePath,
                [IO.FileMode]::CreateNew,
                [IO.FileAccess]::Write,
                [IO.FileShare]::None
            )
        } catch [UnauthorizedAccessException] {
            $creationDenied = $true
        } catch {
            throw "$Label write-denial probe failed for an unexpected reason: $($_.Exception.Message)"
        }

        if ($creationDenied) {
            return
        }
        throw "$Label is writable by the native worker token"
    } finally {
        if ($null -ne $probeStream) {
            $probeStream.Dispose()
        }
        if (Test-Path -LiteralPath $probePath) {
            Remove-Item -LiteralPath $probePath -Force
        }
    }
}

function ConvertFrom-WindowsNativeTestJSONLines {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [AllowEmptyString()]
        [string[]]$Lines
    )

    $events = [Collections.Generic.List[object]]::new()
    for ($lineIndex = 0; $lineIndex -lt $Lines.Count; $lineIndex++) {
        if ([string]::IsNullOrWhiteSpace($Lines[$lineIndex])) {
            throw "required Windows/NTFS native tests emitted an empty JSON line at index $lineIndex"
        }
        try {
            $event = ConvertFrom-Json -InputObject $Lines[$lineIndex] -ErrorAction Stop
        } catch {
            throw "required Windows/NTFS native tests emitted invalid JSON at line index $lineIndex"
        }
        $events.Add($event)
    }
    return @($events)
}

function Assert-WindowsNativeStandardUserIdentity {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string]$ActualUserSID,

        [Parameter(Mandatory)]
        [string]$ExpectedUserSID,

        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [string[]]$GroupSIDs,

        [Parameter(Mandatory)]
        [bool]$IsAdministrator
    )

    if ($ActualUserSID -cne $ExpectedUserSID) {
        throw "native worker token SID $ActualUserSID does not match its ephemeral account SID"
    }
    if ($ActualUserSID -notmatch '^S-[0-9]+(?:-[0-9]+)+$') {
        throw 'native worker token has a malformed user SID'
    }
    if ($ActualUserSID -in $script:ForbiddenServiceSIDs -or $ActualUserSID -match '-500$') {
        throw 'native worker must not use a built-in administrator or service identity'
    }
    if ($IsAdministrator -or $GroupSIDs -contains $script:AdministratorsSID) {
        throw 'native worker token must not carry the built-in Administrators group'
    }
    if ($GroupSIDs -notcontains $script:UsersSID) {
        throw 'native worker token does not carry the built-in Users group'
    }
}

function Assert-WindowsNativeTestEvents {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [int]$ExitCode,

        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [object[]]$Events,

        [string[]]$RequiredTests = $script:RequiredWindowsNativeTests
    )

    if ($ExitCode -ne 0) {
        throw "required Windows/NTFS native tests exited with code $ExitCode"
    }
    if ($Events.Count -eq 0) {
        throw 'required Windows/NTFS native tests produced no JSON events'
    }

    $skippedEvents = @($Events | Where-Object {
        (Get-WindowsNativeEventField $_ 'Action') -ceq 'skip'
    })
    if ($skippedEvents.Count -ne 0) {
        $skipNames = @($skippedEvents | ForEach-Object {
            $testName = Get-WindowsNativeEventField $_ 'Test'
            $packageName = Get-WindowsNativeEventField $_ 'Package'
            if (-not [string]::IsNullOrWhiteSpace($testName)) {
                $testName
            } elseif (-not [string]::IsNullOrWhiteSpace($packageName)) {
                $packageName
            } else {
                '<unknown>'
            }
        })
        throw "required Windows/NTFS native test suite reported SKIP: $($skipNames -join ', ')"
    }

    $failedEvents = @($Events | Where-Object {
        (Get-WindowsNativeEventField $_ 'Action') -ceq 'fail'
    })
    if ($failedEvents.Count -ne 0) {
        $failureNames = @($failedEvents | ForEach-Object {
            $testName = Get-WindowsNativeEventField $_ 'Test'
            $packageName = Get-WindowsNativeEventField $_ 'Package'
            if (-not [string]::IsNullOrWhiteSpace($testName)) {
                $testName
            } elseif (-not [string]::IsNullOrWhiteSpace($packageName)) {
                $packageName
            } else {
                '<unknown>'
            }
        })
        throw "required Windows/NTFS native test suite reported FAIL: $($failureNames -join ', ')"
    }

    foreach ($requiredTest in $RequiredTests) {
        $topLevelRuns = @($Events | Where-Object {
            (Get-WindowsNativeEventField $_ 'Action') -ceq 'run' -and
                (Get-WindowsNativeEventField $_ 'Test') -ceq $requiredTest
        })
        if ($topLevelRuns.Count -ne 1) {
            throw "required native test reported $($topLevelRuns.Count) top-level RUN events, want exactly one: $requiredTest"
        }

        $topLevelPasses = @($Events | Where-Object {
            (Get-WindowsNativeEventField $_ 'Action') -ceq 'pass' -and
                (Get-WindowsNativeEventField $_ 'Test') -ceq $requiredTest
        })
        if ($topLevelPasses.Count -ne 1) {
            throw "required native test reported $($topLevelPasses.Count) top-level PASS events, want exactly one: $requiredTest"
        }
    }
}

Export-ModuleMember -Function @(
    'Assert-WindowsNativeReadOnlyDirectory',
    'Assert-WindowsNativeStandardUserIdentity',
    'Assert-WindowsNativeTestEvents',
    'ConvertFrom-WindowsNativeTestJSONLines',
    'Get-WindowsNativeRequiredTestExpression',
    'Get-WindowsNativeRequiredTestNames',
    'New-WindowsNativeWorkerArgumentLine'
)
