Set-StrictMode -Version Latest

function New-WindShareTestRunID {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [ValidatePattern('^[a-z0-9-]+$')]
        [string]$Suite
    )

    $maximumPortableTokenBytes = 128
    $entropyByteCount = 16
    $portableTokenPattern = '^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$'
    $seed = if (Test-Path Env:WINDSHARE_TEST_RUN_ID) {
        $env:WINDSHARE_TEST_RUN_ID
    } else {
        'local'
    }
    if ($seed -notmatch $portableTokenPattern) {
        throw 'WINDSHARE_TEST_RUN_ID must be an ASCII portable token without edge punctuation'
    }

    # A caller-provided CI identity remains visible, while an explicit 128-bit
    # CSPRNG suffix prevents concurrent suite processes from becoming aliased.
    $entropyBytes = [byte[]]::new($entropyByteCount)
    [Security.Cryptography.RandomNumberGenerator]::Fill($entropyBytes)
    $entropy = -join ($entropyBytes | ForEach-Object { $_.ToString('x2') })
    $runID = '{0}-{1}-{2}' -f $seed, $Suite, $entropy
    if ($runID.Length -gt $maximumPortableTokenBytes -or $runID -notmatch $portableTokenPattern) {
        throw "WINDSHARE_TEST_RUN_ID seed is too long for the $maximumPortableTokenBytes-byte portable token contract"
    }
    return $runID
}

function Invoke-WithWindShareTestRunID {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [ValidatePattern('^[a-z0-9-]+$')]
        [string]$Suite,

        [Parameter(Mandatory)]
        [scriptblock]$Body
    )

    $hadRunID = Test-Path Env:WINDSHARE_TEST_RUN_ID
    $previousRunID = $env:WINDSHARE_TEST_RUN_ID
    $runID = New-WindShareTestRunID -Suite $Suite
    try {
        $env:WINDSHARE_TEST_RUN_ID = $runID
        & $Body $runID
    } finally {
        if ($hadRunID) {
            $env:WINDSHARE_TEST_RUN_ID = $previousRunID
        } else {
            Remove-Item Env:WINDSHARE_TEST_RUN_ID -ErrorAction SilentlyContinue
        }
    }
}

Export-ModuleMember -Function @(
    'New-WindShareTestRunID',
    'Invoke-WithWindShareTestRunID'
)
