Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)

function Convert-Setting($value) {
    switch ([string]$value) { 'True' { 1 }; 'False' { 2 }; default { 0 } }
}
function Convert-Profile($value) {
    switch ([string]$value) { 'Domain' { 1 }; 'DomainAuthenticated' { 1 }; 'Private' { 2 }; 'Public' { 3 }; default { 0 } }
}
function Convert-Action($value) {
    switch ([string]$value) { 'Allow' { 1 }; 'Block' { 2 }; default { 0 } }
}
function Test-Any($value) {
    $values = @($value)
    return $values.Count -eq 1 -and [string]$values[0] -eq 'Any'
}
function Index-Filters($filters) {
    $index = @{}
    foreach ($filter in $filters) {
        if ($index.ContainsKey($filter.InstanceID)) { throw 'Duplicate executable firewall filter identity.' }
        $index[$filter.InstanceID] = $filter
    }
    return $index
}
function Require-Filter($index, $rule) {
    if (-not $index.ContainsKey($rule.InstanceID)) { throw "Missing executable firewall filter: $($rule.Name)" }
    return $index[$rule.InstanceID]
}

$networks = @(Get-NetConnectionProfile -ErrorAction Stop | ForEach-Object {
    @{ Interface = [string]$_.InterfaceAlias; Profile = Convert-Profile $_.NetworkCategory }
})
$activeProfiles = @($networks | ForEach-Object { $_.Profile })
$profiles = @(Get-NetFirewallProfile -PolicyStore ActiveStore -ErrorAction Stop | Where-Object {
    (Convert-Profile $_.Name) -in $activeProfiles
} | ForEach-Object {
    @{ Profile = Convert-Profile $_.Name; Enabled = Convert-Setting $_.Enabled;
        AllowInboundRules = Convert-Setting $_.AllowInboundRules;
        AllowLocalRules = Convert-Setting $_.AllowLocalFirewallRules;
        DefaultInbound = Convert-Action $_.DefaultInboundAction;
        DisabledInterfaces = @($_.DisabledInterfaceAliases | Where-Object { $_ }) }
})
# Query the executable in the provider, not an all-program rule inventory. A
# missing application filter is ordinary first setup; all other read errors fail.
$queryErrors = @()
$applications = @(Get-NetFirewallApplicationFilter -PolicyStore ActiveStore -Program ([WildcardPattern]::Escape($Executable)) -ErrorAction SilentlyContinue -ErrorVariable queryErrors)
foreach ($failure in $queryErrors) {
    if ($failure.FullyQualifiedErrorId -notlike 'CmdletizationQuery_NotFound*') { throw $failure }
}
$applications = @($applications | Where-Object {
    [string]$_.Program -ieq $Executable -and
        ([string]::IsNullOrEmpty([string]$_.Package) -or $_.Package -eq 'Any')
})
$candidates = @()
if ($applications.Count -gt 0) {
    $candidates = @($applications | Get-NetFirewallRule -PolicyStore ActiveStore -ErrorAction Stop | Where-Object {
        [string]$_.Enabled -eq 'True' -and [string]$_.Direction -eq 'Inbound' -and
            (-not $_.PSObject.Properties['PackageFamilyName'] -or [string]::IsNullOrEmpty([string]$_.PackageFamilyName))
    })
}
$rules = @()
if ($candidates.Count -gt 0) {
    $services = Index-Filters @($candidates | Get-NetFirewallServiceFilter -ErrorAction Stop)
    $candidates = @($candidates | Where-Object { Test-Any (Require-Filter $services $_).Service })
}
if ($candidates.Count -gt 0) {
    $ports = Index-Filters @($candidates | Get-NetFirewallPortFilter -ErrorAction Stop)
    $addresses = Index-Filters @($candidates | Get-NetFirewallAddressFilter -ErrorAction Stop)
    $interfaces = Index-Filters @($candidates | Get-NetFirewallInterfaceFilter -ErrorAction Stop)
    $types = Index-Filters @($candidates | Get-NetFirewallInterfaceTypeFilter -ErrorAction Stop)
    $security = Index-Filters @($candidates | Get-NetFirewallSecurityFilter -ErrorAction Stop)
    $rules = @(foreach ($rule in $candidates) {
        $port = Require-Filter $ports $rule
        $protocol = switch ([string]$port.Protocol) {
            'UDP' { 1 }; '17' { 1 }; 'TCP' { 2 }; '6' { 2 }; 'Any' { 0 }; '256' { 0 }; default { -1 }
        }
        if ($protocol -lt 0) { continue }
        $address = Require-Filter $addresses $rule
        $interface = Require-Filter $interfaces $rule
        $type = Require-Filter $types $rule
        $secure = Require-Filter $security $rule
        # Constraints are evidence, not a request to reimplement Windows matching.
        $restrictions = @(
            foreach ($field in @('LocalPort', 'RemotePort', 'DynamicTarget')) {
                if (-not (Test-Any $port.$field)) { "$field=$($port.$field -join ',')" }
            }
            foreach ($field in @('LocalAddress', 'RemoteAddress')) {
                if (-not (Test-Any $address.$field)) { "$field=$($address.$field -join ',')" }
            }
            foreach ($field in @('LocalUser', 'RemoteUser', 'RemoteMachine')) {
                if (-not (Test-Any $secure.$field)) { "$field=$($secure.$field -join ',')" }
            }
            if (-not (Test-Any $interface.InterfaceAlias)) { "Interface=$($interface.InterfaceAlias -join ',')" }
            if ([string]$type.InterfaceType -ne 'Any') { "InterfaceType=$($type.InterfaceType)" }
            if ([string]$secure.Authentication -ne 'NotRequired') { "Authentication=$($secure.Authentication)" }
            if ([string]$secure.Encryption -ne 'NotRequired') { "Encryption=$($secure.Encryption)" }
            if ($secure.OverrideBlockRules) { 'OverrideBlockRules=True' }
            if ([string]$rule.PrimaryStatus -ne 'OK') { "Status=$($rule.PrimaryStatus)" }
            if ($rule.PSObject.Properties['PolicyAppId'] -and $rule.PolicyAppId) { "PolicyAppId=$($rule.PolicyAppId)" }
            if ($rule.PSObject.Properties['RemoteDynamicKeywordAddresses'] -and $rule.RemoteDynamicKeywordAddresses) { 'RemoteDynamicKeywordAddresses' }
            if ($rule.Owner) { "Owner=$($rule.Owner)" }
        )
        $scope = @(foreach ($profile in @(1, 2, 3)) {
            $mask = 1 -shl ($profile - 1)
            if ([int]$rule.Profile -eq 0 -or ([int]$rule.Profile -band $mask) -ne 0) { $profile }
        })
        @{ Name = [string]$rule.Name; Profiles = $scope; Protocol = $protocol;
            Action = Convert-Action $rule.Action; Local = [string]$rule.PolicyStoreSourceType -eq 'Local';
            Conditional = $restrictions.Count -gt 0; Restrictions = $restrictions -join '; ' }
    })
}
@{ Networks = $networks; Profiles = $profiles; Rules = $rules } | ConvertTo-Json -Depth 8 -Compress
