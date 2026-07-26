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
    'Get-WindowsNativeRequiredTestNames'
)
