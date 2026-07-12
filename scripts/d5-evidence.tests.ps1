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

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-d5-evidence-' + [guid]::NewGuid().ToString('N'))
$sourceFixture = Join-Path $repositoryRoot ('d5-evidence-source-fixture-' + [guid]::NewGuid().ToString('N') + '.txt')
$started = [datetimeoffset]::Parse('2026-07-11T10:00:00+00:00')
$completed = [datetimeoffset]::Parse('2026-07-11T10:01:00+00:00')
try {
    $first = New-D5EvidenceRun $repositoryRoot $testRoot 'Regression' 'd5 evidence regression' $started
    [IO.File]::WriteAllText(
        (Join-Path $first.StagePath 'raw.txt'),
        'immutable raw evidence',
        [Text.UTF8Encoding]::new($false)
    )
    [void](Add-D5SourceCheckpoint $first 'before-fixture-command')
    $firstResult = Complete-D5EvidenceRun $first 'Success' '' $completed
    $manifest = Get-Content -LiteralPath (Join-Path $firstResult.Path 'manifest.json') -Raw | ConvertFrom-Json
    $payload = Get-Content -LiteralPath (Join-Path $firstResult.Path 'payload.json') -Raw | ConvertFrom-Json
    if ($firstResult.Status -ne 'Success' -or
        $payload.Status -ne 'Success' -or
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
    [IO.File]::WriteAllText(
        (Join-Path $second.StagePath 'raw.txt'),
        'immutable raw evidence',
        [Text.UTF8Encoding]::new($false)
    )
    [void](Add-D5SourceCheckpoint $second 'before-fixture-command')
    Assert-Throws {
        Complete-D5EvidenceRun $second 'Success' '' $completed
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
    $failedResult = Complete-D5EvidenceRun $failed 'Failed' 'exit code 7' $completed
    $failedPayload = Get-Content -LiteralPath (Join-Path $failedResult.Path 'payload.json') -Raw | ConvertFrom-Json
    if ($failedPayload.Status -ne 'Failed' -or
        $failedPayload.Error -ne 'exit code 7') {
        throw 'Failed command was recorded as successful'
    }

    $drifted = New-D5EvidenceRun $repositoryRoot $testRoot 'Regression' 'source drift' $started
    [IO.File]::WriteAllText($sourceFixture, 'concurrent edit', [Text.UTF8Encoding]::new($false))
    Assert-Throws {
        Add-D5SourceCheckpoint $drifted 'after-concurrent-edit'
    } 'Workspace source changed'
    $driftResult = Complete-D5EvidenceRun $drifted 'Success' '' $completed
    $driftPayload = Get-Content -LiteralPath (Join-Path $driftResult.Path 'payload.json') -Raw | ConvertFrom-Json
    if ($driftResult.Status -ne 'Failed' -or
        $driftPayload.Status -ne 'Failed' -or
        $driftPayload.Source.UnchangedForWholeRun) {
        throw 'Source drift was not published exclusively as failed evidence'
    }
} finally {
    if (Test-Path -LiteralPath $sourceFixture) {
        Remove-Item -LiteralPath $sourceFixture -Force
    }
    Set-D5Writable $testRoot
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'D5 immutable evidence tests PASS'
