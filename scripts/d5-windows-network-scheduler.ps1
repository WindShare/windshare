Set-StrictMode -Version Latest

$script:D5NetworkParallelExecutionClass = 'parallel'
$script:D5NetworkExclusiveExecutionClass = 'exclusive'
$script:D5NetworkExecuteStepKind = 'execute'
$script:D5NetworkQuiescenceStepKind = 'quiescence'

function Get-D5NetworkExecutionBatches(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]]$Definitions
) {
    $parallel = [Collections.Generic.List[object]]::new()
    $exclusive = [Collections.Generic.List[object]]::new()
    foreach ($definition in $Definitions) {
        $name = [string]$definition.Name
        $executionClass = [string]$definition.ExecutionClass
        if ([string]::IsNullOrWhiteSpace($name)) {
            throw 'Network execution definition has no package name'
        }
        if ($executionClass -ceq $script:D5NetworkParallelExecutionClass) {
            $parallel.Add($definition)
        } elseif ($executionClass -ceq $script:D5NetworkExclusiveExecutionClass) {
            $exclusive.Add($definition)
        } else {
            throw "Network execution definition $name has unsupported class: $executionClass"
        }
    }

    $batches = [Collections.Generic.List[object]]::new()
    # Exclusive plans run first and alone. This gives process-heavy suites a
    # clean machine namespace and prevents later parallel work from influencing
    # their fixed process deadlines.
    foreach ($definition in @($exclusive | Sort-Object { [string]$_.Name })) {
        $batches.Add([pscustomobject][ordered]@{
            ExecutionClass = $script:D5NetworkExclusiveExecutionClass
            Definitions = @($definition)
        })
    }
    if ($parallel.Count -gt 0) {
        $batches.Add([pscustomobject][ordered]@{
            ExecutionClass = $script:D5NetworkParallelExecutionClass
            Definitions = @($parallel | Sort-Object { [string]$_.Name })
        })
    }
    return @($batches)
}

function Get-D5NetworkExecutionSteps(
    [Parameter(Mandatory)] [AllowEmptyCollection()] [object[]]$Batches
) {
    $steps = [Collections.Generic.List[object]]::new()
    foreach ($batch in $Batches) {
        $definitions = @($batch.Definitions)
        if ($definitions.Count -eq 0) {
            throw 'Network execution batch is empty'
        }
        $steps.Add([pscustomobject][ordered]@{
            Kind = $script:D5NetworkExecuteStepKind
            Batch = $batch
        })
        # The quiescence step is part of the schedule, rather than cleanup
        # hidden in the runner, so exclusivity cannot silently degrade to
        # launch ordering while descendants remain alive.
        $steps.Add([pscustomobject][ordered]@{
            Kind = $script:D5NetworkQuiescenceStepKind
            Batch = $batch
        })
    }
    return @($steps)
}
