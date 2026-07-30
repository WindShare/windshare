Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# CreateProcess limits the complete Windows command line to 32,767 UTF-16 code
# units. Reserving 4 KiB for the executable, fixed options, and host-specific
# serialization keeps dynamic batches safe without multiplying expensive
# workspace-wide analyzer startups.
$script:WindowsCommandLineLimit = 32767
$script:ReservedCommandLineCapacity = 4KB
$script:DefaultDynamicArgumentBudget =
    $script:WindowsCommandLineLimit - $script:ReservedCommandLineCapacity

function Get-NativeArgumentEncodedLengthUpperBound {
    param(
        [Parameter(Mandatory)]
        [AllowEmptyString()]
        [string]$Argument
    )

    $requiresQuoting = $Argument.Length -eq 0 -or $Argument.Contains('"')
    if (-not $requiresQuoting) {
        foreach ($character in $Argument.GetEnumerator()) {
            if ([char]::IsWhiteSpace($character)) {
                $requiresQuoting = $true
                break
            }
        }
    }
    if ($requiresQuoting) {
        # Doubling the full quoted argument bounds the Windows backslash-before-
        # quote rules without relying on PowerShell's current serialization mode.
        return ([long]$Argument.Length * 2) + 3
    }
    # Unquoted arguments are serialized verbatim; one extra unit accounts for
    # the separator before the next argument.
    return ([long]$Argument.Length) + 1
}

function Split-WindowsNativeArguments {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [AllowEmptyCollection()]
        [string[]]$Arguments,

        [ValidateRange(1, [int]::MaxValue)]
        [int]$MaximumEncodedLength = $script:DefaultDynamicArgumentBudget
    )

    $batches = [Collections.Generic.List[object]]::new()
    $currentArguments = [Collections.Generic.List[string]]::new()
    $currentEncodedLength = 0

    foreach ($argument in $Arguments) {
        $encodedLength = Get-NativeArgumentEncodedLengthUpperBound -Argument $argument
        if ($encodedLength -gt $MaximumEncodedLength) {
            throw "Native argument requires $encodedLength encoded characters; batch budget is $MaximumEncodedLength"
        }

        if ($currentArguments.Count -gt 0 -and
            $currentEncodedLength + $encodedLength -gt $MaximumEncodedLength) {
            $batches.Add([pscustomobject]@{
                Arguments               = $currentArguments.ToArray()
                EncodedLengthUpperBound = $currentEncodedLength
            })
            $currentArguments = [Collections.Generic.List[string]]::new()
            $currentEncodedLength = 0
        }

        $currentArguments.Add($argument)
        $currentEncodedLength += $encodedLength
    }

    if ($currentArguments.Count -gt 0) {
        $batches.Add([pscustomobject]@{
            Arguments               = $currentArguments.ToArray()
            EncodedLengthUpperBound = $currentEncodedLength
        })
    }

    return @($batches)
}

Export-ModuleMember -Function 'Split-WindowsNativeArguments'
