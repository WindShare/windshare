Set-StrictMode -Version Latest

function Get-R8SummaryOperation(
    [object]$Operations,
    [string]$Name,
    [scriptblock]$Default
) {
    if ($null -eq $Operations) {
        return $Default
    }
    $property = $Operations.PSObject.Properties[$Name]
    if ($null -eq $property -or $null -eq $property.Value) {
        return $Default
    }
    if ($property.Value -isnot [scriptblock]) {
        throw "R8 summary operation $Name is not a script block"
    }
    return [scriptblock]$property.Value
}

function Write-R8SummaryAtomically(
    [string]$Path,
    [object]$Value,
    [ValidateSet('Success', 'Failed')]
    [string]$ExpectedStatus,
    [object]$Operations = $null
) {
    $destination = [IO.Path]::GetFullPath($Path)
    if (Test-Path -LiteralPath $destination) {
        throw "R8 summary already exists: $destination"
    }
    $parent = Split-Path -Parent $destination
    [void][IO.Directory]::CreateDirectory($parent)
    $temporary = Join-Path $parent ('.summary-' + [guid]::NewGuid().ToString('N') + '.tmp')
    $serialize = Get-R8SummaryOperation $Operations 'Serialize' {
        param([object]$Document)
        $Document | ConvertTo-Json -Depth 24
    }
    $write = Get-R8SummaryOperation $Operations 'Write' {
        param([string]$Target, [string]$JSON)
        [IO.File]::WriteAllText($Target, $JSON, [Text.UTF8Encoding]::new($false))
    }
    $verify = Get-R8SummaryOperation $Operations 'Verify' {
        param([string]$Target, [string]$Status)
        if (-not (Test-Path -LiteralPath $Target -PathType Leaf)) {
            throw "R8 temporary summary is missing: $Target"
        }
        $raw = [IO.File]::ReadAllText($Target)
        if ([string]::IsNullOrWhiteSpace($raw)) {
            throw 'R8 temporary summary is empty'
        }
        $document = $raw | ConvertFrom-Json
        if ([string]$document.Status -cne $Status) {
            throw "R8 temporary summary status is $($document.Status), want $Status"
        }
    }
    $move = Get-R8SummaryOperation $Operations 'Move' {
        param([string]$Source, [string]$Destination)
        [IO.File]::Move($Source, $Destination)
    }

    $published = $false
    try {
        $json = [string](& $serialize $Value)
        if ([string]::IsNullOrWhiteSpace($json)) {
            throw 'R8 summary serialization returned no bytes'
        }
        [void](& $write $temporary $json)
        [void](& $verify $temporary $ExpectedStatus)
        [void](& $move $temporary $destination)
        $published = $true
    } finally {
        if (-not $published -and [IO.File]::Exists($temporary)) {
            try {
                [IO.File]::Delete($temporary)
            } catch {
                # A hidden temporary file is not a published verdict. Cleanup
                # is best-effort so it cannot mask the original publication error.
            }
        }
    }
    return $destination
}

function Complete-R8EvidenceSummary(
    [string]$Path,
    [AllowEmptyCollection()] [string[]]$InitialFailures,
    [scriptblock]$MetadataFactory,
    [scriptblock]$SourceCheckpoint,
    [scriptblock]$SummaryFactory,
    [object]$WriteOperations = $null
) {
    $failures = [Collections.Generic.List[string]]::new()
    foreach ($failure in $InitialFailures) {
        if (-not [string]::IsNullOrWhiteSpace($failure)) {
            $failures.Add($failure)
        }
    }

    $metadata = $null
    try {
        $metadata = & $MetadataFactory
    } catch {
        $failures.Add("metadata collection failed: $_")
    }

    # This checkpoint deliberately follows metadata collection. Everything
    # after it is pure in-memory composition or an ignored evidence-root write.
    $source = $null
    try {
        $source = & $SourceCheckpoint
        if ($null -eq $source -or -not [bool]$source.Stable) {
            $message = if ($null -ne $source -and
                -not [string]::IsNullOrWhiteSpace([string]$source.Error)) {
                [string]$source.Error
            } else {
                'workspace source changed before R8 finalization'
            }
            $failures.Add($message)
        }
    } catch {
        $failures.Add("final source checkpoint failed: $_")
    }

    $status = if ($failures.Count -eq 0) { 'Success' } else { 'Failed' }
    $errorMessage = if ($failures.Count -eq 0) { '' } else { $failures -join '; ' }
    try {
        $summary = & $SummaryFactory $status $errorMessage $metadata $source
    } catch {
        throw "R8 summary construction failed: $_"
    }
    if ($null -eq $summary -or [string]$summary.Status -cne $status) {
        throw "R8 summary factory did not preserve final status $status"
    }
    $summaryPath = Write-R8SummaryAtomically $Path $summary $status $WriteOperations
    return [pscustomobject][ordered]@{
        Path = $summaryPath
        Status = $status
        Error = $errorMessage
        Summary = $summary
    }
}
