Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'd5-windows-network-scheduler.ps1')

function Assert-Equal([object]$Actual, [object]$Expected, [string]$Context) {
    if ([string]$Actual -cne [string]$Expected) {
        throw "$Context = $Actual, want $Expected"
    }
}

$definitions = @(
    [pscustomobject]@{ Name = 'windowsjob'; ExecutionClass = 'parallel' },
    [pscustomobject]@{ Name = 'e2e'; ExecutionClass = 'exclusive' },
    [pscustomobject]@{ Name = 'cli'; ExecutionClass = 'parallel' }
)
$batches = @(Get-D5NetworkExecutionBatches $definitions)
Assert-Equal $batches.Count 2 'mixed batch count'
Assert-Equal $batches[0].ExecutionClass 'exclusive' 'first batch class'
Assert-Equal @($batches[0].Definitions).Count 1 'exclusive batch size'
Assert-Equal $batches[0].Definitions[0].Name 'e2e' 'exclusive package'
Assert-Equal $batches[1].ExecutionClass 'parallel' 'second batch class'
Assert-Equal (@($batches[1].Definitions.Name) -join ',') 'cli,windowsjob' 'parallel package order'

$steps = @(Get-D5NetworkExecutionSteps $batches)
Assert-Equal $steps.Count 4 'mixed execution step count'
Assert-Equal (@($steps.Kind) -join ',') 'execute,quiescence,execute,quiescence' 'execution step order'
Assert-Equal $steps[0].Batch.ExecutionClass 'exclusive' 'exclusive execution step'
Assert-Equal $steps[1].Batch.ExecutionClass 'exclusive' 'exclusive quiescence step'
Assert-Equal $steps[2].Batch.ExecutionClass 'parallel' 'parallel execution step'
Assert-Equal $steps[3].Batch.ExecutionClass 'parallel' 'parallel quiescence step'

$twoExclusive = @(
    [pscustomobject]@{ Name = 'z-heavy'; ExecutionClass = 'exclusive' },
    [pscustomobject]@{ Name = 'a-heavy'; ExecutionClass = 'exclusive' }
)
$exclusiveBatches = @(Get-D5NetworkExecutionBatches $twoExclusive)
Assert-Equal $exclusiveBatches.Count 2 'exclusive batch count'
Assert-Equal $exclusiveBatches[0].Definitions[0].Name 'a-heavy' 'first exclusive package'
Assert-Equal $exclusiveBatches[1].Definitions[0].Name 'z-heavy' 'second exclusive package'

$invalidRejected = $false
try {
    [void](Get-D5NetworkExecutionBatches @(
        [pscustomobject]@{ Name = 'invalid'; ExecutionClass = 'process-heavy' }
    ))
} catch {
    $invalidRejected = [string]$_ -match 'unsupported class'
}
if (-not $invalidRejected) {
    throw 'Scheduler accepted an unsupported execution class'
}
Assert-Equal @(Get-D5NetworkExecutionBatches @()).Count 0 'empty batch count'
Assert-Equal @(Get-D5NetworkExecutionSteps @()).Count 0 'empty step count'

$emptyBatchRejected = $false
try {
    [void](Get-D5NetworkExecutionSteps @(
        [pscustomobject]@{ ExecutionClass = 'parallel'; Definitions = @() }
    ))
} catch {
    $emptyBatchRejected = [string]$_ -match 'batch is empty'
}
if (-not $emptyBatchRejected) {
    throw 'Scheduler accepted an empty execution batch'
}

Write-Output 'D5 network scheduler tests passed'
