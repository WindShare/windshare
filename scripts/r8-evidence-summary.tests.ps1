Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'r8-evidence-summary.ps1')

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

function New-R8SummaryFactory {
    return {
        param([string]$Status, [string]$ErrorMessage, [object]$Metadata, [object]$Source)
        [ordered]@{
            SchemaVersion = 1
            Status = $Status
            Error = $ErrorMessage
            Metadata = $Metadata
            Source = $Source
        }
    }
}

function Assert-NoPublishedR8Summary([string]$Path) {
    if (Test-Path -LiteralPath $Path) {
        throw "R8 failure left a published summary: $Path"
    }
    $temporary = @(
        Get-ChildItem -LiteralPath (Split-Path -Parent $Path) -Filter '.summary-*.tmp' -File -ErrorAction SilentlyContinue
    )
    if ($temporary.Count -ne 0) {
        throw "R8 failure stranded temporary summary bytes: $($temporary.FullName -join ', ')"
    }
}

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-r8-summary-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($testRoot)
try {
    $order = [Collections.Generic.List[string]]::new()
    $metadata = { $order.Add('metadata'); return [pscustomobject]@{ Host = 'fixture' } }.GetNewClosure()
    $source = {
        $order.Add('source')
        return [pscustomobject]@{ Stable = $true; Value = 'same'; Error = '' }
    }.GetNewClosure()
    $successPath = Join-Path $testRoot 'success.json'
    $success = Complete-R8EvidenceSummary `
        $successPath `
        @() `
        $metadata `
        $source `
        (New-R8SummaryFactory)
    if ($success.Status -ne 'Success' -or
        -not (Test-Path -LiteralPath $successPath -PathType Leaf) -or
        ($order -join ',') -ne 'metadata,source') {
        throw 'R8 success summary did not preserve finalization order and atomic visibility'
    }

    $metadataFailurePath = Join-Path $testRoot 'metadata-failure.json'
    $metadataFailure = Complete-R8EvidenceSummary `
        $metadataFailurePath `
        @() `
        { throw 'injected metadata failure' } `
        { [pscustomobject]@{ Stable = $true; Value = 'same'; Error = '' } } `
        (New-R8SummaryFactory)
    $metadataFailureDocument = Get-Content -LiteralPath $metadataFailurePath -Raw | ConvertFrom-Json
    if ($metadataFailure.Status -ne 'Failed' -or
        $metadataFailureDocument.Status -ne 'Failed' -or
        [string]$metadataFailureDocument.Error -notmatch 'injected metadata failure') {
        throw 'Metadata failure was published as Success'
    }

    $driftPath = Join-Path $testRoot 'source-drift.json'
    $drift = Complete-R8EvidenceSummary `
        $driftPath `
        @() `
        { [pscustomobject]@{ Host = 'fixture' } } `
        { [pscustomobject]@{ Stable = $false; Value = 'changed'; Error = 'injected source drift' } } `
        (New-R8SummaryFactory)
    $driftDocument = Get-Content -LiteralPath $driftPath -Raw | ConvertFrom-Json
    if ($drift.Status -ne 'Failed' -or
        $driftDocument.Status -ne 'Failed' -or
        [string]$driftDocument.Error -notmatch 'injected source drift') {
        throw 'Source drift was published as Success'
    }

    $combinedPath = Join-Path $testRoot 'combined-failure-ledger.json'
    $combined = Complete-R8EvidenceSummary `
        $combinedPath `
        @('injected command failure') `
        { throw 'injected combined metadata failure' } `
        { [pscustomobject]@{ Stable = $false; Value = 'changed'; Error = 'injected combined source drift' } } `
        (New-R8SummaryFactory)
    $combinedDocument = Get-Content -LiteralPath $combinedPath -Raw | ConvertFrom-Json
    $combinedError = [string]$combinedDocument.Error
    if ($combined.Status -ne 'Failed' -or
        $combinedDocument.Status -ne 'Failed' -or
        $combinedError -notmatch 'injected command failure' -or
        $combinedError -notmatch 'injected combined metadata failure' -or
        $combinedError -notmatch 'injected combined source drift') {
        throw 'R8 whole-run failure ledger did not retain command, metadata, and source failures'
    }

    $orderedPath = Join-Path $testRoot 'ordered-publication.json'
    $publicationOrder = [Collections.Generic.List[string]]::new()
    $orderedOperations = [pscustomobject]@{
        Serialize = {
            param([object]$Document)
            $publicationOrder.Add('serialize')
            $Document | ConvertTo-Json -Depth 24
        }.GetNewClosure()
        Write = {
            param([string]$Target, [string]$JSON)
            $publicationOrder.Add('write')
            [IO.File]::WriteAllText($Target, $JSON, [Text.UTF8Encoding]::new($false))
        }.GetNewClosure()
        Verify = {
            param([string]$Target, [string]$Status)
            $publicationOrder.Add('verify')
            $document = Get-Content -LiteralPath $Target -Raw | ConvertFrom-Json
            if ([string]$document.Status -cne $Status) {
                throw 'ordered fixture status changed before publication'
            }
        }.GetNewClosure()
        Move = {
            param([string]$Source, [string]$Destination)
            $publicationOrder.Add('move')
            [IO.File]::Move($Source, $Destination)
        }.GetNewClosure()
    }
    [void](Complete-R8EvidenceSummary `
        $orderedPath `
        @() `
        { [pscustomobject]@{ Host = 'fixture' } } `
        { [pscustomobject]@{ Stable = $true; Value = 'same'; Error = '' } } `
        (New-R8SummaryFactory) `
        $orderedOperations)
    if (($publicationOrder -join ',') -ne 'serialize,write,verify,move' -or
        -not (Test-Path -LiteralPath $orderedPath -PathType Leaf)) {
        throw 'R8 summary publication did not preserve serialize, write, verify, atomic-move order'
    }

    $serializationPath = Join-Path $testRoot 'serialization-failure.json'
    Assert-Throws {
        Complete-R8EvidenceSummary `
            $serializationPath `
            @() `
            { [pscustomobject]@{ Host = 'fixture' } } `
            { [pscustomobject]@{ Stable = $true; Value = 'same'; Error = '' } } `
            (New-R8SummaryFactory) `
            ([pscustomobject]@{ Serialize = { throw 'injected serialization failure' } })
    } 'injected serialization failure'
    Assert-NoPublishedR8Summary $serializationPath

    $writePath = Join-Path $testRoot 'write-failure.json'
    Assert-Throws {
        Complete-R8EvidenceSummary `
            $writePath `
            @() `
            { [pscustomobject]@{ Host = 'fixture' } } `
            { [pscustomobject]@{ Stable = $true; Value = 'same'; Error = '' } } `
            (New-R8SummaryFactory) `
            ([pscustomobject]@{ Write = { throw 'injected summary write failure' } })
    } 'injected summary write failure'
    Assert-NoPublishedR8Summary $writePath

    $verifyPath = Join-Path $testRoot 'verify-failure.json'
    Assert-Throws {
        Complete-R8EvidenceSummary `
            $verifyPath `
            @() `
            { [pscustomobject]@{ Host = 'fixture' } } `
            { [pscustomobject]@{ Stable = $true; Value = 'same'; Error = '' } } `
            (New-R8SummaryFactory) `
            ([pscustomobject]@{ Verify = { throw 'injected summary verify failure' } })
    } 'injected summary verify failure'
    Assert-NoPublishedR8Summary $verifyPath

    $movePath = Join-Path $testRoot 'move-failure.json'
    Assert-Throws {
        Complete-R8EvidenceSummary `
            $movePath `
            @() `
            { [pscustomobject]@{ Host = 'fixture' } } `
            { [pscustomobject]@{ Stable = $true; Value = 'same'; Error = '' } } `
            (New-R8SummaryFactory) `
            ([pscustomobject]@{ Move = { throw 'injected summary move failure' } })
    } 'injected summary move failure'
    Assert-NoPublishedR8Summary $movePath
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'R8 atomic evidence summary tests PASS'
