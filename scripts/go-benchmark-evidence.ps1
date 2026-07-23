Set-StrictMode -Version Latest

function Test-GoBenchmarkFiniteNumber([double]$Value) {
    return -not [double]::IsNaN($Value) -and -not [double]::IsInfinity($Value)
}

function ConvertFrom-GoBenchmarkLog([Parameter(Mandatory)] [string]$Path) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Go benchmark log is missing: $Path"
    }

    $samplesByName = @{}
    $samples = [Collections.Generic.List[object]]::new()
    foreach ($rawLine in [IO.File]::ReadLines($Path)) {
        $line = $rawLine.Trim()
        if (-not $line.StartsWith('Benchmark', [StringComparison]::Ordinal)) {
            continue
        }
        if ($line -notmatch '^(Benchmark\S+)\s+([0-9]+)\s+(.+)$') {
            throw "Malformed Go benchmark sample in ${Path}: $rawLine"
        }

        $name = $Matches[1] -replace '-[0-9]+$', ''
        $iterations = [long]$Matches[2]
        if ($iterations -le 0) {
            throw "Go benchmark sample has no iterations in ${Path}: $rawLine"
        }
        $tokens = @($Matches[3] -split '\s+' | Where-Object { $_ -ne '' })
        if ($tokens.Count -eq 0 -or $tokens.Count % 2 -ne 0) {
            throw "Malformed Go benchmark metrics in ${Path}: $rawLine"
        }

        $metrics = [ordered]@{}
        for ($index = 0; $index -lt $tokens.Count; $index += 2) {
            $unit = [string]$tokens[$index + 1]
            if ([string]::IsNullOrWhiteSpace($unit) -or $metrics.Contains($unit)) {
                throw "Duplicate or empty Go benchmark metric in ${Path}: $rawLine"
            }
            $value = 0.0
            $parsed = [double]::TryParse(
                [string]$tokens[$index],
                [Globalization.NumberStyles]::Float,
                [Globalization.CultureInfo]::InvariantCulture,
                [ref]$value
            )
            if (-not $parsed -or -not (Test-GoBenchmarkFiniteNumber $value)) {
                throw "Non-finite or invalid Go benchmark metric in ${Path}: $rawLine"
            }
            $metrics[$unit] = $value
        }

        $ordinal = if ($samplesByName.ContainsKey($name)) {
            [int]$samplesByName[$name] + 1
        } else {
            1
        }
        $samplesByName[$name] = $ordinal
        $samples.Add([pscustomobject][ordered]@{
            Name = $name
            Sample = $ordinal
            Iterations = $iterations
            Metrics = [pscustomobject]$metrics
            RawLog = [IO.Path]::GetFileName($Path)
        })
    }
    if ($samples.Count -eq 0) {
        throw "Go benchmark log contains no samples: $Path"
    }
    return @($samples)
}

function Assert-GoBenchmarkEvidenceContract(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]]$Samples,
    [Parameter(Mandatory)] [Collections.IDictionary]$ExpectedMetrics,
    [Parameter(Mandatory)] [ValidateRange(1, 1000)] [int]$SampleCount
) {
    if ($ExpectedMetrics.Count -eq 0) {
        throw 'Go benchmark contract contains no benchmark identities'
    }
    if ($Samples.Count -eq 0) {
        throw 'Go benchmark evidence contains no samples'
    }

    $actualNames = @($Samples | ForEach-Object { [string]$_.Name } | Sort-Object -Unique)
    $expectedNames = @($ExpectedMetrics.Keys | ForEach-Object { [string]$_ } | Sort-Object -Unique)
    $identityDelta = @(Compare-Object -CaseSensitive -ReferenceObject $expectedNames -DifferenceObject $actualNames)
    if ($actualNames.Count -ne $expectedNames.Count -or $identityDelta.Count -ne 0) {
        throw (
            'Go benchmark identity set differs from the exact contract. ' +
            "Expected=[$($expectedNames -join ', ')], actual=[$($actualNames -join ', ')]"
        )
    }

    foreach ($name in $expectedNames) {
        $group = @($Samples | Where-Object { [string]$_.Name -ceq $name })
        if ($group.Count -ne $SampleCount) {
            throw "Go benchmark $name has $($group.Count) samples; expected exactly $SampleCount"
        }
        $ordinals = @($group | ForEach-Object { [int]$_.Sample } | Sort-Object)
        for ($index = 0; $index -lt $SampleCount; $index++) {
            if ($ordinals[$index] -ne $index + 1) {
                throw "Go benchmark $name has a missing or duplicate sample ordinal"
            }
        }

        $requiredMetrics = @($ExpectedMetrics[$name] | ForEach-Object { [string]$_ })
        if ($requiredMetrics.Count -eq 0) {
            throw "Go benchmark $name has no required metrics"
        }
        $uniqueRequiredMetrics = @($requiredMetrics | Sort-Object -Unique)
        if ($uniqueRequiredMetrics.Count -ne $requiredMetrics.Count) {
            throw "Go benchmark contract for $name repeats a metric identity"
        }
        foreach ($sample in $group) {
            if ([long]$sample.Iterations -le 0) {
                throw "Go benchmark $name sample $($sample.Sample) has no iterations"
            }
            $properties = @($sample.Metrics.PSObject.Properties)
            if ($properties.Count -eq 0) {
                throw "Go benchmark $name sample $($sample.Sample) has no metrics"
            }
            $actualMetrics = @($properties.Name | ForEach-Object { [string]$_ } | Sort-Object -Unique)
            $metricDelta = @(
                Compare-Object `
                    -CaseSensitive `
                    -ReferenceObject @($uniqueRequiredMetrics | Sort-Object) `
                    -DifferenceObject $actualMetrics
            )
            if ($actualMetrics.Count -ne $uniqueRequiredMetrics.Count -or $metricDelta.Count -ne 0) {
                throw (
                    "Go benchmark $name sample $($sample.Sample) metric set differs from the exact contract. " +
                    "Expected=[$($uniqueRequiredMetrics -join ', ')], actual=[$($actualMetrics -join ', ')]"
                )
            }
            foreach ($property in $properties) {
                $value = [double]$property.Value
                if (-not (Test-GoBenchmarkFiniteNumber $value)) {
                    throw "Go benchmark $name sample $($sample.Sample) metric $($property.Name) is not finite"
                }
            }
            foreach ($metric in $requiredMetrics) {
                $property = $sample.Metrics.PSObject.Properties[$metric]
                if ($null -eq $property) {
                    throw "Go benchmark $name sample $($sample.Sample) is missing required metric $metric"
                }
                if (-not (Test-GoBenchmarkFiniteNumber ([double]$property.Value))) {
                    throw "Go benchmark $name sample $($sample.Sample) metric $metric is not finite"
                }
            }
        }
    }
}

function Get-GoBenchmarkPercentile(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [double[]]$Values,
    [Parameter(Mandatory)] [ValidateRange(0.0, 1.0)] [double]$Percentile
) {
    if ($Values.Count -eq 0) {
        throw 'Cannot calculate a percentile over an empty sample set'
    }
    $ordered = @($Values | Sort-Object)
    $index = [math]::Max(0, [math]::Ceiling($Percentile * $ordered.Count) - 1)
    return [double]$ordered[$index]
}

function Get-GoBenchmarkAggregates(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]]$Samples
) {
    if ($Samples.Count -eq 0) {
        throw 'Cannot aggregate empty Go benchmark evidence'
    }
    $aggregates = [Collections.Generic.List[object]]::new()
    foreach ($group in @($Samples | Group-Object Name | Sort-Object Name)) {
        $metricNames = @(
            $group.Group |
                ForEach-Object { $_.Metrics.PSObject.Properties.Name } |
                Sort-Object -Unique
        )
        $metrics = [ordered]@{}
        foreach ($metricName in $metricNames) {
            $values = [double[]]@(
                $group.Group |
                    ForEach-Object { [double]$_.Metrics.PSObject.Properties[$metricName].Value }
            )
            $metrics[$metricName] = [ordered]@{
                Values = @($values)
                P50 = Get-GoBenchmarkPercentile $values 0.50
                P95 = Get-GoBenchmarkPercentile $values 0.95
            }
        }
        $aggregates.Add([pscustomobject][ordered]@{
            Name = $group.Name
            SampleCount = @($group.Group).Count
            Metrics = [pscustomobject]$metrics
        })
    }
    return @($aggregates)
}
