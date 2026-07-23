Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'd5-evidence.ps1')
. (Join-Path $PSScriptRoot 'r8-web-performance-authority.ps1')
. (Join-Path $PSScriptRoot 'r8-evidence-summary.ps1')

function Assert-Throws([scriptblock]$Action, [string]$Pattern) {
    try {
        & $Action
    } catch {
        if ([string]$_ -notmatch $Pattern) { throw "Unexpected error: $_" }
        return
    }
    throw "Expected an error matching: $Pattern"
}

function New-Identity([string]$Digest) {
    return [pscustomobject]@{
        IdentityKind = 'workspace-manifest'
        Commit = 'test-commit'
        CommitStatus = 'commit-pending-dirty-workspace'
        WorktreeClean = $false
        SourceDigest = $Digest
    }
}

function New-VerifiedEvidenceFixture([string]$Root, [string]$Suffix) {
    $stage = Join-Path $Root ('stage-' + $Suffix)
    $log = Join-Path (Join-Path (Join-Path $stage 'browser') 'browser') 'playwright.txt'
    [void][IO.Directory]::CreateDirectory((Split-Path -Parent $log))
    [IO.File]::WriteAllText($log, "verified trend log $Suffix", [Text.UTF8Encoding]::new($false))
    $artifact = [ordered]@{
        Path = 'browser/browser/playwright.txt'
        Bytes = [long](Get-Item -LiteralPath $log).Length
        SHA256 = (Get-FileHash -LiteralPath $log -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    $payloadDocument = [ordered]@{
        SchemaVersion = 2
        Status = 'Success'
        Source = [ordered]@{
            UnchangedForWholeRun = $true
            Start = New-Identity 'same-source'
            End = New-Identity 'same-source'
        }
        Artifacts = @($artifact)
    }
    $payloadJSON = $payloadDocument | ConvertTo-Json -Depth 12 -Compress
    $evidenceID = Get-D5TextSHA256 $payloadJSON
    [IO.File]::WriteAllText(
        (Join-Path $stage 'payload.json'),
        $payloadJSON,
        [Text.UTF8Encoding]::new($false)
    )
    [IO.File]::WriteAllText(
        (Join-Path $stage 'manifest.json'),
        ([ordered]@{
            EvidenceID = $evidenceID
            PayloadSHA256 = $evidenceID
            PayloadFile = 'payload.json'
        } | ConvertTo-Json),
        [Text.UTF8Encoding]::new($false)
    )
    $destination = Join-Path $Root $evidenceID
    [IO.Directory]::Move($stage, $destination)
    return $destination
}

$start = New-Identity 'same-source'
$end = New-Identity 'same-source'
$payload = [pscustomobject]@{
    Status = 'Success'
    Source = [pscustomobject]@{
        UnchangedForWholeRun = $true
        Start = New-Identity 'same-source'
        End = New-Identity 'same-source'
    }
}

Assert-R8WindowsPerformanceAuthority $true
Assert-R8SourceAuthority $start $end $payload
Assert-Throws { Assert-R8WindowsPerformanceAuthority $false } 'audited Windows D5'
Assert-Throws {
    Assert-R8SourceAuthority $start (New-Identity 'runner-drift') $payload
} 'runner start and end'
$d5Drift = [pscustomobject]@{
    Status = 'Success'
    Source = [pscustomobject]@{
        UnchangedForWholeRun = $true
        Start = New-Identity 'same-source'
        End = New-Identity 'd5-drift'
    }
}
Assert-Throws { Assert-R8SourceAuthority $start $end $d5Drift } 'audited D5 end'
$failed = [pscustomobject]@{
    Status = 'Failed'
    Source = [pscustomobject]@{
        UnchangedForWholeRun = $false
        Start = New-Identity 'same-source'
        End = New-Identity 'same-source'
    }
}
Assert-Throws { Assert-R8SourceAuthority $start $end $failed } 'successful, source-stable'

$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-r8-web-authority-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($testRoot)
try {
    $valid = New-VerifiedEvidenceFixture $testRoot 'valid'
    $verified = Read-R8VerifiedD5Evidence $valid
    if ([string]$verified.Payload.Status -ne 'Success') {
        throw 'Verified R8 evidence fixture did not round-trip'
    }

    $payloadTamper = New-VerifiedEvidenceFixture $testRoot 'payload-tamper'
    Add-Content -LiteralPath (Join-Path $payloadTamper 'payload.json') -Value ' ' -NoNewline
    Assert-Throws { Read-R8VerifiedD5Evidence $payloadTamper } 'content address|invalid'

    $logTamper = New-VerifiedEvidenceFixture $testRoot 'log-tamper'
    $tamperedLog = Join-Path (Join-Path (Join-Path $logTamper 'browser') 'browser') 'playwright.txt'
    Add-Content -LiteralPath $tamperedLog `
        -Value 'tampered' -NoNewline
    Assert-Throws { Read-R8VerifiedD5Evidence $logTamper } 'differs from its manifest'

    $partialID = 'a' * 64
    $partial = Join-Path $testRoot $partialID
    [void][IO.Directory]::CreateDirectory($partial)
    [IO.File]::WriteAllText(
        (Join-Path $partial 'manifest.json'),
        '{"EvidenceID":"missing-payload"}',
        [Text.UTF8Encoding]::new($false)
    )
    Assert-Throws { Read-R8VerifiedD5Evidence $partial } 'partially finalized'

    $finalizationOrder = [Collections.Generic.List[string]]::new()
    $driftSummaryPath = Join-Path $testRoot 'validator-after-drift.json'
    $driftEnd = New-Identity 'validator-after-drift'
    $driftPayload = $payload
    $driftSummary = Complete-R8EvidenceSummary `
        $driftSummaryPath `
        @() `
        { $finalizationOrder.Add('validator'); return [pscustomobject]@{ Records = @() } } `
        {
            $finalizationOrder.Add('source')
            return Get-R8SourceAuthorityCheckpoint $start $driftEnd $driftPayload
        } `
        {
            param([string]$Status, [string]$ErrorMessage, [object]$Metadata, [object]$Source)
            [ordered]@{ Status = $Status; Error = $ErrorMessage; Source = $Source; Metadata = $Metadata }
        }
    $driftDocument = Get-Content -LiteralPath $driftSummaryPath -Raw | ConvertFrom-Json
    if (($finalizationOrder -join ',') -ne 'validator,source' -or
        $driftSummary.Status -ne 'Failed' -or
        [string]$driftDocument.Status -ne 'Failed' -or
        [string]$driftDocument.Error -notmatch 'runner start and end') {
        throw 'Post-validator source drift was published as R8 Web Success'
    }
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'R8 Web performance source-authority tests PASS'
