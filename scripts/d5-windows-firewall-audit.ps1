Set-StrictMode -Version Latest

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
    $fullProgram = [IO.Path]::GetFullPath(
        [Environment]::ExpandEnvironmentVariables($Program)
    )
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
        [string]$fields.Action -notin @('2', '3') -or
        [string]$fields.Profiles -ne '6' -or
        [string]$fields.LocalPorts -ne '*' -or
        [string]$fields.RemotePorts -ne '*' -or
        [string]$fields.LocalAddresses -ne '*' -or
        [string]$fields.RemoteAddresses -ne '*') {
        throw "Cold phase event is not the exact bounded Query User shape: $($Event | ConvertTo-Json -Depth 6 -Compress)"
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
            Assert-D5FixedRule $rule $expected 'Cold phase'
            $newRules.Add($rule)
        }
    }
    foreach ($identity in $before.Keys) {
        if (-not $after.ContainsKey($identity)) {
            throw "Cold phase removed an existing firewall identity: $identity"
        }
    }

    $protocolsByProgram = @{}
    foreach ($rule in $newRules) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        if (-not $protocolsByProgram.ContainsKey($program)) {
            $protocolsByProgram[$program] = [Collections.Generic.HashSet[string]]::new(
                [StringComparer]::OrdinalIgnoreCase
            )
        }
        if (-not $protocolsByProgram[$program].Add((ConvertTo-D5ProtocolName $rule.Protocol))) {
            throw "Cold phase produced duplicate protocol identity for $program"
        }
    }
    Assert-D5ExactProtocolPairs $protocolsByProgram 'Cold phase'

    $newInstanceIDs = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($rule in $newRules) {
        [void]$newInstanceIDs.Add((ConvertTo-D5SemanticValue $rule.InstanceID))
    }
    $addCounts = @{}
    $modifyCounts = @{}
    foreach ($event in @($Audit.NewRelevantEvents)) {
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
    if (@($Audit.NewRelevantEvents).Count -gt 4 * $newRules.Count) {
        throw 'Cold phase emitted too many firewall events for its bounded identities'
    }
    if ($newRules.Count -eq 0 -and @($Audit.NewRelevantEvents).Count -ne 0) {
        throw 'Cold phase emitted firewall events without a new stable identity'
    }
}

function Assert-D5FirewallUnchanged(
    [object]$Audit,
    [object]$OwnershipPolicy,
    [object]$OwnershipBaseline
) {
    Assert-D5FirewallPreflight @($Audit.BeforeRules) $OwnershipPolicy $OwnershipBaseline
    Assert-D5FirewallPreflight @($Audit.AfterRules) $OwnershipPolicy $OwnershipBaseline

    $before = @($Audit.BeforeRules | ForEach-Object {
        ConvertTo-D5RuleSignature $_
    } | Sort-Object)
    $after = @($Audit.AfterRules | ForEach-Object {
        ConvertTo-D5RuleSignature $_
    } | Sort-Object)
    if ($before.Count -ne $after.Count -or
        @(Compare-Object -ReferenceObject $before -DifferenceObject $after).Count -ne 0) {
        throw 'Windows automation changed firewall rule identity or semantics'
    }
    if (@($Audit.NewRelevantEvents).Count -ne 0) {
        throw 'Windows automation emitted a relevant firewall rule event'
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
        SchemaVersion = 1
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
    if ([int]$State.SchemaVersion -ne 1 -or
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
        SchemaVersion = 1
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
