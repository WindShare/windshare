[CmdletBinding()]
param(
    [ValidateRange(5, 5)]
    [int]$Count = 5,

    [string]$OutputRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
. (Join-Path $repositoryRoot 'scripts\d5-evidence.ps1')
. (Join-Path $repositoryRoot 'scripts\r8-web-performance-authority.ps1')
. (Join-Path $repositoryRoot 'scripts\r8-evidence-summary.ps1')
Assert-R8WindowsPerformanceAuthority $IsWindows
if ([string]::IsNullOrWhiteSpace($OutputRoot)) {
    $OutputRoot = Join-Path $repositoryRoot 'tmp\r8-web-performance'
}
$runID = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssfffffffZ')
$runRoot = Join-Path ([IO.Path]::GetFullPath($OutputRoot)) $runID
New-Item -ItemType Directory -Force -Path $runRoot | Out-Null

$sampleEnvironmentWasDefined = Test-Path Env:WINDSHARE_R8_PERFORMANCE_SAMPLES
$previousSampleEnvironment = $env:WINDSHARE_R8_PERFORMANCE_SAMPLES
$env:WINDSHARE_R8_PERFORMANCE_SAMPLES = [string]$Count

try {
    Push-Location $repositoryRoot
    try {
        $sourceIdentity = Get-D5SourceIdentitySummary (Get-D5SourceIdentity $repositoryRoot)
        # Host labels are diagnostic only. Do not let Windows management
        # telemetry prevent browser scenarios from reaching their real gates.
        $cpuName = [string]$env:PROCESSOR_IDENTIFIER
        if ([string]::IsNullOrWhiteSpace($cpuName)) {
            $cpuName = [Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()
        }
        $physicalMemoryBytes = if (-not $IsWindows) {
            [uint64][GC]::GetGCMemoryInfo().TotalAvailableMemoryBytes
        } else {
            $null
        }
        $playwrightRuntime = (& pnpm -C web exec node scripts/report-playwright-runtime.mjs) |
            ConvertFrom-Json
        if ($LASTEXITCODE -ne 0) { throw "Playwright runtime probe exited with code $LASTEXITCODE" }
        $browserRevisions = @($playwrightRuntime.browsers)
        [ordered]@{
            schema = 1
            generatedAt = [DateTime]::UtcNow.ToString('O')
            sampleCount = $Count
            operatingSystem = [Environment]::OSVersion.VersionString
            architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
            processArchitecture = [Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()
            cpu = $cpuName.Trim()
            logicalProcessorCount = [Environment]::ProcessorCount
            physicalMemoryBytes = $physicalMemoryBytes
            node = (& node --version)
            pnpm = (& pnpm --version)
            go = (& go version)
            playwright = [string]$playwrightRuntime.playwrightTestVersion
            playwrightCore = [string]$playwrightRuntime.playwrightCoreVersion
            browserRevisions = $browserRevisions
            source = $sourceIdentity
        } | ConvertTo-Json -Depth 8 |
            Set-Content -LiteralPath (Join-Path $runRoot 'environment.json') -Encoding utf8

        & pnpm -C web run test:browser:preflight 2>&1 |
            Tee-Object -FilePath (Join-Path $runRoot 'browser-preflight.txt')
        if ($LASTEXITCODE -ne 0) { throw "Browser preflight exited with code $LASTEXITCODE" }

        & pnpm -C web run build 2>&1 |
            Tee-Object -FilePath (Join-Path $runRoot 'web-build.txt')
        if ($LASTEXITCODE -ne 0) { throw "Web build exited with code $LASTEXITCODE" }

        & node web/scripts/report-r8-bundle.mjs 2>&1 |
            Tee-Object -FilePath (Join-Path $runRoot 'bundle.json')
        if ($LASTEXITCODE -ne 0) { throw "Bundle report exited with code $LASTEXITCODE" }

        & (Join-Path $repositoryRoot 'scripts\d5-windows-performance.ps1') `
            -Mode BrowserTests `
            -EvidenceRoot (Join-Path $runRoot 'd5-evidence') 2>&1 |
            Tee-Object -FilePath (Join-Path $runRoot 'browser.txt')
        if ($LASTEXITCODE -ne 0) { throw "R8 browser performance run exited with code $LASTEXITCODE" }
        $evidenceRunDirectories = @(Get-ChildItem `
            -LiteralPath (Join-Path $runRoot 'd5-evidence') `
            -Directory)
        if ($evidenceRunDirectories.Count -ne 1) {
            throw "Expected one published D5 evidence directory, found $($evidenceRunDirectories.Count)"
        }
        $evidenceDirectory = $evidenceRunDirectories[0].FullName
        $verifiedEvidence = Read-R8VerifiedD5Evidence $evidenceDirectory
        $evidenceManifest = $verifiedEvidence.Manifest
        $evidencePayload = $verifiedEvidence.Payload
        $trendLogs = @(Get-ChildItem `
            -LiteralPath (Join-Path $evidenceDirectory 'browser') `
            -Recurse `
            -Filter playwright.txt `
            -File)
        # A first-time D5 registration runs a cold phase and an audited repeat;
        # only the repeat is comparable to the already-registered browser phase.
        $trendLogCandidates = @($trendLogs | Where-Object {
            (Split-Path -Leaf (Split-Path -Parent $_.FullName)) -in @('browser', 'browser-repeat')
        })
        if ($trendLogCandidates.Count -ne 1) {
            throw "Expected one canonical D5 Playwright log, found $($trendLogCandidates.Count)"
        }
        $trendLog = $trendLogCandidates[0].FullName
        $trendPhase = Split-Path -Leaf (Split-Path -Parent $trendLog)
        $trendText = Get-Content -LiteralPath $trendLog -Raw
        $trendMatches = [regex]::Matches(
            $trendText,
            'WINDSHARE_R8_TREND (?<json>\{[^\r\n]+\})'
        )
        $trendRecords = @($trendMatches | ForEach-Object {
            $_.Groups['json'].Value | ConvertFrom-Json
        })
        $rawTrendRecordsPath = Join-Path $runRoot 'trend-records.raw.json'
        $trendRecords | ConvertTo-Json -Depth 16 |
            Set-Content -LiteralPath $rawTrendRecordsPath -Encoding utf8
        & node web/scripts/validate-r8-trends.mjs $rawTrendRecordsPath $Count 2>&1 |
            Tee-Object -FilePath (Join-Path $runRoot 'trend-validation.json')
        if ($LASTEXITCODE -ne 0) { throw "R8 trend contract validation exited with code $LASTEXITCODE" }
        $summaryMetadata = [pscustomobject][ordered]@{
            GeneratedAt = [DateTime]::UtcNow.ToString('O')
            EvidenceID = [string]$evidenceManifest.EvidenceID
            EvidenceStatus = [string]$evidencePayload.Status
            Phase = $trendPhase
            D5SourceUnchanged = [bool]$evidencePayload.Source.UnchangedForWholeRun
            D5Source = Get-D5SourceIdentitySummary $evidencePayload.Source.Start
            Records = $trendRecords
        }
        $metadataFactory = { return $summaryMetadata }
        $sourceCheckpoint = {
            $end = Get-D5SourceIdentitySummary (Get-D5SourceIdentity $repositoryRoot)
            return Get-R8SourceAuthorityCheckpoint $sourceIdentity $end $evidencePayload
        }
        $summaryFactory = {
            param([string]$Status, [string]$ErrorMessage, [object]$Metadata, [object]$Source)
            [ordered]@{
                schema = 2
                Status = $Status
                Error = if ([string]::IsNullOrWhiteSpace($ErrorMessage)) { $null } else { $ErrorMessage }
                generatedAt = [string]$Metadata.GeneratedAt
                sampleCount = $Count
                d5 = [ordered]@{
                    evidenceID = [string]$Metadata.EvidenceID
                    status = [string]$Metadata.EvidenceStatus
                    phase = [string]$Metadata.Phase
                    sourceUnchanged = [bool]$Metadata.D5SourceUnchanged
                    source = $Metadata.D5Source
                    endSource = if ($null -eq $Source) { $null } else { $Source.Value }
                }
                records = @($Metadata.Records)
            }
        }
        $summary = Complete-R8EvidenceSummary `
            (Join-Path $runRoot 'trends.json') `
            @() `
            $metadataFactory `
            $sourceCheckpoint `
            $summaryFactory
        if ($summary.Status -ne 'Success') {
            throw "R8 Web performance summary failed: $($summary.Error)"
        }
    } finally {
        Pop-Location
    }
} finally {
    if ($sampleEnvironmentWasDefined) {
        $env:WINDSHARE_R8_PERFORMANCE_SAMPLES = $previousSampleEnvironment
    } else {
        Remove-Item Env:WINDSHARE_R8_PERFORMANCE_SAMPLES -ErrorAction SilentlyContinue
    }
}

Write-Output "R8 Web performance evidence: $runRoot"
