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
        if ($index.ContainsKey($filter.InstanceID)) { throw 'Duplicate effective firewall filter identity.' }
        $index[$filter.InstanceID] = $filter
    }
    return $index
}
function Require-Filter($index, $rule) {
    if (-not $index.ContainsKey($rule.InstanceID)) { throw "Missing effective firewall filter: $($rule.Name)" }
    return $index[$rule.InstanceID]
}

$localAddresses = @(Get-NetIPAddress -ErrorAction Stop)
$networks = @(Get-NetConnectionProfile -ErrorAction Stop | ForEach-Object {
    $connection = $_
    @{ Interface = [string]$connection.InterfaceAlias; Profile = Convert-Profile $connection.NetworkCategory;
        LocalAddresses = @($localAddresses | Where-Object { $_.InterfaceIndex -eq $connection.InterfaceIndex } | ForEach-Object { [string]$_.IPAddress }) }
})
$profiles = @(Get-NetFirewallProfile -PolicyStore ActiveStore -ErrorAction Stop | ForEach-Object {
    @{ Profile = Convert-Profile $_.Name; Enabled = Convert-Setting $_.Enabled;
        AllowInboundRules = Convert-Setting $_.AllowInboundRules;
        AllowLocalRules = Convert-Setting $_.AllowLocalFirewallRules;
        DefaultInbound = Convert-Action $_.DefaultInboundAction;
        DisabledInterfaces = @($_.DisabledInterfaceAliases | Where-Object { $_ }) }
})
# ActiveStore is the resultant policy, including GPO. Restrict associated filter
# reads to this process; unrelated protected rules can deny whole-store queries.
$applications = Index-Filters @(Get-NetFirewallApplicationFilter -PolicyStore ActiveStore -ErrorAction Stop)
$candidates = @(Get-NetFirewallRule -PolicyStore ActiveStore -Enabled True -Direction Inbound -ErrorAction Stop | Where-Object {
    $app = Require-Filter $applications $_
    $program = [Environment]::ExpandEnvironmentVariables([string]$app.Program)
    ($program -ieq $Executable -or $program -eq 'Any') -and
        ([string]::IsNullOrEmpty([string]$app.Package) -or $app.Package -eq 'Any') -and
        (-not $_.PSObject.Properties['PackageFamilyName'] -or [string]::IsNullOrEmpty([string]$_.PackageFamilyName))
})
$rules = @()
if ($candidates.Count -gt 0) {
    $services = Index-Filters @($candidates | Get-NetFirewallServiceFilter -ErrorAction Stop)
    $candidates = @($candidates | Where-Object { Test-Any (Require-Filter $services $_).Service })
    $ports = Index-Filters @($candidates | Get-NetFirewallPortFilter -ErrorAction Stop)
    $addresses = Index-Filters @($candidates | Get-NetFirewallAddressFilter -ErrorAction Stop)
    $interfaces = Index-Filters @($candidates | Get-NetFirewallInterfaceFilter -ErrorAction Stop)
    $types = Index-Filters @($candidates | Get-NetFirewallInterfaceTypeFilter -ErrorAction Stop)
    $security = Index-Filters @($candidates | Get-NetFirewallSecurityFilter -ErrorAction Stop)
    $rules = @(foreach ($rule in $candidates) {
        # wind is an ordinary executable, not a Windows service or packaged app.
        $service = Require-Filter $services $rule
        if (-not (Test-Any $service.Service)) { continue }
        $port = Require-Filter $ports $rule
        $protocol = switch ([string]$port.Protocol) {
            'UDP' { 1 }; '17' { 1 }; 'TCP' { 2 }; '6' { 2 }; 'Any' { 0 }; '256' { 0 }; default { -1 }
        }
        if ($protocol -lt 0) { continue }
        $address = Require-Filter $addresses $rule
        $interface = Require-Filter $interfaces $rule
        $type = Require-Filter $types $rule
        $secure = Require-Filter $security $rule
        $restrictions = @(
            foreach ($field in @('LocalPort', 'RemotePort')) {
                if (-not (Test-Any $port.$field)) { "$field=$($port.$field -join ',')" }
            }
            foreach ($field in @('RemoteAddress')) {
                if (-not (Test-Any $address.$field)) { "$field=$($address.$field -join ',')" }
            }
            foreach ($field in @('LocalUser', 'RemoteUser', 'RemoteMachine')) {
                if (-not (Test-Any $secure.$field)) { "$field=$($secure.$field -join ',')" }
            }
            if ([string]$secure.Authentication -ne 'NotRequired') { "Authentication=$($secure.Authentication)" }
            if ([string]$secure.Encryption -ne 'NotRequired') { "Encryption=$($secure.Encryption)" }
            if ($secure.OverrideBlockRules) { 'OverrideBlockRules=True' }
            if ([string]$type.InterfaceType -ne 'Any') { "InterfaceType=$($type.InterfaceType)" }
            if ([string]$port.DynamicTarget -ne 'Any') { "DynamicTarget=$($port.DynamicTarget)" }
            if ([string]$rule.PrimaryStatus -ne 'OK') { "Status=$($rule.PrimaryStatus)" }
            if ($rule.PSObject.Properties['PolicyAppId'] -and $rule.PolicyAppId) { "PolicyAppId=$($rule.PolicyAppId)" }
            if ($rule.PSObject.Properties['RemoteDynamicKeywordAddresses'] -and $rule.RemoteDynamicKeywordAddresses) { 'RemoteDynamicKeywordAddresses' }
            if ($rule.Owner) { "Owner=$($rule.Owner)" }
        )
        $scope = @(foreach ($profile in @(1, 2, 3)) {
            $mask = 1 -shl ($profile - 1)
            if ([int]$rule.Profile -eq 0 -or ([int]$rule.Profile -band $mask) -ne 0) { $profile }
        })
        $aliases = @()
        if (-not (Test-Any $interface.InterfaceAlias)) { $aliases = @($interface.InterfaceAlias) }
        $localScope = @()
        $scopeDescription = $restrictions
        if (-not (Test-Any $address.LocalAddress)) {
            $localScope = @($address.LocalAddress)
            $scopeDescription += "LocalAddress=$($localScope -join ',')"
        }
        @{ Name = [string]$rule.Name; Profiles = $scope; Protocol = $protocol;
            Action = Convert-Action $rule.Action; Local = [string]$rule.PolicyStoreSourceType -eq 'Local';
            Conditional = $restrictions.Count -gt 0; Restrictions = $scopeDescription -join '; '; Interfaces = $aliases;
            LocalAddresses = $localScope; OverridesBlocks = [bool]$secure.OverrideBlockRules }
    })
}
@{ Networks = $networks; Profiles = $profiles; Rules = $rules } | ConvertTo-Json -Depth 8 -Compress
