Set-StrictMode -Version Latest

$script:D5FirewallOwnershipSchemaVersion = 1
$script:D5NetworkManifestSchemaVersion = 2
$script:D5SHA256Pattern = '^[0-9a-f]{64}$'
$script:D5GUIDPattern = '^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$'
$script:D5RetiredConnectivityRelativeProgram = 'connectivity.test.exe'
$script:D5RetiredConnectivityDisplayName = 'connectivity.test'
# The cleanup history corpus is deliberately untracked local forensic evidence
# (docs/.orchestration is gitignored), so an environment that never ran a D5
# cleanup -- e.g. a fresh CI checkout -- legitimately has none of it.
$script:D5CleanupHistoryCorpusPrefix = 'docs/.orchestration/'

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

function Assert-D5ExactPropertySet(
    [object]$Value,
    [string[]]$Expected,
    [string]$Context
) {
    $actual = @($Value.PSObject.Properties.Name | ForEach-Object { [string]$_ })
    if (-not (Test-D5OrdinalStringSetsEqual $Expected $actual)) {
        throw "$Context has an invalid field set: $($actual -join ', ')"
    }
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

function New-D5RetiredProgramTombstoneRule(
    [string]$Program,
    [string]$DisplayName,
    [string]$Action,
    [string]$Protocol,
    [string]$Guid
) {
    $ruleID = "$Protocol Query User{$Guid}$Program"
    return [pscustomobject][ordered]@{
        RuleID = $ruleID
        InstanceID = $ruleID
        Program = $Program
        DisplayName = $DisplayName
        Direction = 'Inbound'
        Action = $Action
        Profile = 'Private, Public'
        Enabled = $true
        PolicyStoreSourceType = 'Local'
        Protocol = $Protocol
        LocalPort = 'Any'
        RemotePort = 'Any'
        LocalAddress = 'Any'
        RemoteAddress = 'Any'
        ProgramExists = $false
        ProgramSHA256 = ''
        ProgramProcessIDs = @()
    }
}

function ConvertTo-D5RetiredProgramRuleSignature([object]$Rule) {
    $program = [IO.Path]::GetFullPath([string]$Rule.Program).ToLowerInvariant()
    $identity = {
        param([string]$Value)
        $match = [regex]::Match(
            $Value,
            '(?i)^(?<protocol>TCP|UDP) Query User\{(?<guid>[0-9a-f-]{36})\}(?<program>.+)$'
        )
        if (-not $match.Success) {
            return $Value
        }
        try {
            $identityProgram = [IO.Path]::GetFullPath(
                $match.Groups['program'].Value
            ).ToLowerInvariant()
        } catch {
            return $Value
        }
        if ($identityProgram -cne $program) {
            return $Value
        }
        return "$($match.Groups['protocol'].Value.ToUpperInvariant()) Query User{$($match.Groups['guid'].Value.ToUpperInvariant())}$program"
    }
    $signature = [ordered]@{
        RuleID = & $identity (ConvertTo-D5SemanticValue $Rule.RuleID)
        InstanceID = & $identity (ConvertTo-D5SemanticValue $Rule.InstanceID)
        Program = $program
        DisplayName = ConvertTo-D5SemanticValue $Rule.DisplayName
    }
    foreach ($field in $script:D5FirewallSemanticFields) {
        if ($field -notin @('RuleID', 'InstanceID', 'Program')) {
            $signature[$field] = ConvertTo-D5SemanticValue $Rule.$field
        }
    }
    return $signature | ConvertTo-Json -Compress
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
    $missingHistorySources = [Collections.Generic.List[string]]::new()
    $presentHistorySources = [Collections.Generic.List[string]]::new()
    foreach ($source in @($manifest.Sources)) {
        $normalizedRelativePath = ([string]$source.RelativePath).Replace('\', '/')
        $relativePath = $normalizedRelativePath.Replace(
            '/',
            [IO.Path]::DirectorySeparatorChar
        )
        $path = [IO.Path]::GetFullPath((Join-Path $root $relativePath))
        if (-not (Test-D5ProgramUnderRoot $path $root)) {
            throw "D5 firewall ownership source escapes the repository: $relativePath"
        }
        $isHistorySource = $normalizedRelativePath.StartsWith(
            $script:D5CleanupHistoryCorpusPrefix,
            [StringComparison]::Ordinal
        )
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            # Only the untracked history corpus may be absent; tracked sources
            # exist in every checkout, so their loss is always an error.
            if (-not $isHistorySource) {
                throw "D5 firewall ownership source is missing: $relativePath"
            }
            $missingHistorySources.Add($normalizedRelativePath)
            continue
        }
        if ($isHistorySource) {
            $presentHistorySources.Add($normalizedRelativePath)
        }
        $expectedHash = ([string]$source.SHA256).ToLowerInvariant()
        $actualHash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($expectedHash -notmatch $script:D5SHA256Pattern -or $actualHash -cne $expectedHash) {
            throw "D5 firewall ownership source hash changed: $relativePath"
        }
        $sourceIdentities.Add([pscustomobject][ordered]@{
            RelativePath = $normalizedRelativePath
            SHA256 = $actualHash
        })
    }
    # A surviving fragment proves this environment owns cleanup history, so a
    # missing member is lost forensic evidence, not a fresh environment.
    if ($missingHistorySources.Count -gt 0 -and $presentHistorySources.Count -gt 0) {
        throw ('D5 cleanup history corpus is partially present; missing: ' +
            ($missingHistorySources -join ', '))
    }
    $cleanupHistoryPresent = $missingHistorySources.Count -eq 0

    $candidateRelativePath = 'docs/.orchestration/firewall-cleanup-candidates.md'
    $candidateSources = @(
        $manifest.Sources |
            Where-Object { [string]$_.RelativePath -ceq $candidateRelativePath }
    )
    if ($candidateSources.Count -ne 1) {
        throw 'D5 firewall ownership evidence must pin one cleanup candidate document'
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
    $cleanupOwnedRuleCount = 0
    $cleanupOwnedProgramCount = 0
    $cleanupOwnedPayloadSHA256 = Get-D5OrdinalPayloadSHA256 @()
    if ($cleanupHistoryPresent) {
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
        $cleanupOwnedRuleCount = [int]$manifest.CleanupOwned.ExpectedRuleCount
        $cleanupOwnedProgramCount = [int]$manifest.CleanupOwned.ExpectedProgramCount
        $cleanupOwnedPayloadSHA256 = (
            [string]$manifest.CleanupOwned.ApprovedSemanticPayloadSHA256
        ).ToLowerInvariant()
    }

    $packageManifestPath = Join-Path $root 'scripts\d5-windows-network-packages.json'
    $networkManifest = Get-Content -LiteralPath $packageManifestPath -Raw | ConvertFrom-Json
    Assert-D5ExactPropertySet `
        $networkManifest `
        @('Packages', 'RetiredProgramTombstone', 'SchemaVersion') `
        'D5 network package manifest'
    if ([int]$networkManifest.SchemaVersion -ne $script:D5NetworkManifestSchemaVersion) {
        throw 'D5 network package manifest has an unsupported schema'
    }
    $tombstone = $networkManifest.RetiredProgramTombstone
    Assert-D5ExactPropertySet `
        $tombstone `
        @('Action', 'DisplayName', 'RelativeProgram', 'TCPGuid', 'UDPGuid') `
        'D5 retired connectivity tombstone'
    if ([string]$tombstone.RelativeProgram -cne $script:D5RetiredConnectivityRelativeProgram -or
        [string]$tombstone.DisplayName -cne $script:D5RetiredConnectivityDisplayName -or
        [string]$tombstone.Action -cne 'Block' -or
        [string]$tombstone.TCPGuid -notmatch $script:D5GUIDPattern -or
        [string]$tombstone.UDPGuid -notmatch $script:D5GUIDPattern -or
        [string]$tombstone.TCPGuid -ceq [string]$tombstone.UDPGuid) {
        throw 'D5 network package manifest altered the exact retired connectivity tombstone'
    }
    $packages = @($networkManifest.Packages)
    $stableRelativePrograms = [Collections.Generic.List[string]]::new()
    $packageNames = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $packagePaths = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($package in $packages) {
        Assert-D5ExactPropertySet $package @('Name', 'Path') 'D5 network package record'
        $name = [string]$package.Name
        $path = ([string]$package.Path).Replace('\', '/')
        if ($path.StartsWith('./', [StringComparison]::Ordinal)) {
            $path = $path.Substring(2)
        }
        if ($name -notmatch '^[a-z0-9][a-z0-9-]*$' -or
            [string]::IsNullOrWhiteSpace($path) -or
            $path.StartsWith('../', [StringComparison]::Ordinal) -or
            -not $packageNames.Add($name) -or
            -not $packagePaths.Add($path) -or
            "$name.test.exe" -ceq $script:D5RetiredConnectivityRelativeProgram) {
            throw "D5 package manifest has an invalid executable name: $name"
        }
        $stableRelativePrograms.Add("$name.test.exe")
    }
    if ($packages.Count -eq 0) {
        throw 'D5 network package manifest has no active packages'
    }
    foreach ($relativeProgram in @($manifest.D5.StableChildRelativePrograms)) {
        $normalized = ([string]$relativeProgram).Replace('\', '/')
        if ($normalized -ceq $script:D5RetiredConnectivityRelativeProgram) {
            throw 'D5 stable child manifest reintroduced the retired connectivity executable'
        }
        $stableRelativePrograms.Add($normalized)
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
        CleanupHistoryPresent = $cleanupHistoryPresent
        StableRelativePrograms = @($stableRelativePrograms | Sort-Object -Unique)
        D5HistoricalProgramSHA256 = $historicalHashes
        D5OwnedTemporaryPathPatterns = @($manifest.D5.OwnedTemporaryPathPatterns)
        CleanupOwnedProgramPaths = @($cleanupProgramPaths | Sort-Object)
        CleanupOwnedProgramRoots = @($cleanupProgramRoots | Sort-Object)
        CleanupOwnedProgramNames = @($cleanupProgramNames | Sort-Object)
        CleanupOwnedProgramSHA256 = @($cleanupProgramHashes | Sort-Object)
        CleanupOwnedRuleCount = $cleanupOwnedRuleCount
        CleanupOwnedProgramCount = $cleanupOwnedProgramCount
        CleanupOwnedSemanticPayloadSHA256 = $cleanupOwnedPayloadSHA256
        RetiredProgramTombstone = [pscustomobject][ordered]@{
            RelativeProgram = [string]$tombstone.RelativeProgram
            DisplayName = [string]$tombstone.DisplayName
            Action = [string]$tombstone.Action
            TCPGuid = [string]$tombstone.TCPGuid
            UDPGuid = [string]$tombstone.UDPGuid
        }
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

    $retiredEntry = $Evidence.RetiredProgramTombstone
    $retiredRelative = ([string]$retiredEntry.RelativeProgram).Replace(
        '/',
        [IO.Path]::DirectorySeparatorChar
    )
    $retiredProgram = [IO.Path]::GetFullPath((Join-Path $root $retiredRelative))
    if ([string]$retiredEntry.RelativeProgram -cne $script:D5RetiredConnectivityRelativeProgram -or
        -not (Test-D5ProgramUnderRoot $retiredProgram $root) -or
        $authorized.Contains($retiredProgram)) {
        throw 'D5 firewall ownership policy does not reserve the exact retired connectivity path'
    }
    $retiredPrograms = New-D5ProgramSet @($retiredProgram)
    $retiredNames = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    [void]$retiredNames.Add([IO.Path]::GetFileName($retiredProgram))
    $retiredRules = @(
        New-D5RetiredProgramTombstoneRule `
            ($retiredProgram.ToLowerInvariant()) `
            ([string]$retiredEntry.DisplayName) `
            ([string]$retiredEntry.Action) `
            'TCP' `
            ([string]$retiredEntry.TCPGuid)
        New-D5RetiredProgramTombstoneRule `
            ($retiredProgram.ToLowerInvariant()) `
            ([string]$retiredEntry.DisplayName) `
            ([string]$retiredEntry.Action) `
            'UDP' `
            ([string]$retiredEntry.UDPGuid)
    )
    $retiredRuleSignatures = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    $retiredRuleIdentities = [Collections.Generic.HashSet[string]]::new(
        [StringComparer]::OrdinalIgnoreCase
    )
    foreach ($rule in $retiredRules) {
        if (-not $retiredRuleSignatures.Add((ConvertTo-D5RetiredProgramRuleSignature $rule)) -or
            -not $retiredRuleIdentities.Add([string]$rule.InstanceID)) {
            throw 'D5 retired connectivity tombstone repeats a firewall identity'
        }
    }

    $stableNames = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($program in $evidencePrograms) {
        [void]$stableNames.Add([IO.Path]::GetFileName($program))
    }
    [void]$stableNames.Add([IO.Path]::GetFileName($retiredProgram))
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
        CleanupHistoryPresent = [bool]$Evidence.CleanupHistoryPresent
        D5ProgramNames = @($stableNames | Sort-Object)
        StableProgramNameSet = $stableNames
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
        RetiredProgram = $retiredProgram
        RetiredProgramSet = $retiredPrograms
        RetiredProgramNameSet = $retiredNames
        RetiredRules = $retiredRules
        RetiredRuleSignatureSet = $retiredRuleSignatures
        RetiredRuleIdentitySet = $retiredRuleIdentities
        EvidenceSources = @($Evidence.Sources)
    }
}

function Test-D5ProgramObserved([string]$Program, [object]$Policy) {
    foreach ($root in @($Policy.ObservedRoots)) {
        if (Test-D5ProgramUnderRoot $Program ([string]$root)) {
            return $true
        }
    }
    $fullProgram = [IO.Path]::GetFullPath($Program)
    return $Policy.RetiredProgramSet.Contains($fullProgram) -or
        $Policy.ExcludedProgramSet.Contains($fullProgram) -or
        (Test-D5ExternalStableProgramPath $fullProgram $Policy)
}

function Test-D5ExternalStableProgramPath([string]$Program, [object]$Policy) {
    $program = [IO.Path]::GetFullPath($Program)
    return -not (Test-D5ProgramUnderRoot $program $Policy.HarnessRoot) -and
        $Policy.StableProgramNameSet.Contains([IO.Path]::GetFileName($program))
}

function Test-D5ExternalStableProgramNameRule([object]$Rule, [object]$Policy) {
    return Test-D5ExternalStableProgramPath ([string]$Rule.Program) $Policy
}

function Select-D5ExactProcessIDs(
    [string]$ExpectedImagePath,
    [object[]]$Observations
) {
    foreach ($observation in $Observations) {
        if ([string]::IsNullOrWhiteSpace([string]$observation.ImagePath)) {
            throw "Cannot classify same-name PID $($observation.ProcessID) without an executable image path"
        }
    }
    return @(
        $Observations |
            Where-Object {
                [string]$_.ImagePath -and
                [string]$_.ImagePath -ieq $ExpectedImagePath
            } |
            ForEach-Object { [int]$_.ProcessID } |
            Sort-Object -Unique
    )
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

function Assert-D5RetiredProgramRuntimeStates(
    [object[]]$States,
    [object]$Policy,
    [string]$Phase
) {
    if ($States.Count -ne 1) {
        throw "$Phase must observe exactly one retired connectivity runtime state"
    }
    $state = $States[0]
    $program = [IO.Path]::GetFullPath([string]$state.Program)
    if (-not $Policy.RetiredProgramSet.Contains($program)) {
        throw "$Phase observed an unlisted retired program: $program"
    }
    $processProperty = $state.PSObject.Properties['ProcessIDs']
    if ($null -eq $processProperty) {
        throw "$Phase cannot prove the retired connectivity program has no matching process"
    }
    if ([bool]$state.Exists) {
        throw "$Phase found the retired connectivity executable reintroduced: $program"
    }
    if (@($processProperty.Value).Count -ne 0) {
        throw "$Phase found a live retired connectivity process: $program"
    }
}

function Assert-D5ProgramsExcludeRetiredTombstone(
    [string[]]$Programs,
    [object]$Policy,
    [string]$Context
) {
    foreach ($candidate in $Programs) {
        if ([string]::IsNullOrWhiteSpace($candidate)) {
            throw "$Context contains an empty executable path"
        }
        if ($candidate.IndexOfAny([char[]]'*?') -ge 0) {
            throw "$Context contains a wildcard executable path: $candidate"
        }
        $program = [IO.Path]::GetFullPath($candidate)
        if ($Policy.RetiredProgramSet.Contains($program) -or
            $Policy.RetiredProgramNameSet.Contains([IO.Path]::GetFileName($program))) {
            throw "$Context reintroduced the retired connectivity executable: $program"
        }
    }
}

function Assert-D5RetiredProgramRule(
    [object]$Rule,
    [object]$Policy,
    [string]$Phase
) {
    $program = [IO.Path]::GetFullPath([string]$Rule.Program)
    if (-not $Policy.RetiredProgramSet.Contains($program)) {
        throw "$Phase found an unlisted retired firewall program: $program"
    }
    $existsProperty = $Rule.PSObject.Properties['ProgramExists']
    $hashProperty = $Rule.PSObject.Properties['ProgramSHA256']
    $processProperty = $Rule.PSObject.Properties['ProgramProcessIDs']
    if ($null -eq $existsProperty -or $null -eq $hashProperty -or $null -eq $processProperty) {
        throw "$Phase lacks inert runtime evidence for the retired connectivity rule"
    }
    if ([bool]$existsProperty.Value -or
        -not [string]::IsNullOrEmpty([string]$hashProperty.Value)) {
        throw "$Phase found the retired connectivity executable reintroduced: $program"
    }
    if (@($processProperty.Value).Count -ne 0) {
        throw "$Phase found a live retired connectivity process: $program"
    }
    $signature = ConvertTo-D5RetiredProgramRuleSignature $Rule
    if (-not $Policy.RetiredRuleSignatureSet.Contains($signature)) {
        throw "$Phase found an altered retired connectivity firewall rule: $signature"
    }
}

function Assert-D5RetiredProgramRuleSet(
    [object[]]$Rules,
    [object]$Policy,
    [string]$Phase
) {
    # Zero rules means the host owner removed the obsolete pair. Any surviving
    # fragment must remain the exact historical pair; accepting a half-pair
    # would turn the tombstone into a general stable-root allowlist.
    if ($Rules.Count -eq 0) {
        return
    }
    if ($Rules.Count -ne $Policy.RetiredRuleSignatureSet.Count) {
        throw "$Phase requires either zero retired rules or the exact retired TCP/UDP pair"
    }
    $actual = @($Rules | ForEach-Object {
        Assert-D5RetiredProgramRule $_ $Policy $Phase
        ConvertTo-D5RetiredProgramRuleSignature $_
    })
    if (-not (Test-D5OrdinalStringSetsEqual @($Policy.RetiredRuleSignatureSet) $actual)) {
        throw "$Phase altered the exact retired connectivity TCP/UDP pair"
    }
}

function New-D5FirewallOwnershipSnapshot([object[]]$Rules, [object]$Policy) {
    $stableRules = [Collections.Generic.List[object]]::new()
    $retiredRules = [Collections.Generic.List[object]]::new()
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
        if ($Policy.RetiredProgramSet.Contains($program)) {
            Assert-D5RetiredProgramRule $rule $Policy 'Preflight'
            $state = Get-D5RuleProgramState $rule
            $stateSignature = ConvertTo-D5ProgramStateSignature `
                $state.Program `
                $state.Exists `
                $state.SHA256
            if ($excludedProgramStates.ContainsKey($program) -and
                [string]$excludedProgramStates[$program] -cne $stateSignature) {
                throw "Firewall observations disagree about executable state for $program"
            }
            $excludedProgramStates[$program] = $stateSignature
            $retiredRules.Add($rule)
            continue
        }
        if (Test-D5ProgramUnderRoot $program $Policy.HarnessRoot) {
            throw "Preflight found an unmanifested program under the stable harness root: $program"
        }

        # Outside-root executable state has no ownership meaning. Record only
        # the normalized program identity so an unreadable or stale host binary
        # cannot become a preflight dependency; ActiveStore debt owns all dynamic
        # rule telemetry and its limits.
        $stateSignature = ConvertTo-D5ProgramStateSignature $program $false ''
        $excludedProgramStates[$program] = $stateSignature

        $signature = ConvertTo-D5RuleSignature $rule
        # Outside the canonical harness namespace, names, historical hashes and
        # cleanup provenance are observational metadata only. Exact historical
        # pins retain their classification when unchanged, but neither a pin nor
        # a same-name executable can acquire D5 ownership.
        if ($Policy.ExcludedProgramSet.Contains($program) -and
            $Policy.ExcludedRuleSignatureSet.Contains($signature)) {
            $pinnedRules.Add($rule)
            continue
        }
        $unrelatedRules.Add($rule)
    }

    Assert-D5RetiredProgramRuleSet @($retiredRules) $Policy 'Preflight'

    $excludedRules = @($retiredRules) + @($pinnedRules) + @($unrelatedRules)
    $ruleSignatures = @($excludedRules | ForEach-Object { ConvertTo-D5RuleSignature $_ })
    $programStateSignatures = @($excludedProgramStates.Values | ForEach-Object { [string]$_ })
    $excludedPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $pinnedPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $retiredPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $unrelatedPrograms = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($rule in $excludedRules) {
        [void]$excludedPrograms.Add([IO.Path]::GetFullPath([string]$rule.Program))
    }
    foreach ($rule in $pinnedRules) {
        [void]$pinnedPrograms.Add([IO.Path]::GetFullPath([string]$rule.Program))
    }
    foreach ($rule in $retiredRules) {
        [void]$retiredPrograms.Add([IO.Path]::GetFullPath([string]$rule.Program))
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
        RetiredTombstoneRuleCount = $retiredRules.Count
        RetiredTombstoneProgramCount = $retiredPrograms.Count
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
        RetiredTombstoneRules = @($retiredRules)
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
    # Rule classifications are disjoint, but a host-owned program may have one
    # historical pinned rule and one drifted rule. Program telemetry therefore
    # overlaps by design and must not be treated as an ownership partition.
    if ([int]$State.RetiredTombstoneRuleCount +
            [int]$State.PinnedExcludedRuleCount +
            [int]$State.UnrelatedExcludedRuleCount -ne
            [int]$State.ExcludedRuleCount -or
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
    # Only durable policy provenance and the inert stable-root tombstone belong
    # to this fail-closed baseline. Outside-root observations remain in both
    # snapshots for evidence, but their identities and executable state are
    # governed exclusively by bounded ActiveStore telemetry.
    $stableFields = @(
        'EvidenceRuleCount',
        'EvidenceProgramCount',
        'EvidenceSemanticPayloadSHA256',
        'EvidenceExecutableStateProvenanceSHA256',
        'RetiredTombstoneRuleCount',
        'RetiredTombstoneProgramCount'
    )
    foreach ($field in $stableFields) {
        if ([string]$Baseline.$field -cne [string]$current.$field) {
            throw "$Phase changed stable firewall ownership field $field"
        }
    }
}
