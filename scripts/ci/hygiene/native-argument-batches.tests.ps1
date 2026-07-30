Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Import-Module (Join-Path $PSScriptRoot 'native-argument-batches.psm1') -Force

function Assert-Equal([object]$Actual, [object]$Expected, [string]$Context) {
    if ([string]$Actual -cne [string]$Expected) {
        throw "$Context = $Actual, want $Expected"
    }
}

$emptyBatches = @(Split-WindowsNativeArguments -Arguments @())
Assert-Equal $emptyBatches.Count 0 'empty batch count'

# Unquoted arguments use their verbatim length plus a separator, while quoted
# arguments reserve twice their length plus delimiters. This boundary proves an
# exact fit stays together and the next argument starts a new process.
$boundaryArguments = @('abc', '1234567', 'c cc', 'd')
$boundaryBatches = @(Split-WindowsNativeArguments `
    -Arguments $boundaryArguments `
    -MaximumEncodedLength 12)
Assert-Equal $boundaryBatches.Count 3 'boundary batch count'
Assert-Equal ($boundaryBatches[0].Arguments -join ',') 'abc,1234567' 'exact-fit batch'
Assert-Equal ($boundaryBatches[1].Arguments -join ',') 'c cc' 'quoted argument batch'
Assert-Equal ($boundaryBatches[2].Arguments -join ',') 'd' 'trailing batch'
Assert-Equal (($boundaryBatches.Arguments | ForEach-Object { $_ }) -join ',') `
    ($boundaryArguments -join ',') `
    'flattened argument order'
foreach ($batch in $boundaryBatches) {
    if ($batch.EncodedLengthUpperBound -gt 12) {
        throw "batch exceeded encoded-length budget: $($batch.EncodedLengthUpperBound)"
    }
}

$oversizedRejected = $false
try {
    [void](Split-WindowsNativeArguments -Arguments @('123456789012') -MaximumEncodedLength 12)
} catch {
    $oversizedRejected = [string]$_ -match 'requires 13 encoded characters'
}
if (-not $oversizedRejected) {
    throw 'oversized native argument was not rejected'
}

# This fixture is intentionally larger than one default Windows batch. It
# protects the repository-scale case that originally made gofmt fail at launch.
$repositoryScaleArguments = @(
    0..799 | ForEach-Object {
        "core/session/representative-package-$_.go"
    }
)
$repositoryScaleBatches = @(Split-WindowsNativeArguments -Arguments $repositoryScaleArguments)
if ($repositoryScaleBatches.Count -le 1) {
    throw 'repository-scale arguments were not split'
}
Assert-Equal (($repositoryScaleBatches.Arguments | ForEach-Object { $_ }) -join ',') `
    ($repositoryScaleArguments -join ',') `
    'repository-scale argument order'

Write-Output 'Windows native argument batch tests passed'
