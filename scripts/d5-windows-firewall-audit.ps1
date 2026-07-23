Set-StrictMode -Version Latest

$script:D5FirewallRegistrationSchemaVersion = 2
$script:D5PlaywrightFirewallDebtMaxNewPairsPerRun = 1
$script:D5PlaywrightFirewallDebtMaxRetainedRevisionsPerEngine = 2
# Host-owned rules can legitimately churn during a run, but two fresh TCP/UDP
# pairs is already the largest benign burst worth tolerating. The total caps sit
# well above the audited workstation baseline while still bounding repeated
# Playwright/Go-build identity leakage without granting either path ownership.
$script:D5ExternalFirewallDebtMaxNetGrowthPerRun = 4
$script:D5ExternalFirewallDebtMaxTotalRules = 1024
$script:D5GoBuildFirewallDebtMaxTotalRules = 128

$script:D5FirewallSemanticFields = @(
    'RuleID',
    'InstanceID',
    'Program',
    'Direction',
    'Action',
    'Profile',
    'Enabled',
    'PolicyStoreSourceType',
    'Protocol',
    'LocalPort',
    'RemotePort',
    'LocalAddress',
    'RemoteAddress'
)

function ConvertTo-D5SemanticValue([object]$Value) {
    if ($null -eq $Value) {
        return ''
    }
    return @($Value | ForEach-Object { [string]$_ } | Sort-Object -Unique) -join ','
}

function ConvertTo-D5RuleSignature([object]$Rule) {
    $signature = [ordered]@{}
    foreach ($field in $script:D5FirewallSemanticFields) {
        $signature[$field] = ConvertTo-D5SemanticValue $Rule.$field
    }
    return $signature | ConvertTo-Json -Compress
}

function ConvertTo-D5ProtocolName([object]$Protocol) {
    $value = ConvertTo-D5SemanticValue $Protocol
    switch -Regex ($value) {
        '^(?i:TCP|6)$' { return 'TCP' }
        '^(?i:UDP|17)$' { return 'UDP' }
        default { return $value.ToUpperInvariant() }
    }
}

function New-D5ProgramSet([string[]]$Programs) {
    $set = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($program in $Programs) {
        if (-not [string]::IsNullOrWhiteSpace($program)) {
            [void]$set.Add([IO.Path]::GetFullPath($program))
        }
    }
    return ,$set
}

function Test-D5RuleSetsEqual([object[]]$Expected, [object[]]$Actual) {
    $expectedSignatures = @($Expected | ForEach-Object { ConvertTo-D5RuleSignature $_ } | Sort-Object)
    $actualSignatures = @($Actual | ForEach-Object { ConvertTo-D5RuleSignature $_ } | Sort-Object)
    return $expectedSignatures.Count -eq $actualSignatures.Count -and
        @(Compare-Object -ReferenceObject $expectedSignatures -DifferenceObject $actualSignatures).Count -eq 0
}

function Assert-D5ExactProtocolPairs([hashtable]$ProtocolsByProgram, [string]$Phase) {
    foreach ($program in $ProtocolsByProgram.Keys) {
        $protocols = @($ProtocolsByProgram[$program] | Sort-Object)
        if ($protocols.Count -ne 2 -or
            @(Compare-Object -ReferenceObject @('TCP', 'UDP') -DifferenceObject $protocols).Count -ne 0) {
            throw "$Phase requires exactly one TCP rule and one UDP rule for $program; found $($protocols -join ', ')"
        }
    }
}

function Test-D5ProgramUnderRoot([string]$Program, [string]$Root) {
    if ([string]::IsNullOrWhiteSpace($Program)) {
        return $false
    }
    try {
        $candidate = $Program.Trim()
        $isDevicePath = $false
        foreach ($prefix in @('\\?\', '\\.\', '\Device\', '\??\')) {
            if ($candidate.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
                $isDevicePath = $true
                break
            }
        }
        if ($candidate -cne $Program -or
            $candidate.Contains('%') -or
            $candidate.Contains('::') -or
            $candidate -match '(?i)^(?:\$\{?env:|[a-z][a-z0-9+.-]*://)' -or
            $isDevicePath) {
            return $false
        }
        if (-not [IO.Path]::IsPathFullyQualified($candidate) -or
            $candidate.IndexOfAny([char[]]'*?') -ge 0 -or
            $candidate.IndexOfAny([IO.Path]::GetInvalidPathChars()) -ge 0) {
            return $false
        }
        $fullProgram = [IO.Path]::GetFullPath($candidate)
    } catch {
        # Host firewall tokens and malformed paths are external observations,
        # never aliases for the runner's current directory.
        return $false
    }
    $fullRoot = [IO.Path]::GetFullPath($Root).TrimEnd(
        [IO.Path]::DirectorySeparatorChar,
        [IO.Path]::AltDirectorySeparatorChar
    )
    return $fullProgram.StartsWith(
        $fullRoot + [IO.Path]::DirectorySeparatorChar,
        [StringComparison]::OrdinalIgnoreCase
    )
}

. (Join-Path $PSScriptRoot 'd5-windows-firewall-ownership.ps1')

function Test-D5BroadInboundAllow([object]$Rule) {
    $profile = ConvertTo-D5SemanticValue $Rule.Profile
    $publicOrAny = @(
        @($profile -split ',') | ForEach-Object { $_.Trim() } | Where-Object {
            $_ -eq 'Public' -or $_ -eq 'Any'
        }
    )
    return (ConvertTo-D5SemanticValue $Rule.Enabled) -eq 'True' -and
        (ConvertTo-D5SemanticValue $Rule.Direction) -eq 'Inbound' -and
        (ConvertTo-D5SemanticValue $Rule.Action) -eq 'Allow' -and
        $publicOrAny.Count -gt 0 -and
        (ConvertTo-D5SemanticValue $Rule.LocalPort) -eq 'Any' -and
        (ConvertTo-D5SemanticValue $Rule.RemotePort) -eq 'Any' -and
        (ConvertTo-D5SemanticValue $Rule.LocalAddress) -eq 'Any' -and
        (ConvertTo-D5SemanticValue $Rule.RemoteAddress) -eq 'Any'
}

function Assert-D5FirewallPreflight(
    [object[]]$Rules,
    [object]$OwnershipPolicy,
    [object]$OwnershipBaseline
) {
    if ($null -ne $OwnershipBaseline) {
        Assert-D5FirewallOwnershipBaseline `
            $OwnershipBaseline `
            $Rules `
            $OwnershipPolicy `
            'Preflight'
    }
    $snapshot = New-D5FirewallOwnershipSnapshot $Rules $OwnershipPolicy
    $identities = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $protocolsByProgram = @{}
    foreach ($rule in @($snapshot.StableRules)) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        Assert-D5FixedRule $rule $OwnershipPolicy.AuthorizedProgramSet 'Preflight'
        $identity = Get-D5RuleIdentity $rule
        if (-not $identities.Add($identity)) {
            throw "Preflight found a duplicate stable firewall identity: $identity"
        }
        if (-not $protocolsByProgram.ContainsKey($program)) {
            $protocolsByProgram[$program] = [Collections.Generic.HashSet[string]]::new(
                [StringComparer]::OrdinalIgnoreCase
            )
        }
        if (-not $protocolsByProgram[$program].Add((ConvertTo-D5ProtocolName $rule.Protocol))) {
            throw "Preflight found a duplicate protocol identity for $program"
        }
    }
    Assert-D5ExactProtocolPairs $protocolsByProgram 'Preflight'
}

function Get-D5RuleIdentity([object]$Rule) {
    return @(
        ConvertTo-D5SemanticValue $Rule.RuleID
        ConvertTo-D5SemanticValue $Rule.InstanceID
        ConvertTo-D5SemanticValue $Rule.Program
        ConvertTo-D5SemanticValue $Rule.Protocol
    ) -join '|'
}

function Resolve-D5PlaywrightFirewallProgram(
    [string]$Program,
    [string]$CacheRoot
) {
    if ([string]::IsNullOrWhiteSpace($Program) -or
        -not (Test-D5ProgramUnderRoot $Program $CacheRoot)) {
        return $null
    }
    $fullProgram = [IO.Path]::GetFullPath(
        [Environment]::ExpandEnvironmentVariables($Program)
    )
    $relative = [IO.Path]::GetRelativePath($CacheRoot, $fullProgram).Replace('/', '\')
    $patterns = [ordered]@{
        chromium = '^chromium_headless_shell-(?<revision>[0-9]+)\\chrome-headless-shell-win64\\chrome-headless-shell\.exe$'
        firefox = '^firefox-(?<revision>[0-9]+)\\firefox\\firefox\.exe$'
        webkit = '^webkit-(?<revision>[0-9]+)\\(?:Playwright|WebKitNetworkProcess|WebKitWebProcess)\.exe$'
    }
    foreach ($browserName in $patterns.Keys) {
        if ($relative -match [string]$patterns[$browserName]) {
            return [pscustomobject][ordered]@{
                BrowserName = $browserName
                Revision = [string]$Matches.revision
                RevisionKey = "$browserName/$($Matches.revision)"
                Program = $fullProgram
            }
        }
    }
    return $null
}

function New-D5PlaywrightFirewallDebtPolicy(
    [string]$CacheRoot,
    [object[]]$BrowserManifest
) {
    $root = [IO.Path]::GetFullPath($CacheRoot).TrimEnd(
        [IO.Path]::DirectorySeparatorChar,
        [IO.Path]::AltDirectorySeparatorChar
    )
    $installedPrograms = New-D5ProgramSet @()
    $installed = [Collections.Generic.List[object]]::new()
    foreach ($browser in @($BrowserManifest)) {
        if (-not [IO.Path]::GetFullPath([string]$browser.cacheRoot).Equals(
            $root,
            [StringComparison]::OrdinalIgnoreCase
        )) {
            throw 'Playwright firewall manifest spans more than one cache root'
        }
        foreach ($candidate in @($browser.networkExecutables)) {
            $program = [IO.Path]::GetFullPath([string]$candidate)
            $descriptor = Resolve-D5PlaywrightFirewallProgram $program $root
            if ($null -eq $descriptor -or
                [string]$descriptor.BrowserName -cne [string]$browser.browserName) {
                throw "Playwright firewall manifest has a non-canonical network executable: $program"
            }
            if (-not (Test-Path -LiteralPath $program -PathType Leaf)) {
                throw "Playwright firewall manifest executable is missing: $program"
            }
            if (-not $installedPrograms.Add($program)) {
                throw "Playwright firewall manifest repeats network executable: $program"
            }
            $installed.Add($descriptor)
        }
    }
    return [pscustomobject][ordered]@{
        CacheRoot = $root
        InstalledPrograms = $installedPrograms
        InstalledBrowsers = @($installed)
        MaxNewPairsPerRun = $script:D5PlaywrightFirewallDebtMaxNewPairsPerRun
        MaxRetainedRevisionsPerEngine = $script:D5PlaywrightFirewallDebtMaxRetainedRevisionsPerEngine
    }
}

function Assert-D5PlaywrightFirewallRulePair(
    [object[]]$Rules,
    [object]$Descriptor,
    [string]$Phase
) {
    if (@($Rules).Count -ne 2) {
        throw "$Phase requires exactly one TCP/UDP Playwright firewall pair for $($Descriptor.Program)"
    }
    $protocols = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase
    )
    $pairSemantics = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::Ordinal
    )
    foreach ($rule in @($Rules)) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        if (-not $program.Equals(
            [string]$Descriptor.Program,
            [StringComparison]::OrdinalIgnoreCase
        )) {
            throw "$Phase mixed Playwright firewall program identities"
        }
        $instanceID = ConvertTo-D5SemanticValue $rule.InstanceID
        $identityPattern = '(?i)^(TCP|UDP) Query User\{' +
            '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}' +
            [regex]::Escape($program) + '$'
        if ($instanceID -notmatch $identityPattern) {
            throw "$Phase found a non-Query-User Playwright firewall rule: $instanceID"
        }
        $identityProtocol = $Matches[1].ToUpperInvariant()
        $protocol = ConvertTo-D5ProtocolName $rule.Protocol
        $profiles = @(
            foreach ($value in @($rule.Profile)) {
                @([string]$value -split ',') |
                    ForEach-Object { $_.Trim() } |
                    Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
            }
        )
        $uniqueProfiles = @($profiles | Sort-Object -Unique)
        $profileSignature = $uniqueProfiles -join ','
        if ($profiles.Count -ne $uniqueProfiles.Count -or
            $profileSignature -notin @('Private', 'Private,Public')) {
            throw "$Phase found duplicate or unsupported Playwright firewall profiles"
        }
        $semantics = [ordered]@{
            Direction = ConvertTo-D5SemanticValue $rule.Direction
            Action = ConvertTo-D5SemanticValue $rule.Action
            Profile = $profileSignature
            Enabled = ConvertTo-D5SemanticValue $rule.Enabled
            PolicyStoreSourceType = ConvertTo-D5SemanticValue $rule.PolicyStoreSourceType
            LocalPort = ConvertTo-D5SemanticValue $rule.LocalPort
            RemotePort = ConvertTo-D5SemanticValue $rule.RemotePort
            LocalAddress = ConvertTo-D5SemanticValue $rule.LocalAddress
            RemoteAddress = ConvertTo-D5SemanticValue $rule.RemoteAddress
        }
        if ((ConvertTo-D5SemanticValue $rule.RuleID) -ne $instanceID -or
            $protocol -ne $identityProtocol -or
            -not $protocols.Add($protocol) -or
            $semantics.Enabled -ne 'True' -or
            $semantics.Direction -ne 'Inbound' -or
            $semantics.Action -notin @('Allow', 'Block') -or
            $semantics.PolicyStoreSourceType -ne 'Local' -or
            $semantics.LocalPort -ne 'Any' -or
            $semantics.RemotePort -ne 'Any' -or
            $semantics.LocalAddress -ne 'Any' -or
            $semantics.RemoteAddress -ne 'Any') {
            throw "$Phase found altered Playwright firewall semantics: $(ConvertTo-D5RuleSignature $rule)"
        }
        [void]$pairSemantics.Add(($semantics | ConvertTo-Json -Compress))
    }
    if ($protocols.Count -ne 2 -or
        -not $protocols.Contains('TCP') -or
        -not $protocols.Contains('UDP')) {
        throw "$Phase requires exactly one TCP rule and one UDP rule for $($Descriptor.Program)"
    }
    if ($pairSemantics.Count -ne 1) {
        throw "$Phase found mismatched TCP/UDP Playwright firewall semantics"
    }
}

function Get-D5PlaywrightFirewallDebtSnapshot(
    [object[]]$Rules,
    [object]$Policy,
    [string]$Phase
) {
    $rulesByProgram = @{}
    $issues = [Collections.Generic.List[object]]::new()
    $observedRules = [Collections.Generic.List[object]]::new()
    $observedRuleCount = 0
    foreach ($rule in @($Rules)) {
        $program = [string]$rule.Program
        if ([string]::IsNullOrWhiteSpace($program) -or
            -not (Test-D5ProgramUnderRoot $program ([string]$Policy.CacheRoot))) {
            continue
        }
        $observedRuleCount++
        $observedRules.Add($rule)
        $fullProgram = [IO.Path]::GetFullPath($program)
        $descriptor = Resolve-D5PlaywrightFirewallProgram $fullProgram $Policy.CacheRoot
        if ($null -eq $descriptor) {
            $issues.Add([pscustomobject][ordered]@{
                Program = $fullProgram
                Problem = 'non-canonical executable under the Playwright cache'
            })
            continue
        }
        if (-not $rulesByProgram.ContainsKey($fullProgram)) {
            $rulesByProgram[$fullProgram] = [pscustomobject]@{
                Descriptor = $descriptor
                Rules = [Collections.Generic.List[object]]::new()
            }
        }
        $rulesByProgram[$fullProgram].Rules.Add($rule)
    }

    $revisionPrograms = @{}
    $revisionsByEngine = @{}
    $entries = [Collections.Generic.List[object]]::new()
    foreach ($program in @($rulesByProgram.Keys | Sort-Object)) {
        $group = $rulesByProgram[$program]
        $descriptor = $group.Descriptor
        $boundedPair = $true
        try {
            Assert-D5PlaywrightFirewallRulePair @($group.Rules) $descriptor $Phase
        } catch {
            $boundedPair = $false
            $issues.Add([pscustomobject][ordered]@{
                Program = $program
                Problem = [string]$_
            })
        }
        $revisionKey = [string]$descriptor.RevisionKey
        if ($revisionPrograms.ContainsKey($revisionKey)) {
            $boundedPair = $false
            $issues.Add([pscustomobject][ordered]@{
                Program = $program
                Problem = "more than one Playwright firewall program for revision $revisionKey"
            })
        } else {
            $revisionPrograms[$revisionKey] = $program
        }
        $browserName = [string]$descriptor.BrowserName
        if (-not $revisionsByEngine.ContainsKey($browserName)) {
            $revisionsByEngine[$browserName] = [Collections.Generic.HashSet[string]]::new(
                [StringComparer]::Ordinal
            )
        }
        [void]$revisionsByEngine[$browserName].Add([string]$descriptor.Revision)
        $entries.Add([pscustomobject][ordered]@{
            BrowserName = $browserName
            Revision = [string]$descriptor.Revision
            Program = $program
            Installed = $Policy.InstalledPrograms.Contains($program)
            BoundedPair = $boundedPair
            RuleCount = @($group.Rules).Count
            RuleSignatures = @(
                $group.Rules |
                    ForEach-Object { ConvertTo-D5RuleSignature $_ } |
                    Sort-Object
            )
        })
    }
    $violations = [Collections.Generic.List[string]]::new()
    foreach ($browserName in $revisionsByEngine.Keys) {
        if ($revisionsByEngine[$browserName].Count -gt
            [int]$Policy.MaxRetainedRevisionsPerEngine) {
            $message = "exceeded the retained Playwright firewall revision cap for $browserName"
            $violations.Add($message)
        }
    }
    return [pscustomobject][ordered]@{
        Summary = New-D5ActiveStoreFirewallSummary @($observedRules)
        PairCount = @($entries | Where-Object BoundedPair).Count
        ObservedRuleCount = $observedRuleCount
        Entries = @($entries)
        Issues = @($issues)
        Limits = [pscustomobject][ordered]@{
            MaxNewPairsPerRun = [int]$Policy.MaxNewPairsPerRun
            MaxRetainedRevisionsPerEngine = [int]$Policy.MaxRetainedRevisionsPerEngine
        }
        Violations = @($violations)
        CleanupAdvisory = (
            'Playwright Query User pairs are external toolchain debt. Review stale revisions ' +
            'with the host administrator; WindShare records but never creates or deletes them.'
        )
    }
}

function Assert-D5PlaywrightFirewallDebtSnapshot([object]$Snapshot, [string]$Phase) {
    if (@($Snapshot.Violations).Count -gt 0) {
        throw "$Phase $(@($Snapshot.Violations) -join '; ')"
    }
}

function New-D5ActiveStoreFirewallSummary([object[]]$Rules) {
    $signatures = @(
        $Rules |
            ForEach-Object { ConvertTo-D5RuleSignature $_ } |
            Sort-Object
    )
    return [pscustomobject][ordered]@{
        RuleCount = $signatures.Count
        SemanticPayloadSHA256 = Get-D5OrdinalPayloadSHA256 $signatures
    }
}

function Get-D5ExternalFirewallRuleClass([object]$Rule, [object]$PlaywrightPolicy) {
    $program = [string]$Rule.Program
    if ($null -ne $PlaywrightPolicy -and
        -not [string]::IsNullOrWhiteSpace($program) -and
        (Test-D5ProgramUnderRoot $program ([string]$PlaywrightPolicy.CacheRoot))) {
        return 'PlaywrightCache'
    }
    if ($program -match '(?i)(?:^|[\\/])go-build(?:[0-9]+)?(?:[\\/]|$)') {
        return 'GoBuildDiagnostic'
    }
    return 'Other'
}

function New-D5ExternalFirewallDebtSnapshot(
    [object[]]$Rules,
    [string]$StableRoot,
    [object]$PlaywrightPolicy
) {
    $external = @(
        $Rules | Where-Object {
            -not (Test-D5ProgramUnderRoot ([string]$_.Program) $StableRoot)
        }
    )
    $classes = @(
        foreach ($name in @('PlaywrightCache', 'GoBuildDiagnostic', 'Other')) {
            $classRules = @($external | Where-Object {
                (Get-D5ExternalFirewallRuleClass $_ $PlaywrightPolicy) -eq $name
            })
            [pscustomobject][ordered]@{
                Name = $name
                RuleCount = $classRules.Count
                ProgramCount = @(
                    $classRules | ForEach-Object { [string]$_.Program } | Sort-Object -Unique
                ).Count
                Programs = @(
                    $classRules | ForEach-Object { [string]$_.Program } | Sort-Object -Unique
                )
            }
        }
    )
    $violations = [Collections.Generic.List[string]]::new()
    if ($external.Count -gt $script:D5ExternalFirewallDebtMaxTotalRules) {
        $violations.Add('exceeded the total external firewall debt cap')
    }
    $goBuild = @($classes | Where-Object Name -eq 'GoBuildDiagnostic')[0]
    if ([int]$goBuild.RuleCount -gt $script:D5GoBuildFirewallDebtMaxTotalRules) {
        $violations.Add('exceeded the total Go-build firewall debt cap')
    }
    return [pscustomobject][ordered]@{
        Summary = New-D5ActiveStoreFirewallSummary $external
        Classes = $classes
        Entries = @(
            $external |
                ForEach-Object {
                    New-D5ExternalFirewallDeltaEntry $_ $PlaywrightPolicy
                } |
                Sort-Object Program, RuleID
        )
        Limits = [pscustomobject][ordered]@{
            MaxNetGrowthPerRun = $script:D5ExternalFirewallDebtMaxNetGrowthPerRun
            MaxTotalRules = $script:D5ExternalFirewallDebtMaxTotalRules
            MaxGoBuildRules = $script:D5GoBuildFirewallDebtMaxTotalRules
        }
        Violations = @($violations)
        CleanupAdvisory = (
            'External firewall rules are host-owned telemetry, not WindShare launch authority. ' +
            'Review exact stale identities with the host administrator; the runner never creates or deletes rules.'
        )
    }
}

function Assert-D5ExternalFirewallDebtSnapshot([object]$Snapshot, [string]$Phase) {
    if (@($Snapshot.Violations).Count -gt 0) {
        throw "$Phase $(@($Snapshot.Violations) -join '; ')"
    }
}

function New-D5ExternalFirewallDeltaEntry([object]$Rule, [object]$PlaywrightPolicy) {
    return [pscustomobject][ordered]@{
        RuleID = [string]$Rule.RuleID
        InstanceID = [string]$Rule.InstanceID
        Program = [string]$Rule.Program
        Class = Get-D5ExternalFirewallRuleClass $Rule $PlaywrightPolicy
        Signature = ConvertTo-D5RuleSignature $Rule
    }
}

function New-D5FirewallSemanticTransition(
    [object[]]$InitialRules,
    [object[]]$CurrentRules,
    [object]$PlaywrightPolicy,
    [string]$Phase
) {
    $initial = @{}
    foreach ($rule in @($InitialRules)) {
        $identity = Get-D5RuleIdentity $rule
        if ($initial.ContainsKey($identity)) {
            throw "$Phase initial snapshot repeats firewall identity: $identity"
        }
        $initial[$identity] = $rule
    }
    $current = @{}
    foreach ($rule in @($CurrentRules)) {
        $identity = Get-D5RuleIdentity $rule
        if ($current.ContainsKey($identity)) {
            throw "$Phase current snapshot repeats firewall identity: $identity"
        }
        $current[$identity] = $rule
    }
    $added = @(
        foreach ($identity in $current.Keys) {
            if (-not $initial.ContainsKey($identity)) {
                New-D5ExternalFirewallDeltaEntry $current[$identity] $PlaywrightPolicy
            }
        }
    )
    $removed = @(
        foreach ($identity in $initial.Keys) {
            if (-not $current.ContainsKey($identity)) {
                New-D5ExternalFirewallDeltaEntry $initial[$identity] $PlaywrightPolicy
            }
        }
    )
    $changed = @(
        foreach ($identity in $initial.Keys) {
            if ($current.ContainsKey($identity)) {
                $initialSignature = ConvertTo-D5RuleSignature $initial[$identity]
                $currentSignature = ConvertTo-D5RuleSignature $current[$identity]
                if ($initialSignature -cne $currentSignature) {
                    [pscustomobject][ordered]@{
                        RuleID = [string]$current[$identity].RuleID
                        InstanceID = [string]$current[$identity].InstanceID
                        Program = [string]$current[$identity].Program
                        Class = Get-D5ExternalFirewallRuleClass $current[$identity] $PlaywrightPolicy
                        BeforeSignature = $initialSignature
                        AfterSignature = $currentSignature
                    }
                }
            }
        }
    )
    $beforeSummary = New-D5ActiveStoreFirewallSummary $InitialRules
    $afterSummary = New-D5ActiveStoreFirewallSummary $CurrentRules
    return [pscustomobject][ordered]@{
        Before = $beforeSummary
        After = $afterSummary
        NetRuleGrowth = [int]$afterSummary.RuleCount - [int]$beforeSummary.RuleCount
        AddedRuleCount = $added.Count
        RemovedRuleCount = $removed.Count
        ChangedRuleCount = $changed.Count
        Added = @($added | Sort-Object Program, RuleID)
        Removed = @($removed | Sort-Object Program, RuleID)
        Changed = @($changed | Sort-Object Program, RuleID)
    }
}

function New-D5ExternalFirewallTransition(
    [object[]]$InitialRules,
    [object[]]$CurrentRules,
    [string]$StableRoot,
    [object]$PlaywrightPolicy,
    [string]$Phase
) {
    $before = New-D5ExternalFirewallDebtSnapshot `
        $InitialRules $StableRoot $PlaywrightPolicy
    $after = New-D5ExternalFirewallDebtSnapshot `
        $CurrentRules $StableRoot $PlaywrightPolicy
    $initialExternal = @($InitialRules | Where-Object {
        -not (Test-D5ProgramUnderRoot ([string]$_.Program) $StableRoot)
    })
    $currentExternal = @($CurrentRules | Where-Object {
        -not (Test-D5ProgramUnderRoot ([string]$_.Program) $StableRoot)
    })
    $delta = New-D5FirewallSemanticTransition `
        $initialExternal $currentExternal $PlaywrightPolicy $Phase
    $violations = [Collections.Generic.List[string]]::new()
    foreach ($violation in @($before.Violations) + @($after.Violations)) {
        if (-not $violations.Contains([string]$violation)) {
            $violations.Add([string]$violation)
        }
    }
    if ([int]$delta.NetRuleGrowth -gt $script:D5ExternalFirewallDebtMaxNetGrowthPerRun) {
        $violations.Add('exceeded the per-run external firewall growth cap')
    }
    return [pscustomobject][ordered]@{
        Before = $before
        After = $after
        NetRuleGrowth = [int]$delta.NetRuleGrowth
        AddedRuleCount = [int]$delta.AddedRuleCount
        RemovedRuleCount = [int]$delta.RemovedRuleCount
        ChangedRuleCount = [int]$delta.ChangedRuleCount
        Added = @($delta.Added)
        Removed = @($delta.Removed)
        Changed = @($delta.Changed)
        Violations = @($violations)
        CleanupAdvisory = $after.CleanupAdvisory
    }
}

function Assert-D5ExternalFirewallTransition([object]$Transition, [string]$Phase) {
    if (@($Transition.Violations).Count -gt 0) {
        throw "$Phase $(@($Transition.Violations) -join '; ')"
    }
}

function Assert-D5ActiveStoreFirewallTransition(
    [object[]]$RunInitialRules,
    [object[]]$PhaseInitialRules,
    [object[]]$CurrentRules,
    [string]$StableRoot,
    [object]$PlaywrightPolicy,
    [bool]$AllowColdRegistration,
    [string[]]$ExpectedColdPrograms,
    [string]$Phase
) {
    $phaseInitial = @{}
    foreach ($rule in @($PhaseInitialRules)) {
        $identity = Get-D5RuleIdentity $rule
        if ($phaseInitial.ContainsKey($identity)) {
            throw "$Phase initial ActiveStore snapshot repeats firewall identity: $identity"
        }
        $phaseInitial[$identity] = [pscustomobject]@{
            Rule = $rule
            Signature = ConvertTo-D5RuleSignature $rule
        }
    }
    $current = @{}
    foreach ($rule in @($CurrentRules)) {
        $identity = Get-D5RuleIdentity $rule
        if ($current.ContainsKey($identity)) {
            throw "$Phase current ActiveStore snapshot repeats firewall identity: $identity"
        }
        $current[$identity] = [pscustomobject]@{
            Rule = $rule
            Signature = ConvertTo-D5RuleSignature $rule
        }
    }

    foreach ($identity in $phaseInitial.Keys) {
        $entry = $phaseInitial[$identity]
        if (-not (Test-D5ProgramUnderRoot ([string]$entry.Rule.Program) $StableRoot)) {
            continue
        }
        if (-not $current.ContainsKey($identity)) {
            throw "$Phase removed stable-root firewall identity: $identity"
        }
        if ([string]$entry.Signature -cne [string]$current[$identity].Signature) {
            throw "$Phase altered stable-root firewall semantics: $identity"
        }
    }

    $newStableRules = [Collections.Generic.List[object]]::new()
    foreach ($identity in $current.Keys) {
        $entry = $current[$identity]
        if ((Test-D5ProgramUnderRoot ([string]$entry.Rule.Program) $StableRoot) -and
            -not $phaseInitial.ContainsKey($identity)) {
            $newStableRules.Add($entry.Rule)
        }
    }
    if ($newStableRules.Count -gt 0 -and -not $AllowColdRegistration) {
        throw "$Phase added stable-root firewall identity outside an explicit cold-registration phase"
    }
    if ($newStableRules.Count -gt 0) {
        Assert-D5ColdRegistrationRuleSet `
            @($newStableRules) `
            $ExpectedColdPrograms `
            'Cold ActiveStore transition'
    }

    $external = New-D5ExternalFirewallTransition `
        $RunInitialRules $CurrentRules $StableRoot $PlaywrightPolicy $Phase
    $playwrightDebt = $null
    if ($null -ne $PlaywrightPolicy) {
        $playwrightAtStart = Get-D5PlaywrightFirewallDebtSnapshot `
            $RunInitialRules $PlaywrightPolicy "$Phase initial Playwright debt"
        $playwrightDebt = Get-D5PlaywrightFirewallDebtSnapshot `
            $CurrentRules $PlaywrightPolicy $Phase
        $initialPairPrograms = New-D5ProgramSet @(
            $playwrightAtStart.Entries |
                Where-Object BoundedPair |
                ForEach-Object { [string]$_.Program }
        )
        $newPairs = @(
            $playwrightDebt.Entries |
                Where-Object BoundedPair |
                Where-Object { -not $initialPairPrograms.Contains([string]$_.Program) }
        ).Count
        if ($newPairs -gt [int]$PlaywrightPolicy.MaxNewPairsPerRun) {
            throw "$Phase exceeded the per-run Playwright firewall pair cap"
        }
        Assert-D5PlaywrightFirewallDebtSnapshot $playwrightAtStart $Phase
        Assert-D5PlaywrightFirewallDebtSnapshot $playwrightDebt $Phase
    }
    Assert-D5ExternalFirewallTransition $external $Phase
    $stableRootDelta = New-D5FirewallSemanticTransition `
        @($PhaseInitialRules | Where-Object {
            Test-D5ProgramUnderRoot ([string]$_.Program) $StableRoot
        }) `
        @($CurrentRules | Where-Object {
            Test-D5ProgramUnderRoot ([string]$_.Program) $StableRoot
        }) `
        $PlaywrightPolicy `
        "$Phase stable-root transition"
    return [pscustomobject][ordered]@{
        StableRootDelta = $stableRootDelta
        PlaywrightDebt = $playwrightDebt
        ExternalDebt = $external
    }
}

function Assert-D5FixedRule(
    [object]$Rule,
    [Collections.Generic.HashSet[string]]$Expected,
    [string]$Phase
) {
    $program = [IO.Path]::GetFullPath([string]$Rule.Program)
    if ($Expected.Count -gt 0 -and -not $Expected.Contains($program)) {
        throw "$Phase found an unexpected firewall program: $program"
    }
    $instanceID = ConvertTo-D5SemanticValue $Rule.InstanceID
    $pattern = '(?i)^(TCP|UDP) Query User\{[0-9a-f-]{36}\}' + [regex]::Escape($program) + '$'
    if ($instanceID -notmatch $pattern) {
        throw "$Phase found a non-Query-User rule: $instanceID"
    }
    $protocol = ConvertTo-D5ProtocolName $Rule.Protocol
    if ($protocol -ne $Matches[1].ToUpperInvariant()) {
        throw "$Phase protocol $protocol does not match $($Matches[1]) identity"
    }
    $profileValue = ConvertTo-D5SemanticValue $Rule.Profile
    $profile = @($profileValue -split ',' | ForEach-Object { $_.Trim() } | Sort-Object)
    $action = ConvertTo-D5SemanticValue $Rule.Action
    if ((ConvertTo-D5SemanticValue $Rule.RuleID) -ne $instanceID -or
        (ConvertTo-D5SemanticValue $Rule.Enabled) -ne 'True' -or
        (ConvertTo-D5SemanticValue $Rule.Direction) -ne 'Inbound' -or
        $action -notin @('Allow', 'Block') -or
        ($profile -join ',') -ne 'Private,Public' -or
        (ConvertTo-D5SemanticValue $Rule.PolicyStoreSourceType) -ne 'Local' -or
        (ConvertTo-D5SemanticValue $Rule.LocalPort) -ne 'Any' -or
        (ConvertTo-D5SemanticValue $Rule.RemotePort) -ne 'Any' -or
        (ConvertTo-D5SemanticValue $Rule.LocalAddress) -ne 'Any' -or
        (ConvertTo-D5SemanticValue $Rule.RemoteAddress) -ne 'Any') {
        throw "$Phase rule is not the exact bounded Query User shape: $(ConvertTo-D5RuleSignature $Rule)"
    }
}

function Assert-D5ColdRegistrationRuleSet(
    [object[]]$Rules,
    [string[]]$ExpectedPrograms,
    [string]$Phase
) {
    if ($Rules.Count -eq 0) {
        return
    }
    $expected = New-D5ProgramSet $ExpectedPrograms
    if ($expected.Count -eq 0) {
        throw "$Phase has no expected stable program"
    }
    $protocolsByProgram = @{}
    foreach ($rule in $Rules) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        Assert-D5FixedRule $rule $expected $Phase
        if ((ConvertTo-D5SemanticValue $rule.Action) -cne 'Block') {
            throw "$Phase requires an exact Block TCP/UDP pair for $program"
        }
        if (-not $protocolsByProgram.ContainsKey($program)) {
            $protocolsByProgram[$program] = [Collections.Generic.HashSet[string]]::new(
                [StringComparer]::OrdinalIgnoreCase
            )
        }
        if (-not $protocolsByProgram[$program].Add((ConvertTo-D5ProtocolName $rule.Protocol))) {
            throw "$Phase found a duplicate protocol identity for $program"
        }
    }
    Assert-D5ExactProtocolPairs $protocolsByProgram $Phase
}

function Test-D5FirewallEventUnderRoot([object]$Event, [string]$Root) {
    $fieldsProperty = $Event.PSObject.Properties['Fields']
    if ($null -eq $fieldsProperty) {
        return $true
    }
    $pathProperty = $fieldsProperty.Value.PSObject.Properties['ApplicationPath']
    if ($null -eq $pathProperty -or
        [string]::IsNullOrWhiteSpace([string]$pathProperty.Value)) {
        return $true
    }
    try {
        return Test-D5ProgramUnderRoot ([string]$pathProperty.Value) $Root
    } catch {
        # An unclassifiable relevant event cannot be safely demoted to host telemetry.
        return $true
    }
}

function Assert-D5ColdEvent([object]$Event, [Collections.Generic.HashSet[string]]$Expected) {
    if ([int]$Event.EventID -ne 2097 -and [int]$Event.EventID -ne 2099) {
        throw "Cold phase emitted forbidden firewall event $($Event.EventID)"
    }
    $fields = $Event.Fields
    $program = [IO.Path]::GetFullPath([string]$fields.ApplicationPath)
    if (-not $Expected.Contains($program)) {
        throw "Cold phase event names an unexpected program: $program"
    }
    if ([string]$fields.Direction -ne '1' -or
        [string]$fields.Action -ne '2' -or
        [string]$fields.Profiles -ne '6' -or
        [string]$fields.LocalPorts -ne '*' -or
        [string]$fields.RemotePorts -ne '*' -or
        [string]$fields.LocalAddresses -ne '*' -or
        [string]$fields.RemoteAddresses -ne '*') {
        throw "Cold phase event is not the exact bounded Query User shape for a Block registration: $($Event | ConvertTo-Json -Depth 6 -Compress)"
    }
}

function Assert-D5ColdFirewallRegistration(
    [object]$Audit,
    [string[]]$ExpectedPrograms,
    [object]$OwnershipPolicy,
    [object]$OwnershipBaseline
) {
    Assert-D5FirewallPreflight @($Audit.BeforeRules) $OwnershipPolicy $OwnershipBaseline
    Assert-D5FirewallPreflight @($Audit.AfterRules) $OwnershipPolicy $OwnershipBaseline
    $expected = New-D5ProgramSet $ExpectedPrograms
    $beforeRules = @(
        $Audit.BeforeRules |
            Where-Object {
                $OwnershipPolicy.AuthorizedProgramSet.Contains(
                    [IO.Path]::GetFullPath([string]$_.Program)
                )
            }
    )
    $afterRules = @(
        $Audit.AfterRules |
            Where-Object {
                $OwnershipPolicy.AuthorizedProgramSet.Contains(
                    [IO.Path]::GetFullPath([string]$_.Program)
                )
            }
    )

    $before = @{}
    foreach ($rule in $beforeRules) {
        $before[(Get-D5RuleIdentity $rule)] = ConvertTo-D5RuleSignature $rule
    }
    $after = @{}
    $newRules = [Collections.Generic.List[object]]::new()
    foreach ($rule in $afterRules) {
        $identity = Get-D5RuleIdentity $rule
        if ($after.ContainsKey($identity)) {
            throw "Cold phase produced duplicate firewall identity: $identity"
        }
        $signature = ConvertTo-D5RuleSignature $rule
        $after[$identity] = $signature
        if ($before.ContainsKey($identity)) {
            if ($before[$identity] -ne $signature) {
                throw "Cold phase changed existing firewall semantics: $identity"
            }
        } else {
            $newRules.Add($rule)
        }
    }
    foreach ($identity in $before.Keys) {
        if (-not $after.ContainsKey($identity)) {
            throw "Cold phase removed an existing firewall identity: $identity"
        }
    }

    Assert-D5ColdRegistrationRuleSet @($newRules) $ExpectedPrograms 'Cold phase'

    $newInstanceIDs = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($rule in $newRules) {
        [void]$newInstanceIDs.Add((ConvertTo-D5SemanticValue $rule.InstanceID))
    }
    $addCounts = @{}
    $modifyCounts = @{}
    $stableEvents = @(
        $Audit.NewRelevantEvents |
            Where-Object {
                Test-D5FirewallEventUnderRoot $_ $OwnershipPolicy.HarnessRoot
            }
    )
    foreach ($event in $stableEvents) {
        Assert-D5ColdEvent $event $expected
        $ruleID = [string]$event.Fields.RuleId
        if (-not $newInstanceIDs.Contains($ruleID)) {
            throw "Cold phase event does not belong to a newly created identity: $ruleID"
        }
        $counts = if ([int]$event.EventID -eq 2097) { $addCounts } else { $modifyCounts }
        if (-not $counts.ContainsKey($ruleID)) {
            $counts[$ruleID] = 0
        }
        $counts[$ruleID]++
    }
    foreach ($ruleID in $newInstanceIDs) {
        $adds = if ($addCounts.ContainsKey($ruleID)) { [int]$addCounts[$ruleID] } else { 0 }
        $modifies = if ($modifyCounts.ContainsKey($ruleID)) { [int]$modifyCounts[$ruleID] } else { 0 }
        if ($adds -ne 1) {
            throw "Cold phase add events for $ruleID = $adds, want 1"
        }
        if ($modifies -gt 3) {
            throw "Cold phase emitted too many modifications for $ruleID"
        }
    }
    if ($stableEvents.Count -gt 4 * $newRules.Count) {
        throw 'Cold phase emitted too many stable-root firewall events for its bounded identities'
    }
    if ($newRules.Count -eq 0 -and $stableEvents.Count -ne 0) {
        throw 'Cold phase emitted stable-root firewall events without a new stable identity'
    }
}

function Assert-D5FirewallUnchanged(
    [object]$Audit,
    [object]$OwnershipPolicy,
    [object]$OwnershipBaseline
) {
    Assert-D5FirewallPreflight @($Audit.BeforeRules) $OwnershipPolicy $OwnershipBaseline
    Assert-D5FirewallPreflight @($Audit.AfterRules) $OwnershipPolicy $OwnershipBaseline

    $before = @(
        $Audit.BeforeRules |
            Where-Object {
                Test-D5ProgramUnderRoot ([string]$_.Program) $OwnershipPolicy.HarnessRoot
            } |
            ForEach-Object { ConvertTo-D5RuleSignature $_ } |
            Sort-Object
    )
    $after = @(
        $Audit.AfterRules |
            Where-Object {
                Test-D5ProgramUnderRoot ([string]$_.Program) $OwnershipPolicy.HarnessRoot
            } |
            ForEach-Object { ConvertTo-D5RuleSignature $_ } |
            Sort-Object
    )
    if ($before.Count -ne $after.Count -or
        @(Compare-Object -ReferenceObject $before -DifferenceObject $after).Count -ne 0) {
        throw 'Windows automation changed stable-root firewall identity or semantics'
    }
    $stableEvents = @(
        $Audit.NewRelevantEvents |
            Where-Object {
                Test-D5FirewallEventUnderRoot $_ $OwnershipPolicy.HarnessRoot
            }
    )
    if ($stableEvents.Count -ne 0) {
        throw 'Windows automation emitted a stable-root firewall rule event'
    }
}

function Assert-D5FirewallAuditChain(
    [object[]]$Audits,
    [long]$InitialRecordID,
    [object[]]$InitialRules
) {
    $recordID = $InitialRecordID
    $rules = @($InitialRules)
    foreach ($audit in $Audits) {
        if ([long]$audit.BeforeRecordID -ne $recordID) {
            throw "Firewall audit cursor is discontinuous before phase $($audit.Phase)"
        }
        if (-not (Test-D5RuleSetsEqual $rules @($audit.BeforeRules))) {
            throw "Firewall audit rule baseline is discontinuous before phase $($audit.Phase)"
        }
        if ([long]$audit.AfterRecordID -lt [long]$audit.BeforeRecordID) {
            throw "Firewall audit cursor moved backwards during phase $($audit.Phase)"
        }
        foreach ($event in @($audit.NewRelevantEvents)) {
            if ([long]$event.RecordID -le [long]$audit.BeforeRecordID -or
                [long]$event.RecordID -gt [long]$audit.AfterRecordID) {
                throw "Firewall event $($event.RecordID) is outside phase $($audit.Phase)'s cursor interval"
            }
        }
        $recordID = [long]$audit.AfterRecordID
        $rules = @($audit.AfterRules)
    }
}

function Get-D5ProgramRuleSignatures([object[]]$Rules, [string]$Program) {
    $fullPath = [IO.Path]::GetFullPath($Program)
    return @(
        $Rules |
            Where-Object {
                -not [string]::IsNullOrWhiteSpace([string]$_.Program) -and
                [IO.Path]::GetFullPath([string]$_.Program).Equals(
                    $fullPath,
                    [StringComparison]::OrdinalIgnoreCase
                )
            } |
            ForEach-Object { ConvertTo-D5RuleSignature $_ } |
            Sort-Object
    )
}

# The registration Entries functions carry the determinism core alone: do the
# pre-registered rule pairs for the fixed paths exist, hold the bounded
# Query-User shape, and match what was recorded? The preflighted
# *State/*Attempt wrappers add the ownership-baseline forensics on top; the
# NetworkTests flow calls the cores directly (owner decision 2026-07-14) while
# every audited mode keeps the wrappers.
function Assert-D5FixedRegistrationRules(
    [object[]]$Rules,
    [object]$OwnershipPolicy
) {
    # Pure rule-shape validation over the fixed paths: the bounded Query-User
    # shape and exact TCP/UDP pairing need no ownership baseline, so the lean
    # flow keeps them — recording a silently-minted wrong-shape pair would
    # poison the state file that every audited mode trusts.
    $identities = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $protocolsByProgram = @{}
    foreach ($rule in @($Rules)) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        if (-not $OwnershipPolicy.AuthorizedProgramSet.Contains($program)) {
            continue
        }
        Assert-D5FixedRule $rule $OwnershipPolicy.AuthorizedProgramSet 'Registration'
        $identity = Get-D5RuleIdentity $rule
        if (-not $identities.Add($identity)) {
            throw "Registration found a duplicate stable firewall identity: $identity"
        }
        if (-not $protocolsByProgram.ContainsKey($program)) {
            $protocolsByProgram[$program] = [Collections.Generic.HashSet[string]]::new(
                [StringComparer]::OrdinalIgnoreCase
            )
        }
        if (-not $protocolsByProgram[$program].Add((ConvertTo-D5ProtocolName $rule.Protocol))) {
            throw "Registration found a duplicate protocol identity for $program"
        }
    }
    Assert-D5ExactProtocolPairs $protocolsByProgram 'Registration'
}

function New-D5FirewallRegistrationEntries(
    [object[]]$Rules,
    [object]$OwnershipPolicy
) {
    Assert-D5FixedRegistrationRules $Rules $OwnershipPolicy
    $entries = [Collections.Generic.List[object]]::new()
    foreach ($program in $OwnershipPolicy.AuthorizedPrograms | Sort-Object -Unique) {
        $signatures = @(Get-D5ProgramRuleSignatures $Rules $program)
        if ($signatures.Count -eq 0) {
            continue
        }
        if ($signatures.Count -ne 2) {
            throw "Initial registration state requires an exact TCP/UDP pair for $program"
        }
        $entries.Add([pscustomobject][ordered]@{
            Program = [IO.Path]::GetFullPath($program)
            Disposition = 'RegisteredPair'
            RuleSignatures = $signatures
        })
    }
    return [pscustomobject][ordered]@{
        SchemaVersion = $script:D5FirewallRegistrationSchemaVersion
        HarnessRoot = [IO.Path]::GetFullPath($OwnershipPolicy.HarnessRoot)
        Entries = @($entries)
    }
}

function New-D5FirewallRegistrationState(
    [object[]]$Rules,
    [object]$OwnershipPolicy,
    [object]$OwnershipBaseline
) {
    Assert-D5FirewallPreflight $Rules $OwnershipPolicy $OwnershipBaseline
    return New-D5FirewallRegistrationEntries $Rules $OwnershipPolicy
}

function Assert-D5FirewallRegistrationEntries(
    [object]$State,
    [object[]]$Rules,
    [object]$OwnershipPolicy
) {
    if ([int]$State.SchemaVersion -ne $script:D5FirewallRegistrationSchemaVersion -or
        -not [IO.Path]::GetFullPath([string]$State.HarnessRoot).Equals(
            [IO.Path]::GetFullPath($OwnershipPolicy.HarnessRoot),
            [StringComparison]::OrdinalIgnoreCase
        )) {
        throw 'Firewall registration state has an invalid schema or harness root'
    }
    Assert-D5FixedRegistrationRules $Rules $OwnershipPolicy
    $authorized = $OwnershipPolicy.AuthorizedProgramSet
    $entries = @{}
    foreach ($entry in @($State.Entries)) {
        $program = [IO.Path]::GetFullPath([string]$entry.Program)
        if (-not $authorized.Contains($program)) {
            throw "Firewall registration state contains an unauthorized program: $program"
        }
        if ($entries.ContainsKey($program)) {
            throw "Firewall registration state repeats program $program"
        }
        $disposition = [string]$entry.Disposition
        if ($disposition -notin @('RegisteredPair', 'NoRegistration')) {
            throw "Firewall registration state has an invalid disposition for $program"
        }
        $recorded = @($entry.RuleSignatures | ForEach-Object { [string]$_ } | Sort-Object)
        if (($disposition -eq 'RegisteredPair' -and $recorded.Count -ne 2) -or
            ($disposition -eq 'NoRegistration' -and $recorded.Count -ne 0)) {
            throw "Firewall registration state has invalid signatures for $program"
        }
        $current = @(Get-D5ProgramRuleSignatures $Rules $program)
        if ($current.Count -ne $recorded.Count -or
            @(Compare-Object -ReferenceObject $recorded -DifferenceObject $current).Count -ne 0) {
            throw "Firewall registration state changed before launch for $program"
        }
        $entries[$program] = $entry
    }
    foreach ($rule in @($Rules | Where-Object {
        Test-D5ProgramUnderRoot ([string]$_.Program) $OwnershipPolicy.HarnessRoot
    })) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        if ($OwnershipPolicy.RetiredProgramSet.Contains($program)) {
            continue
        }
        if (-not $entries.ContainsKey($program)) {
            throw "Firewall rules contain an unrecorded stable program before launch: $program"
        }
    }
}

function Assert-D5FirewallRegistrationState(
    [object]$State,
    [object[]]$Rules,
    [object]$OwnershipPolicy,
    [object]$OwnershipBaseline
) {
    Assert-D5FirewallPreflight $Rules $OwnershipPolicy $OwnershipBaseline
    Assert-D5FirewallRegistrationEntries $State $Rules $OwnershipPolicy
}

function Get-D5PendingRegistrationPrograms(
    [object]$State,
    [string[]]$ExpectedPrograms
) {
    $recorded = New-D5ProgramSet @($State.Entries | ForEach-Object { [string]$_.Program })
    return @(
        $ExpectedPrograms |
            ForEach-Object { [IO.Path]::GetFullPath($_) } |
            Where-Object { -not $recorded.Contains($_) } |
            Sort-Object -Unique
    )
}

function Complete-D5FirewallRegistrationEntries(
    [object]$State,
    [object[]]$Rules,
    [string[]]$AttemptedPrograms,
    [object]$OwnershipPolicy
) {
    $attempted = New-D5ProgramSet $AttemptedPrograms
    $existing = New-D5ProgramSet @($State.Entries | ForEach-Object { [string]$_.Program })
    $entries = [Collections.Generic.List[object]]::new()
    foreach ($entry in @($State.Entries)) {
        $entries.Add($entry)
    }
    foreach ($program in $attempted | Sort-Object) {
        if ($existing.Contains($program)) {
            throw "Firewall registration attempt repeated recorded program $program"
        }
        $signatures = @(Get-D5ProgramRuleSignatures $Rules $program)
        if ($signatures.Count -ne 0 -and $signatures.Count -ne 2) {
            throw "Firewall registration attempt did not produce an exact TCP/UDP pair for $program"
        }
        $entries.Add([pscustomobject][ordered]@{
            Program = $program
            Disposition = if ($signatures.Count -eq 2) { 'RegisteredPair' } else { 'NoRegistration' }
            RuleSignatures = $signatures
        })
    }
    $next = [pscustomobject][ordered]@{
        SchemaVersion = $script:D5FirewallRegistrationSchemaVersion
        HarnessRoot = [IO.Path]::GetFullPath($OwnershipPolicy.HarnessRoot)
        Entries = @($entries | Sort-Object Program)
    }
    Assert-D5FirewallRegistrationEntries $next $Rules $OwnershipPolicy
    return $next
}

function Complete-D5FirewallRegistrationAttempt(
    [object]$State,
    [object[]]$Rules,
    [string[]]$AttemptedPrograms,
    [object]$OwnershipPolicy,
    [object]$OwnershipBaseline
) {
    Assert-D5FirewallPreflight $Rules $OwnershipPolicy $OwnershipBaseline
    return Complete-D5FirewallRegistrationEntries `
        $State `
        $Rules `
        $AttemptedPrograms `
        $OwnershipPolicy
}
