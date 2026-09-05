Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'firewall.psm1') -Force
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-setup-test-' + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($testRoot) | Out-Null
try {
    $executable = Join-Path $testRoot 'installed app\wind.exe'
    $statusPath = Join-Path $testRoot 'state\connectivity-setup.json'
    $plans = @(Get-WindShareFirewallPlan $executable)
    $plan = $plans[0]
    if ($plans.Count -ne 2 -or $plans[1].Protocol -ne 'TCP') { throw 'Proven TCP rule missing.' }
    if ($plan.Program -ne $executable -or $plan.Protocol -ne 'UDP' -or $plan.Direction -ne 'Inbound' -or
        $plan.Group -ne 'WindShare installation' -or $plan.ContainsKey('LocalPort')) { throw 'Invalid application-scoped rule plan.' }
    if (@(Get-WindShareFirewallPlan $executable)[0].Name -ne $plan.Name) { throw 'Unstable owned rule identity.' }
    if (@(Get-WindShareFirewallPlan (Join-Path $testRoot 'other\wind.exe'))[0].Name -eq $plan.Name) { throw 'Different installations share rule identity.' }
    $script:applied = 0
    $apply = { param($rules) if ($rules[0].Name -ne $plan.Name -or $rules.Count -ne 2) { throw 'Unexpected rule.' }; $script:applied++ }
    $skip = Invoke-WindShareFirewallSetup $executable $statusPath Skip -Apply $apply
    if ($skip.state -ne 'declined' -or $script:applied -ne 0) { throw 'Skip invoked firewall command.' }
    $configured = Invoke-WindShareFirewallSetup $executable $statusPath Configure -Apply $apply
    if ($configured.state -ne 'configured' -or $script:applied -ne 1) { throw 'Configure did not use injected command.' }
    $saved = Get-Content -LiteralPath $statusPath -Raw | ConvertFrom-Json
    if ($saved.executable -ne $executable -or $saved.schema -ne 1 -or $saved.state -ne 'configured') { throw 'Missing durable state.' }
    $skippedUpgrade = Invoke-WindShareFirewallSetup $executable $statusPath Skip -Apply $apply
    if ($skippedUpgrade.state -ne 'configured' -or $script:applied -ne 1) { throw 'Skipped upgrade changed existing firewall authority.' }
    $denied = Invoke-WindShareFirewallSetup $executable $statusPath Configure -Apply { throw 'fake policy denied' } -WarningAction SilentlyContinue
    if ($denied.state -ne 'unavailable') { throw 'Denied firewall is not a diagnostic outcome.' }
    $script:removeCount = 0
    $removed = Invoke-WindShareFirewallSetup $executable $statusPath Remove -Remove { param($rule) $script:removeCount++ }
    if ($removed.reason -ne 'rule-removed' -or $script:removeCount -ne 1) { throw 'Owned removal failed.' }
} finally {
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $ownedPrefix = Join-Path ([IO.Path]::GetFullPath([IO.Path]::GetTempPath())) 'windshare-setup-test-'
    if (-not $resolved.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Unowned test path.' }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
Write-Output 'Windows setup tests: PASS (fake firewall commands only)'
