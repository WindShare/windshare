Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'd5-evidence.ps1')

function Assert-Throws([scriptblock]$Action, [string]$Pattern) {
    try {
        & $Action
    } catch {
        if ([string]$_ -notmatch $Pattern) {
            throw "Unexpected error: $_"
        }
        return
    }
    throw "Expected an error matching: $Pattern"
}

function Set-D5Writable([string]$Path) {
    foreach ($file in Get-ChildItem -LiteralPath $Path -File -Recurse -ErrorAction SilentlyContinue) {
        $file.IsReadOnly = $false
    }
}

function Assert-NoPublishedEvidence([string]$Path) {
    $published = @(
        Get-ChildItem -LiteralPath $Path -Directory -ErrorAction SilentlyContinue |
            Where-Object { -not $_.Name.StartsWith('.staging-', [StringComparison]::Ordinal) }
    )
    if ($published.Count -ne 0) {
        throw "Evidence failure stranded a discoverable final directory: $($published.FullName -join ', ')"
    }
}

function New-D5EvidenceFixtureRun([string]$Root, [string]$Label) {
    $run = New-D5EvidenceRun $repositoryRoot $Root 'Regression' $Label $started
    [IO.File]::WriteAllText(
        (Join-Path $run.StagePath 'raw.txt'),
        "fixture $Label",
        [Text.UTF8Encoding]::new($false)
    )
    return $run
}

function New-D5TestPublicationOperations(
    [string]$InjectedName = '',
    [scriptblock]$InjectedAction = $null
) {
    $operations = [ordered]@{
        Environment = {
            param([object]$Run)
            [ordered]@{ Fixture = 'deterministic' }
        }
        SourceCheckpoint = {
            param([object]$Run)
            $Run.SourceAtStart
        }
    }
    if (-not [string]::IsNullOrWhiteSpace($InjectedName)) {
        $operations[$InjectedName] = $InjectedAction
    }
    return [pscustomobject]$operations
}

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-d5-evidence-' + [guid]::NewGuid().ToString('N'))
$started = [datetimeoffset]::Parse('2026-07-11T10:00:00+00:00')
$completed = [datetimeoffset]::Parse('2026-07-11T10:01:00+00:00')
try {
    $first = New-D5EvidenceRun $repositoryRoot $testRoot 'Regression' 'd5 evidence regression' $started
    [IO.File]::WriteAllText(
        (Join-Path $first.StagePath 'raw.txt'),
        'immutable raw evidence',
        [Text.UTF8Encoding]::new($false)
    )
    $stableOperations = New-D5TestPublicationOperations
    $firstResult = Complete-D5EvidenceRun $first 'Success' '' $completed $stableOperations
    $manifest = Get-Content -LiteralPath (Join-Path $firstResult.Path 'manifest.json') -Raw | ConvertFrom-Json
    $payload = Get-Content -LiteralPath (Join-Path $firstResult.Path 'payload.json') -Raw | ConvertFrom-Json
    if ($firstResult.Status -ne 'Success' -or
        $payload.Status -ne 'Success' -or
        [datetimeoffset]$payload.CompletedAt -ne $completed -or
        -not $payload.Source.UnchangedForWholeRun -or
        [string]::IsNullOrWhiteSpace([string]$manifest.EvidenceID) -or
        [string]::IsNullOrWhiteSpace([string]$payload.Source.Start.SourceDigest) -or
        $payload.Source.Start.SourceDigest -ne $payload.Source.End.SourceDigest) {
        throw 'Successful evidence manifest is incomplete or source-unstable'
    }
    if ($payload.Source.Start.WorktreeClean -and
        $payload.Source.Start.CommitStatus -ne 'committed-clean') {
        throw 'Clean source was not identified as committed'
    }
    if (-not $payload.Source.Start.WorktreeClean -and
        $payload.Source.Start.CommitStatus -ne 'commit-pending-dirty-workspace') {
        throw 'Dirty source did not record honest commit-pending status'
    }
    if (-not (Test-D5EvidenceDirectory $firstResult.Path)) {
        throw 'Published evidence verifier did not authenticate valid bytes'
    }
    if (-not (Get-Item -LiteralPath (Join-Path $firstResult.Path 'raw.txt')).IsReadOnly) {
        throw 'Published raw evidence was not sealed read-only'
    }

    $second = New-D5EvidenceRun $repositoryRoot $testRoot 'Regression' 'd5 evidence regression' $started
    $second.SourceAtStart = $first.SourceAtStart
    [IO.File]::WriteAllText(
        (Join-Path $second.StagePath 'raw.txt'),
        'immutable raw evidence',
        [Text.UTF8Encoding]::new($false)
    )
    Assert-Throws {
        Complete-D5EvidenceRun $second 'Success' '' $completed $stableOperations
    } 'refusing to overwrite'
    if ((Get-Content -LiteralPath (Join-Path $firstResult.Path 'raw.txt') -Raw) -ne 'immutable raw evidence') {
        throw 'Existing evidence changed after a duplicate publication attempt'
    }

    (Get-Item -LiteralPath (Join-Path $firstResult.Path 'raw.txt')).IsReadOnly = $false
    [IO.File]::WriteAllText(
        (Join-Path $firstResult.Path 'raw.txt'),
        'tampered evidence',
        [Text.UTF8Encoding]::new($false)
    )
    Assert-Throws {
        Test-D5EvidenceDirectory $firstResult.Path
    } 'differs from its manifest'

    $failed = New-D5EvidenceRun $repositoryRoot $testRoot 'Regression' 'failing command' $started
    [IO.File]::WriteAllText(
        (Join-Path $failed.StagePath 'stderr.txt'),
        'expected failure',
        [Text.UTF8Encoding]::new($false)
    )
    $failedResult = Complete-D5EvidenceRun $failed 'Failed' 'exit code 7' $completed $stableOperations
    $failedPayload = Get-Content -LiteralPath (Join-Path $failedResult.Path 'payload.json') -Raw | ConvertFrom-Json
    if ($failedPayload.Status -ne 'Failed' -or
        $failedPayload.Error -ne 'exit code 7') {
        throw 'Failed command was recorded as successful'
    }

    foreach ($injection in @(
        [pscustomobject]@{
            Name = 'verify'
            OperationName = 'Verify'
            Action = { throw 'injected verify failure' }
            Pattern = 'injected verify failure'
        },
        [pscustomobject]@{
            Name = 'seal'
            OperationName = 'Seal'
            Action = { throw 'injected seal failure' }
            Pattern = 'injected seal failure'
        },
        [pscustomobject]@{
            Name = 'move'
            OperationName = 'Move'
            Action = { throw 'injected move failure' }
            Pattern = 'injected move failure'
        }
    )) {
        $injectionRoot = Join-Path $testRoot ("injected-$($injection.Name)")
        $injectedRun = New-D5EvidenceFixtureRun $injectionRoot $injection.Name
        $injectedOperations = New-D5TestPublicationOperations `
            $injection.OperationName `
            $injection.Action
        Assert-Throws {
            Complete-D5EvidenceRun `
                $injectedRun `
                'Success' `
                '' `
                $completed `
                $injectedOperations
        } $injection.Pattern
        Assert-NoPublishedEvidence $injectionRoot
    }

    $orderedRoot = Join-Path $testRoot 'ordered-publication'
    $orderedRun = New-D5EvidenceFixtureRun $orderedRoot 'ordered-publication'
    $publicationOrder = [Collections.Generic.List[string]]::new()
    $verifyCount = [ref]0
    $orderedOperations = New-D5TestPublicationOperations
    Add-Member -InputObject $orderedOperations -NotePropertyName Verify -NotePropertyValue ({
        param([string]$Path, [string]$EvidenceID)
        $verifyCount.Value++
        $publicationOrder.Add("verify-$($verifyCount.Value)")
        [void](Test-D5EvidenceDirectory $Path $EvidenceID)
    }.GetNewClosure())
    Add-Member -InputObject $orderedOperations -NotePropertyName Seal -NotePropertyValue ({
        param([string]$Path)
        $publicationOrder.Add('seal')
        Seal-D5EvidenceStaging $Path
    }.GetNewClosure())
    Add-Member -InputObject $orderedOperations -NotePropertyName Move -NotePropertyValue ({
        param([string]$Source, [string]$Destination)
        $publicationOrder.Add('move')
        [IO.Directory]::Move($Source, $Destination)
    }.GetNewClosure())
    $orderedResult = Complete-D5EvidenceRun `
        $orderedRun `
        'Success' `
        '' `
        $completed `
        $orderedOperations
    if (($publicationOrder -join ',') -ne 'verify-1,seal,verify-2,move' -or
        -not (Test-Path -LiteralPath $orderedResult.Path -PathType Container)) {
        throw 'Evidence publication did not preserve verify, seal, verify, atomic-move order'
    }

    # A verifier that fails only after sealing distinguishes the second
    # integrity checkpoint from a duplicate pre-seal call.
    $postSealRoot = Join-Path $testRoot 'injected-post-seal-verify'
    $postSealRun = New-D5EvidenceFixtureRun $postSealRoot 'post-seal-verify'
    $postSealOrder = [Collections.Generic.List[string]]::new()
    $postSealVerifyCount = [ref]0
    $postSealMoveCalled = [ref]$false
    $postSealOperations = New-D5TestPublicationOperations
    Add-Member -InputObject $postSealOperations -NotePropertyName Verify -NotePropertyValue ({
        param([string]$Path, [string]$EvidenceID)
        $postSealVerifyCount.Value++
        $postSealOrder.Add("verify-$($postSealVerifyCount.Value)")
        if ($postSealVerifyCount.Value -eq 2) {
            throw 'injected post-seal verify failure'
        }
        [void](Test-D5EvidenceDirectory $Path $EvidenceID)
    }.GetNewClosure())
    Add-Member -InputObject $postSealOperations -NotePropertyName Seal -NotePropertyValue ({
        param([string]$Path)
        $postSealOrder.Add('seal')
        Seal-D5EvidenceStaging $Path
    }.GetNewClosure())
    Add-Member -InputObject $postSealOperations -NotePropertyName Move -NotePropertyValue ({
        param([string]$Source, [string]$Destination)
        $postSealMoveCalled.Value = $true
        [IO.Directory]::Move($Source, $Destination)
    }.GetNewClosure())
    Assert-Throws {
        Complete-D5EvidenceRun `
            $postSealRun `
            'Success' `
            '' `
            $completed `
            $postSealOperations
    } 'injected post-seal verify failure'
    if (($postSealOrder -join ',') -ne 'verify-1,seal,verify-2' -or
        $postSealMoveCalled.Value) {
        throw 'Post-seal verification failure did not stop before atomic publication'
    }
    Assert-NoPublishedEvidence $postSealRoot

    $cleanupRoot = Join-Path $testRoot 'injected-cleanup'
    $cleanupRun = New-D5EvidenceFixtureRun $cleanupRoot 'cleanup'
    $continued = [ref]$false
    $cleanupTransaction = Complete-D5EvidenceTransaction `
        $cleanupRun `
        'Success' `
        '' `
        @(
            [pscustomobject]@{
                Name = 'runner guard cleanup'
                Action = { throw 'injected cleanup failure' }
            },
            [pscustomobject]@{
                Name = 'remaining cleanup'
                Action = { $continued.Value = $true }.GetNewClosure()
            }
        ) `
        $completed `
        $stableOperations
    $cleanupPayload = Get-Content -LiteralPath (
        Join-Path $cleanupTransaction.Result.Path 'payload.json'
    ) -Raw | ConvertFrom-Json
    if ($cleanupTransaction.Status -ne 'Failed' -or
        $cleanupPayload.Status -ne 'Failed' -or
        [string]$cleanupPayload.Error -notmatch 'injected cleanup failure' -or
        -not $continued.Value) {
        throw 'Cleanup failure did not transactionally demote publication and continue cleanup'
    }

    $completionOrderRoot = Join-Path $testRoot 'completion-timestamp-order'
    $completionOrderRun = New-D5EvidenceFixtureRun $completionOrderRoot 'completion-timestamp-order'
    $lastCleanupAt = [ref][datetimeoffset]::MinValue
    $completionOrderTransaction = Complete-D5EvidenceTransaction `
        -Run $completionOrderRun `
        -RequestedStatus 'Success' `
        -FinalizationSteps @(
            [pscustomobject]@{
                Name = 'last cleanup'
                Action = {
                    $lastCleanupAt.Value = [datetimeoffset]::UtcNow
                }.GetNewClosure()
            }
        ) `
        -PublicationOperations $stableOperations
    $completionOrderPayload = Get-Content -LiteralPath (
        Join-Path $completionOrderTransaction.Result.Path 'payload.json'
    ) -Raw | ConvertFrom-Json
    $recordedCompletion = [datetimeoffset]$completionOrderPayload.CompletedAt
    if ($lastCleanupAt.Value -eq [datetimeoffset]::MinValue -or
        $recordedCompletion -lt $lastCleanupAt.Value) {
        throw (
            'Default evidence completion timestamp was captured before final cleanup: ' +
            "cleanup=$($lastCleanupAt.Value.ToString('o')), completion=$($recordedCompletion.ToString('o'))"
        )
    }

    $drifted = New-D5EvidenceRun $repositoryRoot $testRoot 'Regression' 'source drift' $started
    $drifted.SourceStable = $false
    $drifted.SourceFailures.Add('injected source drift')
    $driftResult = Complete-D5EvidenceRun $drifted 'Success' '' $completed $stableOperations
    $driftPayload = Get-Content -LiteralPath (Join-Path $driftResult.Path 'payload.json') -Raw | ConvertFrom-Json
    if ($driftResult.Status -ne 'Failed' -or
        $driftPayload.Status -ne 'Failed' -or
        $driftPayload.Source.UnchangedForWholeRun) {
        throw 'Source drift was not published exclusively as failed evidence'
    }
} finally {
    Set-D5Writable $testRoot
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'D5 immutable evidence tests PASS'
