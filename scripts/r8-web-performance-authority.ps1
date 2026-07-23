Set-StrictMode -Version Latest

function Assert-R8WindowsPerformanceAuthority([bool]$WindowsPlatform) {
    if (-not $WindowsPlatform) {
        throw 'R8 Web performance evidence requires the audited Windows D5 runner'
    }
}

function Assert-R8SourceAuthority(
    [object]$Start,
    [object]$End,
    [object]$EvidencePayload
) {
    if ([string]$EvidencePayload.Status -ne 'Success' -or
        -not [bool]$EvidencePayload.Source.UnchangedForWholeRun) {
        throw 'R8 timing requires successful, source-stable D5 browser evidence'
    }
    if (-not (Test-D5SourceIdentityEqual $Start $EvidencePayload.Source.Start)) {
        throw 'R8 environment and D5 browser evidence describe different source identities'
    }
    if (-not (Test-D5SourceIdentityEqual $Start $End)) {
        throw 'R8 workspace source changed between the runner start and end identities'
    }
    if (-not (Test-D5SourceIdentityEqual $End $EvidencePayload.Source.End)) {
        throw 'R8 end source identity does not match the audited D5 end identity'
    }
}

function Read-R8VerifiedD5Evidence([string]$Directory) {
    $root = [IO.Path]::GetFullPath($Directory)
    $manifestPath = Join-Path $root 'manifest.json'
    $payloadPath = Join-Path $root 'payload.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $payloadPath -PathType Leaf)) {
        throw "R8 D5 evidence is only partially finalized: $root"
    }
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $evidenceID = [string]$manifest.EvidenceID
    if ([string]::IsNullOrWhiteSpace($evidenceID) -or
        (Split-Path -Leaf $root) -cne $evidenceID) {
        throw 'R8 D5 evidence directory name does not match its content address'
    }
    [void](Test-D5EvidenceDirectory $root $evidenceID)
    # Payload fields are not trusted until the content address and every artifact
    # hash have been verified by Test-D5EvidenceDirectory.
    $payload = Get-Content -LiteralPath $payloadPath -Raw | ConvertFrom-Json
    return [pscustomobject][ordered]@{
        Directory = $root
        Manifest = $manifest
        Payload = $payload
    }
}

function Get-R8SourceAuthorityCheckpoint(
    [object]$Start,
    [object]$End,
    [object]$EvidencePayload
) {
    try {
        Assert-R8SourceAuthority $Start $End $EvidencePayload
        return [pscustomobject][ordered]@{
            Stable = $true
            Value = $End
            Error = ''
        }
    } catch {
        return [pscustomobject][ordered]@{
            Stable = $false
            Value = $End
            Error = [string]$_
        }
    }
}
