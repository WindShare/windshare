Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$script:OwnedGroup = 'WindShare installation'

function Get-WindShareFirewallPlan {
    param([Parameter(Mandatory)][string]$Executable)
    $path = [IO.Path]::GetFullPath($Executable)
    $digest = [Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($path.ToLowerInvariant()))
    $identity = [Convert]::ToHexString($digest).Substring(0, 16).ToLowerInvariant()
    # The rule follows the installed application across ephemeral ICE ports.
    # The pinned Windows provider has authenticated TCP4/TCP6 payload proof.
    foreach ($protocol in @('UDP', 'TCP')) {
        @{
            Name = "WindShare-$identity-$protocol"
            DisplayName = "WindShare direct connections ($protocol)"
            Group = $script:OwnedGroup
            Program = $path
            Direction = 'Inbound'
            Action = 'Allow'
            Protocol = $protocol
            Profile = 'Any'
            Enabled = 'True'
        }
    }
}

function Remove-WindShareFirewallRule {
    param([Parameter(Mandatory)][hashtable]$Plan)
    $rules = @(Get-NetFirewallRule -PolicyStore PersistentStore -ErrorAction Stop |
        Where-Object { $_.Name -ceq $Plan.Name })
    foreach ($rule in $rules) {
        $applications = @($rule | Get-NetFirewallApplicationFilter)
        if ($rule.Group -cne $script:OwnedGroup -or $applications.Count -ne 1 -or
            $applications[0].Program -ine $Plan.Program) {
            throw 'The installation rule name belongs to a different owner or executable.'
        }
        $rule | Remove-NetFirewallRule -ErrorAction Stop
    }
}

function Invoke-WindShareFirewallSetup {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string]$StatusPath,
        [Parameter(Mandatory)][ValidateSet('Configure', 'Skip', 'Remove')][string]$Choice,
        [scriptblock]$Apply = { param($plans) foreach ($plan in $plans) { Remove-WindShareFirewallRule -Plan $plan; New-NetFirewallRule @plan -ErrorAction Stop | Out-Null } },
        [scriptblock]$Remove = { param($plans) foreach ($plan in $plans) { Remove-WindShareFirewallRule -Plan $plan } }
    )
    $plans = @(Get-WindShareFirewallPlan -Executable $Executable)
    $status = [ordered]@{ schema = 1; state = 'unavailable'; reason = ''; executable = $plans[0].Program }
    if ($Choice -eq 'Skip' -and (Test-Path -LiteralPath $StatusPath)) {
        try {
            $previous = Get-Content -LiteralPath $StatusPath -Raw | ConvertFrom-Json
            if ($previous.schema -eq 1 -and $previous.state -eq 'configured' -and
                $previous.executable -ieq $plans[0].Program) {
                # Skipping setup cannot revoke a rule that remains installed.
                return $previous
            }
        } catch { }
    }
    try {
        if ($Choice -eq 'Skip') {
            $status.state = 'declined'
            $status.reason = 'user-skipped'
        } elseif ($Choice -eq 'Remove') {
            & $Remove $plans
            $status.reason = 'rule-removed'
        } else {
            & $Apply $plans
            $status.state = 'configured'
            $status.reason = 'application-udp-tcp-rules-created'
        }
    } catch {
        # Managed policy and missing permissions are diagnostic outcomes; they
        # must not trigger elevation or turn installation into a sharing failure.
        $status.reason = 'firewall-command-unavailable-or-denied'
        Write-Warning "Firewall setup unavailable: $($_.Exception.Message). Sharing can continue through relay."
    }
    $parent = Split-Path -Parent ([IO.Path]::GetFullPath($StatusPath))
    [IO.Directory]::CreateDirectory($parent) | Out-Null
    $temporary = Join-Path $parent ([Guid]::NewGuid().ToString('N') + '.json')
    try {
        [IO.File]::WriteAllText($temporary, ($status | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
        [IO.File]::Move($temporary, $StatusPath, $true)
    } finally {
        if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
    }
    return [pscustomobject]$status
}

Export-ModuleMember -Function Get-WindShareFirewallPlan, Invoke-WindShareFirewallSetup
