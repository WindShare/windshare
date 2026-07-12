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
$durableEvidence = Import-D5FirewallOwnershipEvidence `
    $repositoryRoot `
    (Join-Path $PSScriptRoot 'd5-windows-firewall-ownership.json')
if ($durableEvidence.CleanupOwnedRuleCount -ne 628 -or
    $durableEvidence.CleanupOwnedProgramCount -ne 314 -or
    $durableEvidence.CleanupOwnedSemanticPayloadSHA256 -cne
        'a8e826e7c86fe6a0efd1bb57e074e0d4de364ad7846dace5ff5c4433f2b2ce0b' -or
    $durableEvidence.ExcludedRuleCount -ne 60 -or
    $durableEvidence.ExcludedProgramCount -ne 30 -or
    $durableEvidence.ExcludedSemanticPayloadSHA256 -cne
        'b631b4814182f302ea2bf5d0680f507187f9a602b7ad3d892147ff9a73031d2f') {
    throw 'Durable D5 firewall ownership evidence did not reconstruct its approved payloads'
}

function New-Rule([hashtable]$Override = @{}) {
    $values = [ordered]@{
        RuleID = 'rule-1'
        InstanceID = 'TCP Query User{01234567-89AB-CDEF-0123-456789ABCDEF}C:\Temp\go-build123\webrtc.test.exe'
        Program = 'C:\Temp\go-build123\webrtc.test.exe'
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
    }
    foreach ($key in $Override.Keys) {
        $values[$key] = $Override[$key]
    }
    return [pscustomobject]$values
}

$harnessRoot = 'C:\repo\tmp\d5-harness'
$program = 'C:\repo\tmp\d5-harness\webrtc.test.exe'
$unselectedProgram = 'C:\repo\tmp\d5-harness\hostile-sender.exe'

function New-TestEvidence(
    [object[]]$ExcludedRules = @(),
    [object[]]$ExcludedProgramStates = @(),
    [string[]]$D5Hashes = @(),
    [string[]]$D5PathPatterns = @(),
    [string[]]$CleanupOwnedRoots = @()
) {
    return [pscustomobject]@{
        Sources = @()
        StableRelativePrograms = @('hostile-sender.exe', 'webrtc.test.exe')
        D5HistoricalProgramSHA256 = $D5Hashes
        D5OwnedTemporaryPathPatterns = $D5PathPatterns
        CleanupOwnedProgramPaths = @()
        CleanupOwnedProgramRoots = $CleanupOwnedRoots
        CleanupOwnedProgramNames = @()
        CleanupOwnedProgramSHA256 = @()
        CleanupOwnedRuleCount = 0
        CleanupOwnedProgramCount = 0
        CleanupOwnedSemanticPayloadSHA256 = Get-D5OrdinalPayloadSHA256 @()
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
Assert-Throws {
    Assert-D5FirewallPreflight @($temporaryRule) $policy $emptyOwnershipBaseline
} 'WindShare-attributable random/temp firewall program'
$zero = [pscustomobject]@{
    BeforeRules = @()
    AfterRules = @()
    NewRelevantEvents = @()
}
Assert-D5FirewallPreflight @() $policy $emptyOwnershipBaseline
Assert-D5FirewallUnchanged $zero $policy $emptyOwnershipBaseline

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
    $unexpected = $broadHarnessRule.PSObject.Copy()
    $unexpected.Program = 'C:\repo\tmp\d5-harness\connectivity.test.exe'
    Assert-D5FirewallPreflight @($unexpected) $policy $emptyOwnershipBaseline
} 'unmanifested program under the stable harness root'

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
Assert-D5ColdFirewallRegistration `
    $cold `
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
Assert-D5ColdFirewallRegistration `
    $allowCold `
    @($program) `
    $policy `
    $emptyOwnershipBaseline

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
    @{ Field = 'Action'; Value = 'Allow'; Error = 'changed firewall rule identity or semantics' },
    @{ Field = 'Profile'; Value = @('Private'); Error = 'exact bounded Query User shape' },
    @{ Field = 'LocalPort'; Value = '443'; Error = 'exact bounded Query User shape' },
    @{ Field = 'RemotePort'; Value = '443'; Error = 'exact bounded Query User shape' },
    @{ Field = 'LocalAddress'; Value = '127.0.0.1'; Error = 'exact bounded Query User shape' },
    @{ Field = 'RemoteAddress'; Value = '127.0.0.1'; Error = 'exact bounded Query User shape' },
    @{ Field = 'Program'; Value = 'C:\Temp\go-build456\webrtc.test.exe'; Error = 'WindShare-attributable random/temp firewall program' }
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
} 'emitted a relevant firewall rule event'

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
Assert-Throws {
    New-D5FirewallOwnershipBaseline `
        @($spoofedPinnedTCP, $spoofedPinnedUDP) `
        $excludedPolicy
} 'WindShare-attributable random/temp firewall program'

$spoofedHashRule = New-Rule @{
    RuleID = 'spoofed-hash'
    InstanceID = 'spoofed-hash'
    Program = 'C:\Temp\go-build901\b001\exe\justus-go.exe'
    ProgramExists = $true
    ProgramSHA256 = $d5Hash
}
Assert-Throws {
    New-D5FirewallOwnershipBaseline `
        @($excludedTCP, $excludedUDP, $spoofedHashRule) `
        $excludedPolicy
} 'WindShare-attributable random/temp firewall program'

$spoofedPathRule = New-Rule @{
    RuleID = 'spoofed-path'
    InstanceID = 'spoofed-path'
    Program = 'C:\Temp\go-build777\b001\exe\justus-go.exe'
}
Assert-Throws {
    New-D5FirewallOwnershipBaseline `
        @($excludedTCP, $excludedUDP, $spoofedPathRule) `
        $excludedPolicy
} 'WindShare-attributable random/temp firewall program'

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
Assert-Throws {
    Assert-D5FirewallUnchanged $unrelatedMutation $excludedPolicy $unrelatedBaseline
} 'changed excluded firewall ownership field|changed excluded firewall identities'

$concurrentGrowthRule = New-Rule @{
    RuleID = 'concurrent-growth'
    InstanceID = 'concurrent-growth'
    Program = 'C:\Temp\go-build903\b001\exe\other-tool.exe'
}
$concurrentGrowth = [pscustomobject]@{
    BeforeRules = $unrelatedRules
    AfterRules = @($unrelatedRules + @($concurrentGrowthRule))
    NewRelevantEvents = @([pscustomobject]@{ EventID = 2097 })
}
Assert-Throws {
    Assert-D5FirewallUnchanged $concurrentGrowth $excludedPolicy $unrelatedBaseline
} 'changed excluded firewall ownership field|changed excluded firewall identities'

$d5TemporaryRule = New-Rule @{
    RuleID = 'd5-temp'
    InstanceID = 'd5-temp'
    Program = 'C:\Temp\go-build904\b001\exe\webrtc.test.exe'
}
Assert-Throws {
    New-D5FirewallOwnershipBaseline `
        @($excludedTCP, $excludedUDP, $d5TemporaryRule) `
        $excludedPolicy
} 'WindShare-attributable random/temp firewall program'

Write-Output 'D5 firewall semantic policy tests PASS'
