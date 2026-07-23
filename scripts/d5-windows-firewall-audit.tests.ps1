Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'd5-windows-firewall-audit.ps1')

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

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$performancePath = Join-Path $PSScriptRoot 'd5-windows-performance.ps1'
$tokens = $null
$parseErrors = $null
$performanceAST = [Management.Automation.Language.Parser]::ParseFile(
    $performancePath,
    [ref]$tokens,
    [ref]$parseErrors
)
if (@($parseErrors).Count -ne 0) {
    throw "D5 performance source has parser errors: $($parseErrors -join '; ')"
}
$retiredPlanLiterals = @($performanceAST.FindAll({
    param($node)
    $node -is [Management.Automation.Language.StringConstantExpressionAst] -and
        [string]$node.Value -in @('connectivity', 'connectivity.test.exe')
}, $true))
if ($retiredPlanLiterals.Count -ne 0) {
    throw 'D5 performance source contains a retired connectivity build or launch literal'
}
$durableEvidence = Import-D5FirewallOwnershipEvidence `
    $repositoryRoot `
    (Join-Path $PSScriptRoot 'd5-windows-firewall-ownership.json')
if ($durableEvidence.ExcludedRuleCount -ne 60 -or
    $durableEvidence.ExcludedProgramCount -ne 30 -or
    $durableEvidence.ExcludedSemanticPayloadSHA256 -cne
        'b631b4814182f302ea2bf5d0680f507187f9a602b7ad3d892147ff9a73031d2f') {
    throw 'Durable D5 firewall exclusion evidence did not reconstruct its approved payload'
}
if ([string]$durableEvidence.RetiredProgramTombstone.RelativeProgram -cne 'connectivity.test.exe' -or
    [string]$durableEvidence.RetiredProgramTombstone.DisplayName -cne 'connectivity.test' -or
    [string]$durableEvidence.RetiredProgramTombstone.Action -cne 'Block') {
    throw 'Durable D5 network manifest did not preserve the exact connectivity tombstone'
}
# The cleanup history corpus is untracked local forensic evidence, so its full
# reconstruction is asserted only where the corpus exists; a checkout without
# it (fresh CI) must degrade to an empty cleanup-owned identity set.
if ($durableEvidence.CleanupHistoryPresent) {
    if ($durableEvidence.CleanupOwnedRuleCount -ne 628 -or
        $durableEvidence.CleanupOwnedProgramCount -ne 314 -or
        $durableEvidence.CleanupOwnedSemanticPayloadSHA256 -cne
            'a8e826e7c86fe6a0efd1bb57e074e0d4de364ad7846dace5ff5c4433f2b2ce0b') {
        throw 'Durable D5 cleanup history did not reconstruct its approved payload'
    }
} elseif ($durableEvidence.CleanupOwnedRuleCount -ne 0 -or
    $durableEvidence.CleanupOwnedProgramCount -ne 0 -or
    @($durableEvidence.CleanupOwnedProgramPaths).Count -ne 0 -or
    @($durableEvidence.CleanupOwnedProgramRoots).Count -ne 0 -or
    @($durableEvidence.CleanupOwnedProgramNames).Count -ne 0 -or
    @($durableEvidence.CleanupOwnedProgramSHA256).Count -ne 0 -or
    $durableEvidence.CleanupOwnedSemanticPayloadSHA256 -cne
        (Get-D5OrdinalPayloadSHA256 @())) {
    throw 'A missing cleanup history corpus must yield empty cleanup-owned evidence'
}

function New-Rule([hashtable]$Override = @{}) {
    $values = [ordered]@{
        RuleID = 'rule-1'
        InstanceID = 'TCP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\Temp\go-build123\webrtc.test.exe'
        Program = 'C:\Temp\go-build123\webrtc.test.exe'
        DisplayName = 'webrtc.test'
        Direction = 'Inbound'
        Action = 'Block'
        Profile = @('Private', 'Public')
        Enabled = $true
        PolicyStoreSourceType = 'Local'
        Protocol = 'TCP'
        LocalPort = 'Any'
        RemotePort = 'Any'
        LocalAddress = 'Any'
        RemoteAddress = 'Any'
        ProgramExists = $false
        ProgramSHA256 = ''
        ProgramProcessIDs = @()
    }
    foreach ($key in $Override.Keys) {
        $values[$key] = $Override[$key]
    }
    return [pscustomobject]$values
}

$harnessRoot = 'C:\repo\tmp\d5-harness'
$program = 'C:\repo\tmp\d5-harness\webrtc.test.exe'
$unselectedProgram = 'C:\repo\tmp\d5-harness\windshare.exe'

$malformedProgram = [string]::Concat(
    'C:\repo\tmp\d5-harness\malformed',
    [char]0,
    '.exe'
)
foreach ($hostToken in @(
    'Any',
    'System',
    'relative\app.exe',
    'C:drive-relative.exe',
    'FileSystem::C:\repo\tmp\d5-harness\provider.exe',
    'HKLM:\Software\provider.exe',
    '%TEMP%\environment.exe',
    '$env:TEMP\environment.exe',
    '\\?\C:\repo\tmp\d5-harness\device.exe',
    '\\.\C:\repo\tmp\d5-harness\device.exe',
    '\Device\HarddiskVolume1\repo\tmp\d5-harness\device.exe',
    'file:///C:/repo/tmp/d5-harness/uri.exe',
    'https://example.test/uri.exe',
    ' C:\repo\tmp\d5-harness\spaced.exe',
    'C:\repo\tmp\d5-harness\*.exe',
    $malformedProgram
)) {
    if (Test-D5ProgramUnderRoot $hostToken $harnessRoot) {
        throw "Non-filesystem firewall token inherited stable-root ownership: $hostToken"
    }
}
if (-not (Test-D5ProgramUnderRoot $program $harnessRoot) -or
    (Test-D5ProgramUnderRoot 'C:\repo\tmp\outside.test.exe' $harnessRoot)) {
    throw 'Stable-root classification did not preserve exact absolute path containment'
}

function New-TestEvidence(
    [object[]]$ExcludedRules = @(),
    [object[]]$ExcludedProgramStates = @(),
    [string[]]$D5Hashes = @(),
    [string[]]$D5PathPatterns = @(),
    [string[]]$CleanupOwnedRoots = @(),
    [bool]$CleanupHistoryPresent = $true
) {
    return [pscustomobject]@{
        Sources = @()
        CleanupHistoryPresent = $CleanupHistoryPresent
        StableRelativePrograms = @('windshare.exe', 'webrtc.test.exe')
        D5HistoricalProgramSHA256 = $D5Hashes
        D5OwnedTemporaryPathPatterns = $D5PathPatterns
        CleanupOwnedProgramPaths = @()
        CleanupOwnedProgramRoots = $CleanupOwnedRoots
        CleanupOwnedProgramNames = @()
        CleanupOwnedProgramSHA256 = @()
        CleanupOwnedRuleCount = 0
        CleanupOwnedProgramCount = 0
        CleanupOwnedSemanticPayloadSHA256 = Get-D5OrdinalPayloadSHA256 @()
        RetiredProgramTombstone = [pscustomobject][ordered]@{
            RelativeProgram = 'connectivity.test.exe'
            DisplayName = 'connectivity.test'
            Action = 'Block'
            TCPGuid = 'E9A64ACF-24D6-4B94-91CE-D8E468113113'
            UDPGuid = '6A523662-D935-4D63-BE14-EEC446E3B720'
        }
        ExcludedRules = $ExcludedRules
        ExcludedProgramStates = $ExcludedProgramStates
        ExcludedRuleCount = $ExcludedRules.Count
        ExcludedProgramCount = $ExcludedProgramStates.Count
        ExcludedSemanticPayloadSHA256 = Get-D5OrdinalPayloadSHA256 @(
            $ExcludedRules | ForEach-Object { ConvertTo-D5RuleSignature $_ }
        )
        ExcludedExecutableStateProvenanceSHA256 = ('0' * 64)
    }
}

$policy = New-D5FirewallOwnershipPolicy `
    $harnessRoot `
    @($program, $unselectedProgram) `
    (New-TestEvidence) `
    @() `
    @('C:\Temp')
$emptyOwnershipBaseline = New-D5FirewallOwnershipBaseline @() $policy
$temporaryRule = New-Rule
$externalSameNameBaseline = New-D5FirewallOwnershipBaseline @($temporaryRule) $policy
Assert-D5FirewallPreflight @($temporaryRule) $policy $externalSameNameBaseline
if (-not (Test-D5ExternalStableProgramNameRule $temporaryRule $policy)) {
    throw 'External same-name firewall rule was not classified as informational'
}
$unreadableExternal = $temporaryRule.PSObject.Copy()
$unreadableExternal.ProgramExists = $true
$unreadableExternal.ProgramSHA256 = ''
$unreadableExternalBaseline = New-D5FirewallOwnershipBaseline @($unreadableExternal) $policy
Assert-D5FirewallPreflight @($unreadableExternal) $policy $unreadableExternalBaseline
$outsideObservedRoots = $temporaryRule.PSObject.Copy()
$outsideObservedRoots.Program = 'C:\outside-observed-roots\webrtc.test.exe'
$outsideObservedRootsBaseline = New-D5FirewallOwnershipBaseline @($outsideObservedRoots) $policy
Assert-D5FirewallPreflight @($outsideObservedRoots) $policy $outsideObservedRootsBaseline
$expectedImage = '\Device\HarddiskVolume1\repo\tmp\d5-harness\connectivity.test.exe'
$externalImage = '\Device\HarddiskVolume1\temp\connectivity.test.exe'
$externalOnly = @(Select-D5ExactProcessIDs $expectedImage @(
    [pscustomobject]@{ ProcessID = 1001; ImagePath = $externalImage }
))
if ($externalOnly.Count -ne 0) {
    throw 'External same-name process was attributed to the retired D5 path'
}
Assert-Throws {
    Select-D5ExactProcessIDs $expectedImage @(
        [pscustomobject]@{ ProcessID = 1003; ImagePath = '' }
    )
} 'Cannot classify same-name PID 1003 without an executable image path'
$exactProcess = @(Select-D5ExactProcessIDs $expectedImage @(
    [pscustomobject]@{ ProcessID = 1001; ImagePath = $externalImage }
    [pscustomobject]@{ ProcessID = 1002; ImagePath = $expectedImage.ToUpperInvariant() }
))
if ($exactProcess.Count -ne 1 -or $exactProcess[0] -ne 1002) {
    throw 'Exact retired-path process selector did not preserve full image identity'
}
$zero = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @()
    NewRelevantEvents = @()
}
Assert-D5FirewallPreflight @() $policy $emptyOwnershipBaseline
Assert-D5FirewallUnchanged $zero $policy $emptyOwnershipBaseline

$performanceSource = Get-Content -LiteralPath $performancePath -Raw
foreach ($requiredFragment in @(
    'Get-D5ActiveStoreFirewallRules',
    'Assert-D5ActiveStoreFirewallTransition',
    'ActiveStoreDelta',
    'retired connectivity firewall tombstone remains',
    'NetworkTests cold registration',
    'active-store-before.json',
    'active-store-after.json'
)) {
    if (-not $performanceSource.Contains($requiredFragment)) {
        throw "D5 performance source omitted global ActiveStore control: $requiredFragment"
    }
}
$externalSnapshotIndex = $performanceSource.IndexOf(
    '$script:externalFirewallDebtAtStart = New-D5ExternalFirewallDebtSnapshot',
    [StringComparison]::Ordinal
)
$playwrightSnapshotIndex = $performanceSource.IndexOf(
    '$script:playwrightFirewallDebtAtStart = Get-D5PlaywrightFirewallDebtSnapshot',
    [StringComparison]::Ordinal
)
if ($externalSnapshotIndex -lt 0 -or $playwrightSnapshotIndex -lt 0) {
    throw 'Initial firewall snapshot construction is missing'
}
$initialEvidenceIndex = $performanceSource.IndexOf(
    'ExternalDebt = $script:externalFirewallDebtAtStart',
    $externalSnapshotIndex,
    [StringComparison]::Ordinal
)
$externalAssertionIndex = $performanceSource.IndexOf(
    'Assert-D5ExternalFirewallDebtSnapshot',
    $initialEvidenceIndex,
    [StringComparison]::Ordinal
)
$playwrightAssertionIndex = $performanceSource.IndexOf(
    'Assert-D5PlaywrightFirewallDebtSnapshot',
    $initialEvidenceIndex,
    [StringComparison]::Ordinal
)
if ($initialEvidenceIndex -le $externalSnapshotIndex -or
    $initialEvidenceIndex -le $playwrightSnapshotIndex -or
    $externalAssertionIndex -le $initialEvidenceIndex -or
    $playwrightAssertionIndex -le $initialEvidenceIndex) {
    throw 'Initial firewall evidence must serialize pure snapshots before cap assertions'
}
$firewallPolicySource = @(
    $performanceSource
    Get-Content -LiteralPath (Join-Path $PSScriptRoot 'd5-windows-firewall-audit.ps1') -Raw
    Get-Content -LiteralPath (Join-Path $PSScriptRoot 'd5-windows-firewall-ownership.ps1') -Raw
) -join [Environment]::NewLine
if ($firewallPolicySource -match
    '(?i)(?:New|Set|Remove|Enable|Disable|Copy)-NetFirewallRule') {
    throw 'D5 observation policy must never mutate Windows Firewall rules'
}

function New-PlaywrightRule(
    [string]$Program,
    [ValidateSet('TCP', 'UDP')]
    [string]$Protocol,
    [hashtable]$Override = @{}
) {
    $guid = if ($Protocol -eq 'TCP') {
        '01234567-89AB-CDEF-0123-456789ABCDEF'
    } else {
        'FEDCBA98-7654-3210-FEDC-BA9876543210'
    }
    $identity = "$Protocol Query User{$guid}$Program"
    $values = @{
        RuleID = $identity
        InstanceID = $identity
        Program = $Program
        Protocol = $Protocol
        Action = 'Block'
        Profile = @('Private')
    }
    foreach ($key in $Override.Keys) {
        $values[$key] = $Override[$key]
    }
    return New-Rule $values
}

$playwrightRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-playwright-firewall-' + [guid]::NewGuid().ToString('N')
)
try {
    $chromiumProgram = Join-Path $playwrightRoot (
        'chromium_headless_shell-1228\chrome-headless-shell-win64\chrome-headless-shell.exe'
    )
    $firefoxProgram = Join-Path $playwrightRoot 'firefox-1532\firefox\firefox.exe'
    $webkitProgram = Join-Path $playwrightRoot 'webkit-2311\Playwright.exe'
    $webkitNetworkProgram = Join-Path $playwrightRoot 'webkit-2311\WebKitNetworkProcess.exe'
    $webkitWebProgram = Join-Path $playwrightRoot 'webkit-2311\WebKitWebProcess.exe'
    $webkitGPUProgram = Join-Path $playwrightRoot 'webkit-2311\WebKitGPUProcess.exe'
    foreach ($path in @(
        $chromiumProgram,
        $firefoxProgram,
        $webkitProgram,
        $webkitNetworkProgram,
        $webkitWebProgram,
        $webkitGPUProgram
    )) {
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $path) | Out-Null
        New-Item -ItemType File -Force -Path $path | Out-Null
    }
    $browserManifest = @(
        [pscustomobject]@{
            browserName = 'chromium'
            networkExecutables = @($chromiumProgram)
            cacheRoot = $playwrightRoot
        }
        [pscustomobject]@{
            browserName = 'firefox'
            networkExecutables = @($firefoxProgram)
            cacheRoot = $playwrightRoot
        }
        [pscustomobject]@{
            browserName = 'webkit'
            networkExecutables = @(
                $webkitProgram,
                $webkitNetworkProgram,
                $webkitWebProgram
            )
            cacheRoot = $playwrightRoot
        }
    )
    $playwrightPolicy = New-D5PlaywrightFirewallDebtPolicy $playwrightRoot $browserManifest
    $chromiumPair = @(
        New-PlaywrightRule $chromiumProgram 'TCP'
        New-PlaywrightRule $chromiumProgram 'UDP'
    )
    $webkitPair = @(
        New-PlaywrightRule $webkitProgram 'TCP' @{ Action = 'Allow'; Profile = @('Private', 'Public') }
        New-PlaywrightRule $webkitProgram 'UDP' @{ Action = 'Allow'; Profile = @('Private', 'Public') }
    )
    $firefoxPair = @(
        New-PlaywrightRule $firefoxProgram 'TCP'
        New-PlaywrightRule $firefoxProgram 'UDP'
    )
    $webkitNetworkPair = @(
        New-PlaywrightRule $webkitNetworkProgram 'TCP'
        New-PlaywrightRule $webkitNetworkProgram 'UDP'
    )

    $initialDebt = Get-D5PlaywrightFirewallDebtSnapshot $chromiumPair $playwrightPolicy 'Synthetic preflight'
    if ($initialDebt.PairCount -ne 1 -or
        [string]::IsNullOrWhiteSpace([string]$initialDebt.CleanupAdvisory)) {
        throw 'Playwright debt preflight did not record its exact pair and cleanup advisory'
    }
    $afterOnePair = @($chromiumPair + $webkitPair)
    $acceptedTransition = Assert-D5ActiveStoreFirewallTransition `
        $chromiumPair `
        $chromiumPair `
        $afterOnePair `
        $harnessRoot `
        $playwrightPolicy `
        $false `
        @() `
        'Synthetic browser run'
    if ($acceptedTransition.PlaywrightDebt.PairCount -ne 2 -or
        $acceptedTransition.ExternalDebt.NetRuleGrowth -ne 2) {
        throw 'One exact installed Playwright pair was not accepted as bounded debt'
    }
    $alternateWebKitTransition = Assert-D5ActiveStoreFirewallTransition `
        $chromiumPair `
        $chromiumPair `
        @($chromiumPair + $webkitNetworkPair) `
        $harnessRoot `
        $playwrightPolicy `
        $false `
        @() `
        'Synthetic WebKit network-process run'
    if ($alternateWebKitTransition.PlaywrightDebt.PairCount -ne 2) {
        throw 'A canonical WebKit network-process pair was not accepted as bounded debt'
    }

    $partialTransition = Assert-D5ActiveStoreFirewallTransition `
        $chromiumPair `
        $chromiumPair `
        @($chromiumPair + $webkitPair[0]) `
        $harnessRoot `
        $playwrightPolicy `
        $false `
        @() `
        'Partial pair'
    if ($partialTransition.ExternalDebt.AddedRuleCount -ne 1 -or
        @($partialTransition.PlaywrightDebt.Issues).Count -ne 1) {
        throw 'A partial external Playwright pair was not retained as warning telemetry'
    }
    Assert-Throws {
        $mixedActionPair = @(
            New-PlaywrightRule $webkitProgram 'TCP'
            New-PlaywrightRule $webkitProgram 'UDP' @{ Action = 'Allow' }
        )
        Assert-D5PlaywrightFirewallRulePair `
            $mixedActionPair `
            (Resolve-D5PlaywrightFirewallProgram $webkitProgram $playwrightRoot) `
            'Mixed action'
    } 'mismatched TCP/UDP Playwright firewall semantics'
    Assert-Throws {
        $mixedProfilePair = @(
            New-PlaywrightRule $webkitProgram 'TCP'
            New-PlaywrightRule $webkitProgram 'UDP' @{ Profile = @('Private', 'Public') }
        )
        Assert-D5PlaywrightFirewallRulePair `
            $mixedProfilePair `
            (Resolve-D5PlaywrightFirewallProgram $webkitProgram $playwrightRoot) `
            'Mixed profile'
    } 'mismatched TCP/UDP Playwright firewall semantics'
    Assert-Throws {
        $duplicateProfilePair = @(
            New-PlaywrightRule $webkitProgram 'TCP' @{ Profile = @('Private', 'Private') }
            New-PlaywrightRule $webkitProgram 'UDP'
        )
        Assert-D5PlaywrightFirewallRulePair `
            $duplicateProfilePair `
            (Resolve-D5PlaywrightFirewallProgram $webkitProgram $playwrightRoot) `
            'Duplicate profile'
    } 'duplicate or unsupported Playwright firewall profiles'
    Assert-Throws {
        $badGuidPair = @(
            New-PlaywrightRule $webkitProgram 'TCP'
            New-PlaywrightRule $webkitProgram 'UDP'
        )
        $badGuidPair[0].RuleID = $badGuidPair[0].RuleID.Replace(
            '01234567-89AB-CDEF-0123-456789ABCDEF',
            '0123456789AB-CDEF-0123-456789ABCDEF'
        )
        $badGuidPair[0].InstanceID = $badGuidPair[0].RuleID
        Assert-D5PlaywrightFirewallRulePair `
            $badGuidPair `
            (Resolve-D5PlaywrightFirewallProgram $webkitProgram $playwrightRoot) `
            'Malformed GUID'
    } 'non-Query-User Playwright firewall rule'
    $duplicateRevisionDebt = Get-D5PlaywrightFirewallDebtSnapshot `
        @($webkitPair + $webkitNetworkPair) `
        $playwrightPolicy `
        'Duplicate WebKit revision'
    if ($duplicateRevisionDebt.Entries.Count -ne 2 -or
        @($duplicateRevisionDebt.Issues).Count -ne 1 -or
        [string]$duplicateRevisionDebt.Issues[0].Problem -notmatch
            'more than one Playwright firewall program for revision webkit/2311') {
        throw 'Duplicate Playwright revision was not retained as pure issue telemetry'
    }
    $gpuPair = @(
        New-PlaywrightRule $webkitGPUProgram 'TCP'
        New-PlaywrightRule $webkitGPUProgram 'UDP'
    )
    $noncandidateDebt = Get-D5PlaywrightFirewallDebtSnapshot `
        $gpuPair `
        $playwrightPolicy `
        'Noncandidate WebKit process'
    if ($noncandidateDebt.ObservedRuleCount -ne 2 -or
        $noncandidateDebt.Entries.Count -ne 0 -or
        @($noncandidateDebt.Issues).Count -ne 2) {
        throw 'Noncandidate Playwright rules were not retained as pure issue telemetry'
    }
    $changed = $chromiumPair[0].PSObject.Copy()
    $changed.LocalPort = '49152'
    $alteredTransition = Assert-D5ActiveStoreFirewallTransition `
        $chromiumPair `
        $chromiumPair `
        @($changed, $chromiumPair[1]) `
        $harnessRoot `
        $playwrightPolicy `
        $false `
        @() `
        'Altered pair'
    if ($alteredTransition.ExternalDebt.ChangedRuleCount -ne 1) {
        throw 'Altered external firewall semantics were not retained as warning telemetry'
    }
    $wildcard = New-PlaywrightRule 'Any' 'TCP'
    $wildcardTransition = Assert-D5ActiveStoreFirewallTransition `
        $chromiumPair `
        $chromiumPair `
        @($chromiumPair + $wildcard) `
        $harnessRoot `
        $playwrightPolicy `
        $false `
        @() `
        'Wildcard rule'
    if ($wildcardTransition.ExternalDebt.AddedRuleCount -ne 1 -or
        [string]$wildcardTransition.ExternalDebt.Added[0].Class -cne 'Other') {
        throw 'Unrelated external firewall growth was not retained as warning telemetry'
    }
    Assert-Throws {
        Assert-D5ActiveStoreFirewallTransition `
            $chromiumPair `
            $chromiumPair `
            @($chromiumPair + $firefoxPair + $webkitPair) `
            $harnessRoot `
            $playwrightPolicy `
            $false `
            @() `
            'Rapid accumulation'
    } 'per-run Playwright firewall pair cap'
    Assert-Throws {
        $halfPairsAtStart = @($chromiumPair + $firefoxPair[0] + $webkitPair[0])
        Assert-D5ActiveStoreFirewallTransition `
            $halfPairsAtStart `
            $halfPairsAtStart `
            @($chromiumPair + $firefoxPair + $webkitPair) `
            $harnessRoot `
            $playwrightPolicy `
            $false `
            @() `
            'Completed stale half-pairs'
    } 'per-run Playwright firewall pair cap'
    $removalTransition = Assert-D5ActiveStoreFirewallTransition `
        $chromiumPair `
        $chromiumPair `
        @() `
        $harnessRoot `
        $playwrightPolicy `
        $false `
        @() `
        'Removal'
    if ($removalTransition.ExternalDebt.RemovedRuleCount -ne 2 -or
        $removalTransition.ExternalDebt.NetRuleGrowth -ne -2) {
        throw 'External firewall removal was not retained as warning telemetry'
    }

    $rapidExternalGrowth = @(
        foreach ($index in 1..5) {
            New-Rule @{
                RuleID = "external-rule-$index"
                InstanceID = "external-rule-$index"
                Program = "C:\\external\\app-$index.exe"
            }
        }
    )
    Assert-Throws {
        Assert-D5ActiveStoreFirewallTransition `
            @() `
            @() `
            $rapidExternalGrowth `
            $harnessRoot `
            $null `
            $false `
            @() `
            'Rapid external accumulation'
    } 'per-run external firewall growth cap'

    $previousExternalCap = $script:D5ExternalFirewallDebtMaxTotalRules
    try {
        $script:D5ExternalFirewallDebtMaxTotalRules = 2
        $overTotalSnapshot = New-D5ExternalFirewallDebtSnapshot `
            @($rapidExternalGrowth[0..2]) `
            $harnessRoot `
            $null
        if ($overTotalSnapshot.Entries.Count -ne 3 -or
            $overTotalSnapshot.Summary.RuleCount -ne 3 -or
            [string]::IsNullOrWhiteSpace(
                [string]$overTotalSnapshot.Summary.SemanticPayloadSHA256
            ) -or
            @($overTotalSnapshot.Violations).Count -ne 1 -or
            @($overTotalSnapshot.Classes | Where-Object Name -eq 'Other')[0].RuleCount -ne 3) {
            throw 'Over-limit external snapshot did not retain exact entries, classes and violations'
        }
        Assert-Throws {
            Assert-D5ExternalFirewallDebtSnapshot $overTotalSnapshot 'External debt cap'
        } 'total external firewall debt cap'
    } finally {
        $script:D5ExternalFirewallDebtMaxTotalRules = $previousExternalCap
    }

    $previousGoBuildCap = $script:D5GoBuildFirewallDebtMaxTotalRules
    try {
        $script:D5GoBuildFirewallDebtMaxTotalRules = 1
        $goBuildDebt = @(
            New-Rule @{ RuleID = 'go-build-rule-1'; InstanceID = 'go-build-rule-1' }
            New-Rule @{ RuleID = 'go-build-rule-2'; InstanceID = 'go-build-rule-2'; Protocol = 'UDP' }
        )
        $overGoBuildSnapshot = New-D5ExternalFirewallDebtSnapshot `
            $goBuildDebt `
            $harnessRoot `
            $null
        if ($overGoBuildSnapshot.Entries.Count -ne 2 -or
            @($overGoBuildSnapshot.Violations).Count -ne 1 -or
            @($overGoBuildSnapshot.Classes | Where-Object Name -eq 'GoBuildDiagnostic')[0].RuleCount -ne 2) {
            throw 'Over-limit Go-build snapshot did not retain exact class evidence'
        }
        Assert-Throws {
            Assert-D5ExternalFirewallDebtSnapshot $overGoBuildSnapshot 'Go-build debt cap'
        } 'total Go-build firewall debt cap'
    } finally {
        $script:D5GoBuildFirewallDebtMaxTotalRules = $previousGoBuildCap
    }

    $retainedRules = [Collections.Generic.List[object]]::new()
    foreach ($revision in @('1529', '1530', '1531')) {
        $retainedProgram = Join-Path $playwrightRoot "firefox-$revision\firefox\firefox.exe"
        $retainedRules.Add((New-PlaywrightRule $retainedProgram 'TCP'))
        $retainedRules.Add((New-PlaywrightRule $retainedProgram 'UDP'))
    }
    $retainedTelemetry = Get-D5PlaywrightFirewallDebtSnapshot `
        @($retainedRules) `
        $playwrightPolicy `
        'Retained debt telemetry'
    if ($retainedTelemetry.Entries.Count -ne 3 -or
        $retainedTelemetry.ObservedRuleCount -ne 6 -or
        $retainedTelemetry.Summary.RuleCount -ne 6 -or
        [string]::IsNullOrWhiteSpace(
            [string]$retainedTelemetry.Summary.SemanticPayloadSHA256
        ) -or
        @($retainedTelemetry.Violations).Count -ne 1) {
        throw 'Over-limit Playwright snapshot did not retain exact entries and violation evidence'
    }
    Assert-Throws {
        Assert-D5PlaywrightFirewallDebtSnapshot $retainedTelemetry 'Retained debt telemetry'
    } 'retained Playwright firewall revision cap'
} finally {
    Remove-Item -LiteralPath $playwrightRoot -Recurse -Force -ErrorAction SilentlyContinue
}

$broadHarnessRule = New-Rule @{
    RuleID = 'TCP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\repo\tmp\d5-harness\webrtc.test.exe'
    InstanceID = 'TCP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\repo\tmp\d5-harness\webrtc.test.exe'
    Program = 'C:\repo\tmp\d5-harness\webrtc.test.exe'
    Action = 'Allow'
}
$broadHarnessUDP = New-Rule @{
    RuleID = 'UDP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\repo\tmp\d5-harness\webrtc.test.exe'
    InstanceID = 'UDP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\repo\tmp\d5-harness\webrtc.test.exe'
    Program = 'C:\repo\tmp\d5-harness\webrtc.test.exe'
    Protocol = 'UDP'
    Action = 'Allow'
}
Assert-D5FirewallPreflight `
    @($broadHarnessRule, $broadHarnessUDP) `
    $policy `
    $emptyOwnershipBaseline
Assert-Throws {
    $changedStableRule = $broadHarnessRule.PSObject.Copy()
    $changedStableRule.LocalPort = '49152'
    Assert-D5ActiveStoreFirewallTransition `
        @($broadHarnessRule, $broadHarnessUDP) `
        @($broadHarnessRule, $broadHarnessUDP) `
        @($changedStableRule, $broadHarnessUDP) `
        $harnessRoot `
        $null `
        $false `
        @() `
        'Stable semantic drift'
} 'altered stable-root firewall semantics'
Assert-Throws {
    Assert-D5ActiveStoreFirewallTransition `
        @($broadHarnessRule, $broadHarnessUDP) `
        @($broadHarnessRule, $broadHarnessUDP) `
        @($broadHarnessRule) `
        $harnessRoot `
        $null `
        $false `
        @() `
        'Stable identity removal'
} 'removed stable-root firewall identity'
Assert-Throws {
    $addedStableRule = $broadHarnessRule.PSObject.Copy()
    $addedStableRule.RuleID = 'stable-extra-rule'
    $addedStableRule.InstanceID = 'stable-extra-rule'
    Assert-D5ActiveStoreFirewallTransition `
        @($broadHarnessRule, $broadHarnessUDP) `
        @($broadHarnessRule, $broadHarnessUDP) `
        @($broadHarnessRule, $broadHarnessUDP, $addedStableRule) `
        $harnessRoot `
        $null `
        $false `
        @() `
        'Stable identity addition'
} 'added stable-root firewall identity outside an explicit cold-registration phase'
Assert-Throws {
    $unexpected = $broadHarnessRule.PSObject.Copy()
    $unexpected.Program = 'C:\repo\tmp\d5-harness\unlisted.test.exe'
    Assert-D5FirewallPreflight @($unexpected) $policy $emptyOwnershipBaseline
} 'unmanifested program under the stable harness root'

$retiredProgram = 'C:\repo\tmp\d5-harness\connectivity.test.exe'
$retiredTCP = New-D5RetiredProgramTombstoneRule `
    $retiredProgram `
    'connectivity.test' `
    'Block' `
    'TCP' `
    'E9A64ACF-24D6-4B94-91CE-D8E468113113'
$retiredUDP = New-D5RetiredProgramTombstoneRule `
    $retiredProgram `
    'connectivity.test' `
    'Block' `
    'UDP' `
    '6A523662-D935-4D63-BE14-EEC446E3B720'
$retiredBaseline = New-D5FirewallOwnershipBaseline @($retiredTCP, $retiredUDP) $policy
Assert-D5FirewallPreflight @($retiredTCP, $retiredUDP) $policy $retiredBaseline
Assert-D5FirewallUnchanged `
    ([pscustomobject]@{
        BeforeRules = @($retiredTCP, $retiredUDP)
        AfterRules = @($retiredTCP, $retiredUDP)
        NewRelevantEvents = @()
    }) `
    $policy `
    $retiredBaseline
if ($retiredBaseline.RetiredTombstoneRuleCount -ne 2 -or
    $retiredBaseline.RetiredTombstoneProgramCount -ne 1) {
    throw 'Exact retired connectivity pair was not classified as one tombstone'
}

Assert-D5RetiredProgramRuntimeStates `
    @([pscustomobject]@{ Program = $retiredProgram; Exists = $false; ProcessIDs = @() }) `
    $policy `
    'Synthetic runtime'
Assert-Throws {
    Assert-D5RetiredProgramRuntimeStates `
        @([pscustomobject]@{ Program = $retiredProgram; Exists = $true; ProcessIDs = @() }) `
        $policy `
        'Synthetic runtime'
} 'executable reintroduced'
Assert-Throws {
    Assert-D5RetiredProgramRuntimeStates `
        @([pscustomobject]@{ Program = $retiredProgram; Exists = $false; ProcessIDs = @(4123) }) `
        $policy `
        'Synthetic runtime'
} 'live retired connectivity process'
Assert-Throws {
    Assert-D5RetiredProgramRuntimeStates `
        @([pscustomobject]@{ Program = $retiredProgram; Exists = $false }) `
        $policy `
        'Synthetic runtime'
} 'cannot prove.*no matching process'

Assert-Throws {
    New-D5FirewallOwnershipBaseline @($retiredTCP) $policy
} 'zero retired rules or the exact retired TCP/UDP pair'
Assert-Throws {
    $altered = $retiredTCP.PSObject.Copy()
    $altered.DisplayName = 'connectivity wildcard'
    New-D5FirewallOwnershipBaseline @($altered, $retiredUDP) $policy
} 'altered retired connectivity firewall rule'
Assert-Throws {
    $alteredIdentity = $retiredTCP.PSObject.Copy()
    $alteredIdentity.RuleID = 'TCP Query User{E9A64ACF-24D6-4B94-91CE-D8E468113113}C:\repo\tmp\d5-harness\other.test.exe'
    New-D5FirewallOwnershipBaseline @($alteredIdentity, $retiredUDP) $policy
} 'altered retired connectivity firewall rule'
Assert-Throws {
    $reintroduced = $retiredTCP.PSObject.Copy()
    $reintroduced.ProgramExists = $true
    $reintroduced.ProgramSHA256 = '0' * 64
    New-D5FirewallOwnershipBaseline @($reintroduced, $retiredUDP) $policy
} 'executable reintroduced'
Assert-Throws {
    $live = $retiredTCP.PSObject.Copy()
    $live.ProgramProcessIDs = @(4123)
    New-D5FirewallOwnershipBaseline @($live, $retiredUDP) $policy
} 'live retired connectivity process'
Assert-Throws {
    $wildcard = $retiredTCP.PSObject.Copy()
    $wildcard.Program = 'C:\repo\tmp\d5-harness\*.test.exe'
    New-D5FirewallOwnershipBaseline @($wildcard, $retiredUDP) $policy
} 'outside its observed roots|unmanifested program under the stable harness root'

Assert-D5ProgramsExcludeRetiredTombstone `
    @($program, $unselectedProgram) `
    $policy `
    'Synthetic launch plan'
Assert-Throws {
    Assert-D5ProgramsExcludeRetiredTombstone @($retiredProgram) $policy 'Synthetic launch plan'
} 'reintroduced the retired connectivity executable'
Assert-Throws {
    Assert-D5ProgramsExcludeRetiredTombstone `
        @('C:\alternate\connectivity.test.exe') `
        $policy `
        'Synthetic launch plan'
} 'reintroduced the retired connectivity executable'
Assert-Throws {
    Assert-D5ProgramsExcludeRetiredTombstone `
        @('C:\repo\tmp\d5-harness\*.test.exe') `
        $policy `
        'Synthetic launch plan'
} 'wildcard executable path'

$narrowHarnessRule = New-Rule @{
    RuleID = 'TCP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\repo\tmp\d5-harness\webrtc.test.exe'
    InstanceID = 'TCP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\repo\tmp\d5-harness\webrtc.test.exe'
    Program = 'C:\repo\tmp\d5-harness\webrtc.test.exe'
    Action = 'Block'
    LocalPort = '443'
}
Assert-Throws {
    Assert-D5FirewallPreflight @($narrowHarnessRule) $policy $emptyOwnershipBaseline
} 'exact bounded Query User shape'

$guid = '01234567-89AB-CDEF-0123-456789ABCDEF'
$coldTCP = New-Rule @{
    InstanceID = "TCP Query User{$guid}$program"
    RuleID = "TCP Query User{$guid}$program"
    Program = $program
    Protocol = 'TCP'
}
$coldUDP = New-Rule @{
    InstanceID = "UDP Query User{$guid}$program"
    RuleID = "UDP Query User{$guid}$program"
    Program = $program
    Protocol = 'UDP'
}
function New-ColdEvent([int]$EventID, [string]$RuleID, [string]$Action = '2') {
    return [pscustomobject]@{
        EventID = $EventID
        Fields = [pscustomobject]@{
            ApplicationPath = $program
            RuleId = $RuleID
            Direction = '1'
            Action = $Action
            Profiles = '6'
            LocalPorts = '*'
            RemotePorts = '*'
            LocalAddresses = '*'
            RemoteAddresses = '*'
        }
    }
}
$cold = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @($coldTCP, $coldUDP)
    NewRelevantEvents = @(
        New-ColdEvent 2097 $coldTCP.InstanceID
        New-ColdEvent 2097 $coldUDP.InstanceID
    )
}
$coldActiveStore = Assert-D5ActiveStoreFirewallTransition `
    @() `
    @() `
    @($coldTCP, $coldUDP) `
    $harnessRoot `
    $null `
    $true `
    @($program) `
    'Synthetic cold registration'
if ($coldActiveStore.StableRootDelta.AddedRuleCount -ne 2) {
    throw 'Exact cold Block pair did not produce a two-rule stable-root delta'
}
Assert-Throws {
    $coldAllowTCP = $coldTCP.PSObject.Copy()
    $coldAllowTCP.Action = 'Allow'
    $coldAllowUDP = $coldUDP.PSObject.Copy()
    $coldAllowUDP.Action = 'Allow'
    Assert-D5ActiveStoreFirewallTransition `
        @() `
        @() `
        @($coldAllowTCP, $coldAllowUDP) `
        $harnessRoot `
        $null `
        $true `
        @($program) `
        'Synthetic cold Allow registration'
} 'requires an exact Block TCP/UDP pair'
Assert-D5ColdFirewallRegistration `
    $cold `
    @($program) `
    $policy `
    $emptyOwnershipBaseline
$coldWithExternalEvent = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @($coldTCP, $coldUDP)
    NewRelevantEvents = @($cold.NewRelevantEvents) + @([pscustomobject]@{
        EventID = 2097
        Fields = [pscustomobject]@{
            ApplicationPath = 'C:\Temp\go-build999\b001\exe\host-tool.exe'
        }
    })
}
Assert-D5ColdFirewallRegistration `
    $coldWithExternalEvent `
    @($program) `
    $policy `
    $emptyOwnershipBaseline

$missingHalfPair = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @($coldTCP)
    NewRelevantEvents = @(New-ColdEvent 2097 $coldTCP.InstanceID)
}
Assert-Throws {
    Assert-D5ColdFirewallRegistration `
        $missingHalfPair `
        @($program) `
        $policy `
        $emptyOwnershipBaseline
} 'exactly one TCP rule and one UDP rule'

$unselectedTCP = New-Rule @{
    InstanceID = "TCP Query User{$guid}$unselectedProgram"
    RuleID = "TCP Query User{$guid}$unselectedProgram"
    Program = $unselectedProgram
    Protocol = 'TCP'
}
$unselectedUDP = New-Rule @{
    InstanceID = "UDP Query User{$guid}$unselectedProgram"
    RuleID = "UDP Query User{$guid}$unselectedProgram"
    Program = $unselectedProgram
    Protocol = 'UDP'
}
$unselectedCold = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @($unselectedTCP, $unselectedUDP)
    NewRelevantEvents = @()
}
Assert-Throws {
    Assert-D5ColdFirewallRegistration `
        $unselectedCold `
        @($program) `
        $policy `
        $emptyOwnershipBaseline
} 'unexpected firewall program'

$allowTCP = New-Rule @{
    InstanceID = "TCP Query User{$guid}$program"
    RuleID = "TCP Query User{$guid}$program"
    Program = $program
    Protocol = 'TCP'
    Action = 'Allow'
}
$allowUDP = New-Rule @{
    InstanceID = "UDP Query User{$guid}$program"
    RuleID = "UDP Query User{$guid}$program"
    Program = $program
    Protocol = 'UDP'
    Action = 'Allow'
}
$allowCold = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @($allowTCP, $allowUDP)
    NewRelevantEvents = @(
        New-ColdEvent 2097 $allowTCP.InstanceID '3'
        New-ColdEvent 2097 $allowUDP.InstanceID '3'
    )
}
Assert-Throws {
    Assert-D5ColdFirewallRegistration `
        $allowCold `
        @($program) `
        $policy `
        $emptyOwnershipBaseline
} 'requires an exact Block TCP/UDP pair'

$badScopeCold = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @($coldTCP, $coldUDP)
    NewRelevantEvents = @(New-ColdEvent 2097 $coldTCP.InstanceID)
}
$badScopeCold.NewRelevantEvents[0].Fields.Profiles = '7'
Assert-Throws {
    Assert-D5ColdFirewallRegistration `
        $badScopeCold `
        @($program) `
        $policy `
        $emptyOwnershipBaseline
} 'exact bounded Query User shape'

$semanticCases = @(
    @{ Field = 'Direction'; Value = 'Outbound'; Error = 'exact bounded Query User shape' },
    @{ Field = 'Action'; Value = 'Allow'; Error = 'changed stable-root firewall identity or semantics' },
    @{ Field = 'Profile'; Value = @('Private'); Error = 'exact bounded Query User shape' },
    @{ Field = 'LocalPort'; Value = '443'; Error = 'exact bounded Query User shape' },
    @{ Field = 'RemotePort'; Value = '443'; Error = 'exact bounded Query User shape' },
    @{ Field = 'LocalAddress'; Value = '127.0.0.1'; Error = 'exact bounded Query User shape' },
    @{ Field = 'RemoteAddress'; Value = '127.0.0.1'; Error = 'exact bounded Query User shape' },
    @{ Field = 'Program'; Value = 'C:\Temp\go-build456\webrtc.test.exe'; Error = 'Preflight requires exactly one TCP rule and one UDP rule|changed stable-root firewall identity or semantics' }
)
foreach ($case in $semanticCases) {
    $changed = $coldTCP.PSObject.Copy()
    $changed.($case.Field) = $case.Value
    $audit = [pscustomobject]@{
        BeforeRules = @($coldTCP, $coldUDP)
        AfterRules = @($changed, $coldUDP)
        NewRelevantEvents = @()
    }
    Assert-Throws {
        Assert-D5FirewallUnchanged $audit $policy $emptyOwnershipBaseline
    } $case.Error
}

$eventAudit = [pscustomobject]@{
    BeforeRules = @($coldTCP, $coldUDP)
    AfterRules = @($coldTCP, $coldUDP)
    NewRelevantEvents = @([pscustomobject]@{ EventID = 2099 })
}
Assert-Throws {
    Assert-D5FirewallUnchanged $eventAudit $policy $emptyOwnershipBaseline
} 'emitted a stable-root firewall rule event'

$chainCold = [pscustomobject]@{
    Phase = 'cold'
    BeforeRecordID = 100
    AfterRecordID = 110
    BeforeRules = @()
    AfterRules = @($coldTCP, $coldUDP)
    NewRelevantEvents = @(
        [pscustomobject]@{ RecordID = 105 }
    )
}
$chainRepeat = [pscustomobject]@{
    Phase = 'repeat'
    BeforeRecordID = 110
    AfterRecordID = 120
    BeforeRules = @($coldTCP, $coldUDP)
    AfterRules = @($coldTCP, $coldUDP)
    NewRelevantEvents = @()
}
Assert-D5FirewallAuditChain @($chainCold, $chainRepeat) 100 @()

$interphaseGrowth = [pscustomobject]@{
    Phase = 'repeat'
    BeforeRecordID = 111
    AfterRecordID = 120
    BeforeRules = @($coldTCP, $coldUDP, $unselectedTCP, $unselectedUDP)
    AfterRules = @($coldTCP, $coldUDP, $unselectedTCP, $unselectedUDP)
    NewRelevantEvents = @()
}
Assert-Throws {
    Assert-D5FirewallAuditChain @($chainCold, $interphaseGrowth) 100 @()
} 'cursor is discontinuous|rule baseline is discontinuous'

$registrationState = New-D5FirewallRegistrationState `
    @($coldTCP, $coldUDP) `
    $policy `
    $emptyOwnershipBaseline
Assert-D5FirewallRegistrationState `
    $registrationState `
    @($coldTCP, $coldUDP) `
    $policy `
    $emptyOwnershipBaseline
$pending = @(Get-D5PendingRegistrationPrograms $registrationState @($program, $unselectedProgram))
if ($pending.Count -ne 1 -or $pending[0] -ne $unselectedProgram) {
    throw "Pending registration programs = $($pending -join ', ')"
}
Assert-Throws {
    Assert-D5FirewallRegistrationState `
        $registrationState `
        @() `
        $policy `
        $emptyOwnershipBaseline
} 'changed before launch'

$emptyState = New-D5FirewallRegistrationState @() $policy $emptyOwnershipBaseline
$noRegistration = Complete-D5FirewallRegistrationAttempt `
    $emptyState `
    @() `
    @($program) `
    $policy `
    $emptyOwnershipBaseline
Assert-D5FirewallRegistrationState $noRegistration @() $policy $emptyOwnershipBaseline
Assert-Throws {
    Assert-D5FirewallRegistrationState `
        $noRegistration `
        @($coldTCP, $coldUDP) `
        $policy `
        $emptyOwnershipBaseline
} 'changed before launch'

$pairRegistration = Complete-D5FirewallRegistrationAttempt `
    $emptyState `
    @($coldTCP, $coldUDP) `
    @($program) `
    $policy `
    $emptyOwnershipBaseline
Assert-D5FirewallRegistrationState `
    $pairRegistration `
    @($coldTCP, $coldUDP) `
    $policy `
    $emptyOwnershipBaseline

# The Entries cores must carry the whole determinism contract without any
# ownership baseline (the lean NetworkTests flow calls them directly) and must
# record dispositions identically to their preflighted wrappers.
$coreEntriesState = New-D5FirewallRegistrationEntries @($coldTCP, $coldUDP) $policy
Assert-D5FirewallRegistrationEntries $coreEntriesState @($coldTCP, $coldUDP) $policy
$tombstoneEntriesState = New-D5FirewallRegistrationEntries `
    @($coldTCP, $coldUDP, $retiredTCP, $retiredUDP) `
    $policy
Assert-D5FirewallRegistrationEntries `
    $tombstoneEntriesState `
    @($coldTCP, $coldUDP, $retiredTCP, $retiredUDP) `
    $policy
if (($tombstoneEntriesState | ConvertTo-Json -Depth 8 -Compress) -cne
    ($coreEntriesState | ConvertTo-Json -Depth 8 -Compress)) {
    throw 'Retired tombstone leaked into the active firewall registration state'
}
if (($coreEntriesState | ConvertTo-Json -Depth 8 -Compress) -cne
    ($registrationState | ConvertTo-Json -Depth 8 -Compress)) {
    throw 'New-D5FirewallRegistrationEntries differs from its preflighted wrapper'
}
Assert-Throws {
    Assert-D5FirewallRegistrationEntries $coreEntriesState @() $policy
} 'changed before launch'
$emptyEntriesState = New-D5FirewallRegistrationEntries @() $policy
Assert-Throws {
    Assert-D5FirewallRegistrationEntries $emptyEntriesState @($coldTCP, $coldUDP) $policy
} 'unrecorded stable program'
$coreNoRegistration = Complete-D5FirewallRegistrationEntries `
    $emptyEntriesState `
    @() `
    @($program) `
    $policy
if (($coreNoRegistration | ConvertTo-Json -Depth 8 -Compress) -cne
    ($noRegistration | ConvertTo-Json -Depth 8 -Compress)) {
    throw 'Complete-D5FirewallRegistrationEntries NoRegistration differs from its wrapper'
}
$corePairRegistration = Complete-D5FirewallRegistrationEntries `
    $emptyEntriesState `
    @($coldTCP, $coldUDP) `
    @($program) `
    $policy
if (($corePairRegistration | ConvertTo-Json -Depth 8 -Compress) -cne
    ($pairRegistration | ConvertTo-Json -Depth 8 -Compress)) {
    throw 'Complete-D5FirewallRegistrationEntries RegisteredPair differs from its wrapper'
}

# The Entries cores must reject silently-minted wrong-shape pairs (e.g. the
# Private-only rules Windows creates with consent popups suppressed) instead
# of recording them for the audited modes to trip over later.
$privateOnlyTCP = New-Rule @{
    InstanceID = "TCP Query User{$guid}$program"
    RuleID = "TCP Query User{$guid}$program"
    Program = $program
    Protocol = 'TCP'
    Profile = @('Private')
}
$privateOnlyUDP = New-Rule @{
    InstanceID = "UDP Query User{$guid}$program"
    RuleID = "UDP Query User{$guid}$program"
    Program = $program
    Protocol = 'UDP'
    Profile = @('Private')
}
Assert-Throws {
    New-D5FirewallRegistrationEntries @($privateOnlyTCP, $privateOnlyUDP) $policy
} 'exact bounded Query User shape'
Assert-Throws {
    Assert-D5FirewallRegistrationEntries `
        $coreEntriesState `
        @($privateOnlyTCP, $privateOnlyUDP) `
        $policy
} 'exact bounded Query User shape'
Assert-Throws {
    Complete-D5FirewallRegistrationEntries `
        $emptyEntriesState `
        @($privateOnlyTCP, $privateOnlyUDP) `
        @($program) `
        $policy
} 'exact bounded Query User shape'
Assert-Throws {
    New-D5FirewallRegistrationEntries @($coldTCP) $policy
} 'exactly one TCP rule and one UDP rule'

$excludedProgram = 'C:\Temp\go-build900\b001\exe\justus-go.exe'
$excludedTCP = New-D5ExcludedEvidenceRule `
    $excludedProgram `
    'TCP' `
    '11111111-1111-1111-1111-111111111111'
$excludedUDP = New-D5ExcludedEvidenceRule `
    $excludedProgram `
    'UDP' `
    '22222222-2222-2222-2222-222222222222'
foreach ($rule in @($excludedTCP, $excludedUDP)) {
    $rule | Add-Member -NotePropertyName ProgramExists -NotePropertyValue $false
    $rule | Add-Member -NotePropertyName ProgramSHA256 -NotePropertyValue ''
}
$d5Hash = 'a' * 64
$excludedEvidence = New-TestEvidence `
    @($excludedTCP, $excludedUDP) `
    @([pscustomobject]@{ Program = $excludedProgram; Exists = $false; SHA256 = '' }) `
    @($d5Hash) `
    @('(?i)[\\/](?:windshare-c5-[^\\/]+)[\\/]') `
    @('C:\Temp\go-build777')
$excludedPolicy = New-D5FirewallOwnershipPolicy `
    $harnessRoot `
    @($program, $unselectedProgram) `
    $excludedEvidence `
    @() `
    @('C:\Temp')
$preservedRules = @($excludedTCP, $excludedUDP, $broadHarnessRule, $broadHarnessUDP)
$excludedBaseline = New-D5FirewallOwnershipBaseline $preservedRules $excludedPolicy
Assert-D5FirewallPreflight $preservedRules $excludedPolicy $excludedBaseline
if ($excludedBaseline.EvidenceRuleCount -ne 2 -or
    $excludedBaseline.PinnedExcludedRuleCount -ne 2 -or
    $excludedBaseline.ExcludedRuleCount -ne 2 -or
    $excludedBaseline.ExcludedProgramCount -ne 1) {
    throw 'Evidence-pinned excluded baseline did not preserve its exact counts'
}
$subsetBaseline = New-D5FirewallOwnershipBaseline @($excludedTCP) $excludedPolicy
if ($subsetBaseline.PinnedExcludedRuleCount -ne 1 -or
    $subsetBaseline.ExcludedRuleCount -ne 1) {
    throw 'A pre-existing evidence-pinned subset was not recorded as its own baseline'
}

$spoofedPinnedTCP = $excludedTCP.PSObject.Copy()
$spoofedPinnedUDP = $excludedUDP.PSObject.Copy()
foreach ($rule in @($spoofedPinnedTCP, $spoofedPinnedUDP)) {
    $rule.ProgramExists = $true
    $rule.ProgramSHA256 = $d5Hash
}
[void](New-D5FirewallOwnershipBaseline `
    @($spoofedPinnedTCP, $spoofedPinnedUDP) `
    $excludedPolicy)

$spoofedHashRule = New-Rule @{
    RuleID = 'spoofed-hash'
    InstanceID = 'spoofed-hash'
    Program = 'C:\Temp\go-build901\b001\exe\justus-go.exe'
    ProgramExists = $true
    ProgramSHA256 = $d5Hash
}
[void](New-D5FirewallOwnershipBaseline `
    @($excludedTCP, $excludedUDP, $spoofedHashRule) `
    $excludedPolicy)

$spoofedPathRule = New-Rule @{
    RuleID = 'spoofed-path'
    InstanceID = 'spoofed-path'
    Program = 'C:\Temp\go-build777\b001\exe\justus-go.exe'
}
[void](New-D5FirewallOwnershipBaseline `
    @($excludedTCP, $excludedUDP, $spoofedPathRule) `
    $excludedPolicy)

$mixedPinnedUDP = $excludedUDP.PSObject.Copy()
$mixedPinnedUDP.Action = 'Block'
$mixedExternalBaseline = New-D5FirewallOwnershipBaseline `
    @($excludedTCP, $mixedPinnedUDP) `
    $excludedPolicy
if ($mixedExternalBaseline.PinnedExcludedRuleCount -ne 1 -or
    $mixedExternalBaseline.UnrelatedExcludedRuleCount -ne 1 -or
    $mixedExternalBaseline.ExcludedProgramCount -ne 1) {
    throw 'Mixed historical/drifted external rules were not retained as overlapping telemetry'
}

$unrelatedRule = New-Rule @{
    RuleID = 'unrelated-baseline'
    InstanceID = 'unrelated-baseline'
    Program = 'C:\Temp\go-build902\b001\exe\other-tool.exe'
}
$unrelatedRules = @($excludedTCP, $excludedUDP, $unrelatedRule)
$unrelatedBaseline = New-D5FirewallOwnershipBaseline $unrelatedRules $excludedPolicy
$mutatedUnrelatedRule = $unrelatedRule.PSObject.Copy()
$mutatedUnrelatedRule.Action = 'Allow'
$unrelatedMutation = [pscustomobject]@{
    BeforeRules = $unrelatedRules
    AfterRules = @($excludedTCP, $excludedUDP, $mutatedUnrelatedRule)
    NewRelevantEvents = @()
}
Assert-D5FirewallUnchanged $unrelatedMutation $excludedPolicy $unrelatedBaseline
Assert-D5FirewallOwnershipBaseline `
    $unrelatedBaseline `
    @($excludedTCP, $excludedUDP, $mutatedUnrelatedRule) `
    $excludedPolicy `
    'External semantic telemetry'
$mutationTelemetry = Assert-D5ActiveStoreFirewallTransition `
    $unrelatedRules `
    $unrelatedRules `
    @($excludedTCP, $excludedUDP, $mutatedUnrelatedRule) `
    $harnessRoot `
    $null `
    $false `
    @() `
    'External semantic telemetry'
if ($mutationTelemetry.ExternalDebt.ChangedRuleCount -ne 1) {
    throw 'Bounded external semantic drift was not recorded as an exact delta'
}

$concurrentGrowthRule = New-Rule @{
    RuleID = 'concurrent-growth'
    InstanceID = 'concurrent-growth'
    Program = 'C:\Temp\go-build903\b001\exe\other-tool.exe'
}
$concurrentGrowth = [pscustomobject]@{
    BeforeRules = $unrelatedRules
    AfterRules = @($unrelatedRules + @($concurrentGrowthRule))
    NewRelevantEvents = @([pscustomobject]@{
        EventID = 2097
        Fields = [pscustomobject]@{
            ApplicationPath = $concurrentGrowthRule.Program
        }
    })
}
Assert-D5FirewallUnchanged $concurrentGrowth $excludedPolicy $unrelatedBaseline
Assert-D5FirewallOwnershipBaseline `
    $unrelatedBaseline `
    @($unrelatedRules + @($concurrentGrowthRule)) `
    $excludedPolicy `
    'Bounded external growth'
$growthTelemetry = Assert-D5ActiveStoreFirewallTransition `
    $unrelatedRules `
    $unrelatedRules `
    @($unrelatedRules + @($concurrentGrowthRule)) `
    $harnessRoot `
    $null `
    $false `
    @() `
    'Bounded external growth'
if ($growthTelemetry.ExternalDebt.AddedRuleCount -ne 1 -or
    $growthTelemetry.ExternalDebt.NetRuleGrowth -ne 1) {
    throw 'Bounded external growth was not recorded as warning telemetry'
}

$d5TemporaryRule = New-Rule @{
    RuleID = 'd5-temp'
    InstanceID = 'd5-temp'
    Program = 'C:\Temp\go-build904\b001\exe\webrtc.test.exe'
}
[void](New-D5FirewallOwnershipBaseline `
    @($excludedTCP, $excludedUDP, $d5TemporaryRule) `
    $excludedPolicy)
if (-not (Test-D5ExternalStableProgramNameRule $d5TemporaryRule $excludedPolicy)) {
    throw 'External stable-name collision was not retained as informational evidence'
}

$historyAbsentEvidence = New-TestEvidence `
    @($excludedTCP, $excludedUDP) `
    @([pscustomobject]@{ Program = $excludedProgram; Exists = $false; SHA256 = '' }) `
    @() `
    @() `
    @() `
    $false
$historyAbsentPolicy = New-D5FirewallOwnershipPolicy `
    $harnessRoot `
    @($program, $unselectedProgram) `
    $historyAbsentEvidence `
    @() `
    @('C:\Temp')
[void](New-D5FirewallOwnershipBaseline @($excludedTCP) $historyAbsentPolicy)
[void](New-D5FirewallOwnershipBaseline @($d5TemporaryRule) $historyAbsentPolicy)
[void](New-D5FirewallOwnershipBaseline @($unrelatedRule) $historyAbsentPolicy)

$importRoot = Join-Path ([IO.Path]::GetTempPath()) `
    "windshare-d5-ownership-import-$([guid]::NewGuid().ToString('N'))"
try {
    New-Item -ItemType Directory -Force -Path (Join-Path $importRoot 'scripts') | Out-Null
    Copy-Item `
        -LiteralPath (Join-Path $PSScriptRoot 'd5-windows-network-packages.json') `
        -Destination (Join-Path $importRoot 'scripts\d5-windows-network-packages.json')
    $syntheticManifestPath = Join-Path $importRoot 'scripts\d5-windows-firewall-ownership.json'
    Copy-Item `
        -LiteralPath (Join-Path $PSScriptRoot 'd5-windows-firewall-ownership.json') `
        -Destination $syntheticManifestPath
    $absentHistory = Import-D5FirewallOwnershipEvidence $importRoot $syntheticManifestPath
    if ($absentHistory.CleanupHistoryPresent -or
        $absentHistory.CleanupOwnedRuleCount -ne 0 -or
        @($absentHistory.CleanupOwnedProgramPaths).Count -ne 0 -or
        $absentHistory.ExcludedRuleCount -ne 60) {
        throw 'Import without the cleanup history corpus must yield empty cleanup-owned evidence'
    }
    $partialDir = Join-Path $importRoot 'docs\.orchestration'
    New-Item -ItemType Directory -Force -Path $partialDir | Out-Null
    $partialPath = Join-Path $partialDir 'firewall-cleanup-result.md'
    Set-Content -LiteralPath $partialPath -Value 'partial corpus fixture' -Encoding utf8
    $partialManifest = Get-Content -LiteralPath $syntheticManifestPath -Raw | ConvertFrom-Json
    foreach ($source in @($partialManifest.Sources)) {
        if ([string]$source.RelativePath -ceq 'docs/.orchestration/firewall-cleanup-result.md') {
            $source.SHA256 = (
                Get-FileHash -LiteralPath $partialPath -Algorithm SHA256
            ).Hash.ToLowerInvariant()
        }
    }
    $partialManifestPath = Join-Path $importRoot 'scripts\partial-ownership.json'
    $partialManifest | ConvertTo-Json -Depth 8 |
        Set-Content -LiteralPath $partialManifestPath -Encoding utf8
    Assert-Throws {
        Import-D5FirewallOwnershipEvidence $importRoot $partialManifestPath
    } 'partially present'
} finally {
    Remove-Item -LiteralPath $importRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output 'D5 firewall semantic policy tests PASS'
