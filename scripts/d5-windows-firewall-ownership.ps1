Set-StrictMode -Version Latest

$script:D5FirewallOwnershipSchemaVersion = 1
$script:D5SHA256Pattern = '^[0-9a-f]{64}$'

function Get-D5OrdinalPayloadSHA256([string[]]$Rows) {
    $ordered = @($Rows | ForEach-Object { [string]$_ })
    [Array]::Sort($ordered, [StringComparer]::Ordinal)
    $payload = if ($ordered.Count -eq 0) { '' } else { ($ordered -join "`n") + "`n" }
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($payload)
    return [Convert]::ToHexString(
        [Security.Cryptography.SHA256]::HashData($bytes)
    ).ToLowerInvariant()
}

function Test-D5OrdinalStringSetsEqual([string[]]$Expected, [string[]]$Actual) {
    $left = @($Expected | ForEach-Object { [string]$_ })
    $right = @($Actual | ForEach-Object { [string]$_ })
    [Array]::Sort($left, [StringComparer]::Ordinal)
    [Array]::Sort($right, [StringComparer]::Ordinal)
    if ($left.Count -ne $right.Count) {
        return $false
    }
    for ($index = 0; $index -lt $left.Count; $index++) {
        if ($left[$index] -cne $right[$index]) {
            return $false
        }
    }
    return $true
}

function ConvertTo-D5ProgramStateSignature([string]$Program, [bool]$Exists, [string]$SHA256) {
    $hash = if ([string]::IsNullOrWhiteSpace($SHA256)) {
        ''
    } else {
        $SHA256.ToLowerInvariant()
    }
    return @(
        [IO.Path]::GetFullPath($Program).ToLowerInvariant()
        $(if ($Exists) { 'present' } else { 'absent' })
        $hash
    ) -join "`t"
}

function ConvertTo-D5RuleObservationSignature([object]$Rule) {
    $existsProperty = $Rule.PSObject.Properties['ProgramExists']
    $hashProperty = $Rule.PSObject.Properties['ProgramSHA256']
    return [ordered]@{
        Rule = ConvertTo-D5RuleSignature $Rule
        ProgramState = ConvertTo-D5ProgramStateSignature `
            ([string]$Rule.Program) `
            $(if ($null -eq $existsProperty) { $false } else { [bool]$existsProperty.Value }) `
            $(if ($null -eq $hashProperty) { '' } else { [string]$hashProperty.Value })
    } | ConvertTo-Json -Compress
}

function New-D5ExcludedEvidenceRule(
    [string]$Program,
    [string]$Protocol,
    [string]$Guid
) {
    $ruleID = "$Protocol Query User{$Guid}$Program"
    return [pscustomobject][ordered]@{
        RuleID = $ruleID
        InstanceID = $ruleID
        Program = $Program
        Direction = 'Inbound'
        Action = 'Allow'
        Profile = 'Private, Public'
        Enabled = $true
        PolicyStoreSourceType = 'Local'
        Protocol = $Protocol
        LocalPort = 'Any'
        RemotePort = 'Any'
        LocalAddress = 'Any'
        RemoteAddress = 'Any'
    }
}

function Get-D5CleanupOwnedProgramRoot([string]$Program) {
    $path = [IO.Path]::GetFullPath($Program)
    $tempMatch = [regex]::Match(
        $path,
        '(?i)^(?<root>.+\\temp\\(?:go-build[0-9]+|windshare-c5-[^\\]+|windshare-e2e-bin[0-9]+))(?:\\|$)'
    )
    if ($tempMatch.Success) {
        return [IO.Path]::GetFullPath($tempMatch.Groups['root'].Value)
    }
    $cacheMatch = [regex]::Match(
        $path,
        '(?i)^(?<root>.+\\go-build\\[0-9a-f]{2}\\[0-9a-f]{64}-d)(?:\\|$)'
    )
    if ($cacheMatch.Success) {
        return [IO.Path]::GetFullPath($cacheMatch.Groups['root'].Value)
    }
    throw "Cleanup-owned program has an unrecognized durable path shape: $path"
}

function Import-D5FirewallOwnershipEvidence(
    [string]$RepositoryRoot,
    [string]$ManifestPath
) {
    $root = [IO.Path]::GetFullPath($RepositoryRoot)
    $manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
    if ([int]$manifest.SchemaVersion -ne $script:D5FirewallOwnershipSchemaVersion) {
        throw 'D5 firewall ownership evidence has an unsupported schema'
    }

    $sourceIdentities = [Collections.Generic.List[object]]::new()
    foreach ($source in @($manifest.Sources)) {
        $relativePath = ([string]$source.RelativePath).Replace(
            '/',
            [IO.Path]::DirectorySeparatorChar
        )
        $path = [IO.Path]::GetFullPath((Join-Path $root $relativePath))
        if (-not (Test-D5ProgramUnderRoot $path $root)) {
            throw "D5 firewall ownership source escapes the repository: $relativePath"
        }
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "D5 firewall ownership source is missing: $relativePath"
        }
        $expectedHash = ([string]$source.SHA256).ToLowerInvariant()
        $actualHash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($expectedHash -notmatch $script:D5SHA256Pattern -or $actualHash -cne $expectedHash) {
            throw "D5 firewall ownership source hash changed: $relativePath"
        }
        $sourceIdentities.Add([pscustomobject][ordered]@{
            RelativePath = ([string]$source.RelativePath).Replace('\', '/')
            SHA256 = $actualHash
        })
    }

    $candidateRelativePath = 'docs/.orchestration/firewall-cleanup-candidates.md'
    $candidateSources = @(
        $manifest.Sources |
            Where-Object { [string]$_.RelativePath -ceq $candidateRelativePath }
    )
    if ($candidateSources.Count -ne 1) {
        throw 'D5 firewall ownership evidence must pin one cleanup candidate document'
    }
    $candidatePath = Join-Path $root ($candidateRelativePath.Replace(
        '/',
        [IO.Path]::DirectorySeparatorChar
    ))
    $candidateLines = @(Get-Content -LiteralPath $candidatePath)
    $candidateStart = -1
    $candidateEnd = -1
    for ($index = 0; $index -lt $candidateLines.Count; $index++) {
        if ($candidateLines[$index] -ceq '## Exact candidate payload') {
            $candidateStart = $index
        } elseif ($candidateLines[$index] -ceq '## Stable D5 identities to preserve') {
            $candidateEnd = $index
        }
    }
    if ($candidateStart -lt 0 -or $candidateEnd -le $candidateStart) {
        throw 'D5 firewall ownership evidence cannot locate the cleanup candidate payload'
    }
    $cleanupProgramPaths = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase
    )
    $cleanupProgramRoots = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase
    )
    $cleanupProgramNames = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase
    )
    $cleanupProgramHashes = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase
    )
    $cleanupRows = [Collections.Generic.List[string]]::new()
    for ($index = $candidateStart + 1; $index -lt $candidateEnd; $index++) {
        $fields = @(
            $candidateLines[$index].Trim('|').Split('|') |
                ForEach-Object { $_.Trim() }
        )
        if ($fields.Count -ne 7 -or
            $fields[1] -notmatch '^[A-Za-z]:\\' -or
            $fields[3] -notmatch '^\{[0-9A-F-]{36}\}$' -or
            $fields[4] -notmatch '^\{[0-9A-F-]{36}\}$') {
            continue
        }
        $program = [IO.Path]::GetFullPath($fields[1])
        $action = $fields[2]
        if ($action -notin @('Allow', 'Block') -or -not $cleanupProgramPaths.Add($program)) {
            throw "Cleanup candidate payload has invalid or repeated ownership for $program"
        }
        [void]$cleanupProgramRoots.Add((Get-D5CleanupOwnedProgramRoot $program))
        [void]$cleanupProgramNames.Add([IO.Path]::GetFileName($program))
        if ($fields[5] -match $script:D5SHA256Pattern) {
            [void]$cleanupProgramHashes.Add($fields[5].ToLowerInvariant())
        } elseif ($fields[5] -cne 'absent') {
            throw "Cleanup candidate payload has invalid executable state for $program"
        }
        foreach ($protocolIndex in @(
            [pscustomobject]@{ Protocol = 'TCP'; Field = 3 },
            [pscustomobject]@{ Protocol = 'UDP'; Field = 4 }
        )) {
            $protocol = [string]$protocolIndex.Protocol
            $guid = $fields[[int]$protocolIndex.Field].Trim('{}')
            $ruleID = "$protocol Query User{$guid}$program"
            $cleanupRows.Add(@(
                $program.ToLowerInvariant()
                $protocol
                $ruleID
                $ruleID
                $action
                'Inbound'
                'Private, Public'
                'True'
                'Local'
                'Any'
                'Any'
                'Any'
                'Any'
            ) -join "`t")
        }
    }
    if ($cleanupProgramPaths.Count -ne [int]$manifest.CleanupOwned.ExpectedProgramCount -or
        $cleanupRows.Count -ne [int]$manifest.CleanupOwned.ExpectedRuleCount -or
        (Get-D5OrdinalPayloadSHA256 @($cleanupRows)) -cne
            ([string]$manifest.CleanupOwned.ApprovedSemanticPayloadSHA256).ToLowerInvariant()) {
        throw 'D5 cleanup-owned identity set does not match its authorized durable payload'
    }

    $packageManifestPath = Join-Path $root 'scripts\d5-windows-network-packages.json'
    $packages = @(Get-Content -LiteralPath $packageManifestPath -Raw | ConvertFrom-Json)
    $stableRelativePrograms = [Collections.Generic.List[string]]::new()
    foreach ($package in $packages) {
        $name = [string]$package.Name
        if ($name -notmatch '^[a-z0-9][a-z0-9-]*$') {
            throw "D5 package manifest has an invalid executable name: $name"
        }
        $stableRelativePrograms.Add("$name.test.exe")
    }
    foreach ($relativeProgram in @($manifest.D5.StableChildRelativePrograms)) {
        $stableRelativePrograms.Add(([string]$relativeProgram).Replace('\', '/'))
    }

    $excludedRules = [Collections.Generic.List[object]]::new()
    $excludedProgramStates = [Collections.Generic.List[object]]::new()
    $approvedRows = [Collections.Generic.List[string]]::new()
    $programs = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($entry in @($manifest.Excluded.Programs)) {
        $program = [IO.Path]::GetFullPath([string]$entry.Program)
        if (-not $programs.Add($program)) {
            throw "D5 firewall exclusion evidence repeats program $program"
        }
        $fileHash = if ($null -eq $entry.FileSHA256) {
            ''
        } else {
            ([string]$entry.FileSHA256).ToLowerInvariant()
        }
        if (-not [string]::IsNullOrEmpty($fileHash) -and
            $fileHash -notmatch $script:D5SHA256Pattern) {
            throw "D5 firewall exclusion evidence has an invalid executable hash for $program"
        }
        $excludedProgramStates.Add([pscustomobject][ordered]@{
            Program = $program
            Exists = -not [string]::IsNullOrEmpty($fileHash)
            SHA256 = $fileHash
        })

        foreach ($protocol in @('TCP', 'UDP')) {
            $property = "${protocol}Guid"
            $guid = [string]$entry.$property
            if ($guid -notmatch '^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$') {
                throw "D5 firewall exclusion evidence has an invalid $protocol identity for $program"
            }
            $rule = New-D5ExcludedEvidenceRule $program $protocol $guid
            $excludedRules.Add($rule)
            $approvedRows.Add(@(
                $program.ToLowerInvariant()
                $protocol
                [string]$rule.RuleID
                [string]$rule.InstanceID
                'Allow'
                'Inbound'
                'Private, Public'
                'True'
                'Local'
                'Any'
                'Any'
                'Any'
                'Any'
            ) -join "`t")
        }
    }

    $expectedRuleCount = [int]$manifest.Excluded.ExpectedRuleCount
    $expectedProgramCount = [int]$manifest.Excluded.ExpectedProgramCount
    if ($excludedRules.Count -ne $expectedRuleCount -or
        $excludedProgramStates.Count -ne $expectedProgramCount) {
        throw 'D5 firewall exclusion evidence count does not match its durable cleanup record'
    }
    $approvedSemanticHash = ([string]$manifest.Excluded.ApprovedSemanticPayloadSHA256).ToLowerInvariant()
    if ((Get-D5OrdinalPayloadSHA256 @($approvedRows)) -cne $approvedSemanticHash) {
        throw 'D5 firewall exclusion evidence does not reconstruct the approved semantic payload'
    }

    $historicalHashes = @(
        $manifest.D5.HistoricalProgramSHA256 |
            ForEach-Object { ([string]$_).ToLowerInvariant() } |
            Sort-Object -Unique
    )
    foreach ($hash in $historicalHashes) {
        if ($hash -notmatch $script:D5SHA256Pattern) {
            throw "D5 firewall ownership evidence has an invalid D5 executable hash: $hash"
        }
    }
    $statePayloadHash = ([string]$manifest.Excluded.ApprovedExecutableStatePayloadSHA256).ToLowerInvariant()
    if ($statePayloadHash -notmatch $script:D5SHA256Pattern) {
        throw 'D5 firewall exclusion evidence has an invalid executable-state provenance hash'
    }

    return [pscustomobject][ordered]@{
        SchemaVersion = $script:D5FirewallOwnershipSchemaVersion
        Sources = @($sourceIdentities | Sort-Object RelativePath)
        StableRelativePrograms = @($stableRelativePrograms | Sort-Object -Unique)
        D5HistoricalProgramSHA256 = $historicalHashes
        D5OwnedTemporaryPathPatterns = @($manifest.D5.OwnedTemporaryPathPatterns)
        CleanupOwnedProgramPaths = @($cleanupProgramPaths | Sort-Object)
        CleanupOwnedProgramRoots = @($cleanupProgramRoots | Sort-Object)
        CleanupOwnedProgramNames = @($cleanupProgramNames | Sort-Object)
        CleanupOwnedProgramSHA256 = @($cleanupProgramHashes | Sort-Object)
        CleanupOwnedRuleCount = [int]$manifest.CleanupOwned.ExpectedRuleCount
        CleanupOwnedProgramCount = [int]$manifest.CleanupOwned.ExpectedProgramCount
        CleanupOwnedSemanticPayloadSHA256 = (
            [string]$manifest.CleanupOwned.ApprovedSemanticPayloadSHA256
        ).ToLowerInvariant()
        ExcludedRules = @($excludedRules)
        ExcludedProgramStates = @($excludedProgramStates | Sort-Object Program)
        ExcludedRuleCount = $expectedRuleCount
        ExcludedProgramCount = $expectedProgramCount
        ExcludedSemanticPayloadSHA256 = $approvedSemanticHash
        ExcludedExecutableStateProvenanceSHA256 = $statePayloadHash
    }
}

function New-D5FirewallOwnershipPolicy(
    [string]$HarnessRoot,
    [string[]]$AuthorizedPrograms,
    [object]$Evidence,
    [string[]]$CurrentD5ProgramSHA256 = @(),
    [string[]]$ObservedRoots = @()
) {
    $root = [IO.Path]::GetFullPath($HarnessRoot)
    $authorized = New-D5ProgramSet $AuthorizedPrograms
    $evidencePrograms = @(
        $Evidence.StableRelativePrograms |
            ForEach-Object {
                $relative = ([string]$_).Replace('/', [IO.Path]::DirectorySeparatorChar)
                [IO.Path]::GetFullPath((Join-Path $root $relative))
            }
    )
    $evidenceAuthorized = New-D5ProgramSet $evidencePrograms
    if ($authorized.Count -ne $evidenceAuthorized.Count) {
        throw 'D5 firewall ownership policy does not match the executable package manifest'
    }
    foreach ($program in $authorized) {
        if (-not $evidenceAuthorized.Contains($program)) {
            throw "D5 firewall ownership policy contains an unmanifested stable program: $program"
        }
    }

    $names = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($program in $evidencePrograms) {
        [void]$names.Add([IO.Path]::GetFileName($program))
    }
    foreach ($name in @($Evidence.CleanupOwnedProgramNames)) {
        [void]$names.Add([string]$name)
    }
    $hashes = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($hash in @($Evidence.D5HistoricalProgramSHA256) +
        @($Evidence.CleanupOwnedProgramSHA256) +
        @($CurrentD5ProgramSHA256)) {
        if ([string]::IsNullOrWhiteSpace([string]$hash)) {
            continue
        }
        $normalized = ([string]$hash).ToLowerInvariant()
        if ($normalized -notmatch $script:D5SHA256Pattern) {
            throw "D5 firewall ownership policy has an invalid executable hash: $hash"
        }
        [void]$hashes.Add($normalized)
    }

    $excludedSignatures = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $excludedPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($rule in @($Evidence.ExcludedRules)) {
        if (-not $excludedSignatures.Add((ConvertTo-D5RuleSignature $rule))) {
            throw 'D5 firewall ownership evidence repeats an excluded rule signature'
        }
        [void]$excludedPrograms.Add([IO.Path]::GetFullPath([string]$rule.Program))
    }
    $cleanupPaths = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($program in @($Evidence.CleanupOwnedProgramPaths)) {
        [void]$cleanupPaths.Add([IO.Path]::GetFullPath([string]$program))
    }
    $cleanupRoots = @(
        $Evidence.CleanupOwnedProgramRoots |
            ForEach-Object { [IO.Path]::GetFullPath([string]$_) } |
            Sort-Object -Unique
    )

    $roots = @(
        @($ObservedRoots) + @($root) |
            Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) } |
            ForEach-Object { [IO.Path]::GetFullPath([string]$_) } |
            Sort-Object -Unique
    )
    return [pscustomobject]@{
        HarnessRoot = $root
        AuthorizedPrograms = @($authorized | Sort-Object)
        AuthorizedProgramSet = $authorized
        ObservedRoots = $roots
        D5ProgramNames = @($names | Sort-Object)
        D5ProgramNameSet = $names
        D5ProgramSHA256 = @($hashes | Sort-Object)
        D5ProgramSHA256Set = $hashes
        D5OwnedTemporaryPathPatterns = @($Evidence.D5OwnedTemporaryPathPatterns)
        CleanupOwnedProgramPathSet = $cleanupPaths
        CleanupOwnedProgramRoots = $cleanupRoots
        CleanupOwnedRuleCount = [int]$Evidence.CleanupOwnedRuleCount
        CleanupOwnedProgramCount = [int]$Evidence.CleanupOwnedProgramCount
        CleanupOwnedSemanticPayloadSHA256 = [string]$Evidence.CleanupOwnedSemanticPayloadSHA256
        ExcludedRules = @($Evidence.ExcludedRules)
        ExcludedRuleSignatureSet = $excludedSignatures
        ExcludedProgramSet = $excludedPrograms
        ExcludedProgramStates = @($Evidence.ExcludedProgramStates)
        ExcludedRuleCount = [int]$Evidence.ExcludedRuleCount
        ExcludedProgramCount = [int]$Evidence.ExcludedProgramCount
        ExcludedSemanticPayloadSHA256 = [string]$Evidence.ExcludedSemanticPayloadSHA256
        ExcludedExecutableStateProvenanceSHA256 = [string]$Evidence.ExcludedExecutableStateProvenanceSHA256
        EvidenceSources = @($Evidence.Sources)
    }
}

function Test-D5ProgramObserved([string]$Program, [object]$Policy) {
    foreach ($root in @($Policy.ObservedRoots)) {
        if (Test-D5ProgramUnderRoot $Program ([string]$root)) {
            return $true
        }
    }
    return $Policy.ExcludedProgramSet.Contains([IO.Path]::GetFullPath($Program))
}

function Test-D5WindShareAttributedRule([object]$Rule, [object]$Policy) {
    $program = [IO.Path]::GetFullPath([string]$Rule.Program)
    if ($Policy.CleanupOwnedProgramPathSet.Contains($program)) {
        return $true
    }
    foreach ($root in @($Policy.CleanupOwnedProgramRoots)) {
        if (Test-D5ProgramUnderRoot $program ([string]$root)) {
            return $true
        }
    }
    if ($Policy.D5ProgramNameSet.Contains([IO.Path]::GetFileName($program))) {
        return $true
    }
    $hashProperty = $Rule.PSObject.Properties['ProgramSHA256']
    if ($null -ne $hashProperty -and
        -not [string]::IsNullOrWhiteSpace([string]$hashProperty.Value) -and
        $Policy.D5ProgramSHA256Set.Contains([string]$hashProperty.Value)) {
        return $true
    }
    foreach ($pattern in @($Policy.D5OwnedTemporaryPathPatterns)) {
        if ($program -match [string]$pattern) {
            return $true
        }
    }
    return $false
}

function Get-D5RuleProgramState([object]$Rule) {
    $existsProperty = $Rule.PSObject.Properties['ProgramExists']
    $hashProperty = $Rule.PSObject.Properties['ProgramSHA256']
    $exists = $null -ne $existsProperty -and [bool]$existsProperty.Value
    $hash = if ($null -eq $hashProperty -or
        [string]::IsNullOrWhiteSpace([string]$hashProperty.Value)) {
        ''
    } else {
        ([string]$hashProperty.Value).ToLowerInvariant()
    }
    if ($exists -and $hash -notmatch $script:D5SHA256Pattern) {
        throw "Firewall observation has an invalid executable hash for $($Rule.Program)"
    }
    if (-not $exists -and -not [string]::IsNullOrEmpty($hash)) {
        throw "Firewall observation has a hash for an absent executable: $($Rule.Program)"
    }
    return [pscustomobject]@{
        Program = [IO.Path]::GetFullPath([string]$Rule.Program)
        Exists = $exists
        SHA256 = $hash
    }
}

function New-D5FirewallOwnershipSnapshot([object[]]$Rules, [object]$Policy) {
    $stableRules = [Collections.Generic.List[object]]::new()
    $pinnedRules = [Collections.Generic.List[object]]::new()
    $unrelatedRules = [Collections.Generic.List[object]]::new()
    $excludedProgramStates = @{}

    foreach ($rule in @($Rules)) {
        $program = [IO.Path]::GetFullPath([string]$rule.Program)
        if (-not (Test-D5ProgramObserved $program $Policy)) {
            throw "Firewall ownership received a program outside its observed roots: $program"
        }
        if ($Policy.AuthorizedProgramSet.Contains($program)) {
            $stableRules.Add($rule)
            continue
        }
        if (Test-D5ProgramUnderRoot $program $Policy.HarnessRoot) {
            throw "Preflight found an unmanifested program under the stable harness root: $program"
        }

        $state = Get-D5RuleProgramState $rule
        $stateSignature = ConvertTo-D5ProgramStateSignature $state.Program $state.Exists $state.SHA256
        if ($excludedProgramStates.ContainsKey($program) -and
            [string]$excludedProgramStates[$program] -cne $stateSignature) {
            throw "Firewall observations disagree about executable state for $program"
        }
        $excludedProgramStates[$program] = $stateSignature

        $signature = ConvertTo-D5RuleSignature $rule
        if ($Policy.ExcludedProgramSet.Contains($program)) {
            if (-not $Policy.ExcludedRuleSignatureSet.Contains($signature)) {
                throw "Preflight found drift on an evidence-pinned excluded program: $program"
            }
            if (Test-D5WindShareAttributedRule $rule $Policy) {
                throw "Preflight found a WindShare-attributable random/temp firewall program: $program"
            }
            $pinnedRules.Add($rule)
            continue
        }
        if (Test-D5WindShareAttributedRule $rule $Policy) {
            throw "Preflight found a WindShare-attributable random/temp firewall program: $program"
        }
        $unrelatedRules.Add($rule)
    }

    $excludedRules = @($pinnedRules) + @($unrelatedRules)
    $ruleSignatures = @($excludedRules | ForEach-Object { ConvertTo-D5RuleSignature $_ })
    $programStateSignatures = @($excludedProgramStates.Values | ForEach-Object { [string]$_ })
    $excludedPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $pinnedPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $unrelatedPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($rule in $excludedRules) {
        [void]$excludedPrograms.Add([IO.Path]::GetFullPath([string]$rule.Program))
    }
    foreach ($rule in $pinnedRules) {
        [void]$pinnedPrograms.Add([IO.Path]::GetFullPath([string]$rule.Program))
    }
    foreach ($rule in $unrelatedRules) {
        [void]$unrelatedPrograms.Add([IO.Path]::GetFullPath([string]$rule.Program))
    }
    $state = [pscustomobject][ordered]@{
        SchemaVersion = $script:D5FirewallOwnershipSchemaVersion
        EvidenceRuleCount = [int]$Policy.ExcludedRuleCount
        EvidenceProgramCount = [int]$Policy.ExcludedProgramCount
        EvidenceSemanticPayloadSHA256 = [string]$Policy.ExcludedSemanticPayloadSHA256
        EvidenceExecutableStateProvenanceSHA256 = [string]$Policy.ExcludedExecutableStateProvenanceSHA256
        PinnedExcludedRuleCount = $pinnedRules.Count
        PinnedExcludedProgramCount = $pinnedPrograms.Count
        UnrelatedExcludedRuleCount = $unrelatedRules.Count
        UnrelatedExcludedProgramCount = $unrelatedPrograms.Count
        ExcludedRuleCount = $ruleSignatures.Count
        ExcludedProgramCount = $excludedPrograms.Count
        ExcludedRulePayloadSHA256 = Get-D5OrdinalPayloadSHA256 $ruleSignatures
        ExcludedExecutableStateSHA256 = Get-D5OrdinalPayloadSHA256 $programStateSignatures
        RuleSignatures = @($ruleSignatures | Sort-Object)
        ProgramStateSignatures = @($programStateSignatures | Sort-Object)
    }
    return [pscustomobject]@{
        StableRules = @($stableRules)
        PinnedExcludedRules = @($pinnedRules)
        UnrelatedExcludedRules = @($unrelatedRules)
        State = $state
    }
}

function Assert-D5FirewallOwnershipState([object]$State) {
    if ([int]$State.SchemaVersion -ne $script:D5FirewallOwnershipSchemaVersion) {
        throw 'Firewall ownership baseline has an invalid schema'
    }
    $rules = @($State.RuleSignatures | ForEach-Object { [string]$_ })
    $programs = @($State.ProgramStateSignatures | ForEach-Object { [string]$_ })
    if ([int]$State.PinnedExcludedRuleCount + [int]$State.UnrelatedExcludedRuleCount -ne
            [int]$State.ExcludedRuleCount -or
        [int]$State.PinnedExcludedProgramCount + [int]$State.UnrelatedExcludedProgramCount -ne
            [int]$State.ExcludedProgramCount -or
        $rules.Count -ne [int]$State.ExcludedRuleCount -or
        $programs.Count -ne [int]$State.ExcludedProgramCount -or
        (Get-D5OrdinalPayloadSHA256 $rules) -cne [string]$State.ExcludedRulePayloadSHA256 -or
        (Get-D5OrdinalPayloadSHA256 $programs) -cne [string]$State.ExcludedExecutableStateSHA256) {
        throw 'Firewall ownership baseline count or digest is internally inconsistent'
    }
}

function New-D5FirewallOwnershipBaseline([object[]]$Rules, [object]$Policy) {
    $snapshot = New-D5FirewallOwnershipSnapshot $Rules $Policy
    Assert-D5FirewallOwnershipState $snapshot.State
    return $snapshot.State
}

function Assert-D5FirewallOwnershipBaseline(
    [object]$Baseline,
    [object[]]$Rules,
    [object]$Policy,
    [string]$Phase
) {
    Assert-D5FirewallOwnershipState $Baseline
    $current = (New-D5FirewallOwnershipSnapshot $Rules $Policy).State
    Assert-D5FirewallOwnershipState $current
    $scalarFields = @(
        'EvidenceRuleCount',
        'EvidenceProgramCount',
        'EvidenceSemanticPayloadSHA256',
        'EvidenceExecutableStateProvenanceSHA256',
        'PinnedExcludedRuleCount',
        'PinnedExcludedProgramCount',
        'UnrelatedExcludedRuleCount',
        'UnrelatedExcludedProgramCount',
        'ExcludedRuleCount',
        'ExcludedProgramCount',
        'ExcludedRulePayloadSHA256',
        'ExcludedExecutableStateSHA256'
    )
    foreach ($field in $scalarFields) {
        if ([string]$Baseline.$field -cne [string]$current.$field) {
            throw "$Phase changed excluded firewall ownership field $field"
        }
    }
    if (-not (Test-D5OrdinalStringSetsEqual @($Baseline.RuleSignatures) @($current.RuleSignatures)) -or
        -not (Test-D5OrdinalStringSetsEqual @($Baseline.ProgramStateSignatures) @($current.ProgramStateSignatures))) {
        throw "$Phase changed excluded firewall identities or executable state"
    }
}
