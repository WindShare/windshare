Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$ownedGroup = 'WindShare installation'
$resultCode = 0
$detail = ''
try {
    $path = [IO.Path]::GetFullPath($Executable)
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { $digest = $hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($path.ToLowerInvariant())) }
    finally { $hasher.Dispose() }
    $identity = ([BitConverter]::ToString($digest)).Replace('-', '').Substring(0, 16).ToLowerInvariant()
    $plans = @('UDP', 'TCP') | ForEach-Object {
        @{ Name = "WindShare-$identity-$_"; DisplayName = "WindShare direct connections ($_)";
            Group = $ownedGroup; Program = $path; Direction = 'Inbound'; Action = 'Allow';
            Protocol = $_; Profile = 'Any'; Enabled = 'True'; PolicyStore = 'PersistentStore' }
    }
    $existing = @(Get-NetFirewallRule -PolicyStore PersistentStore -ErrorAction Stop)
    # Validate both owners before changing either rule. Similar display names are
    # not authority to edit another application's or an administrator's policy.
    foreach ($plan in $plans) {
        foreach ($rule in @($existing | Where-Object { $_.Name -ieq $plan.Name })) {
            $application = @($rule | Get-NetFirewallApplicationFilter -ErrorAction Stop)
            if ($rule.Group -cne $ownedGroup -or $application.Count -ne 1 -or $application[0].Program -ine $path) {
                $resultCode = 10
                throw 'Firewall rule ownership conflict.'
            }
        }
    }
    foreach ($plan in $plans) {
        $owned = @($existing | Where-Object { $_.Name -ieq $plan.Name })
        if ($owned.Count -gt 0) { $owned | Remove-NetFirewallRule -ErrorAction Stop }
        # Replacing the owned definition also removes stale port, address, service,
        # and security restrictions that Set-NetFirewallRule would otherwise retain.
        if (-not $Remove) { New-NetFirewallRule @plan -ErrorAction Stop | Out-Null }
    }
} catch {
    if ($resultCode -eq 0) {
        $resultCode = if ($_.Exception -is [UnauthorizedAccessException]) { 5 } else { 1 }
    }
    $detail = $_.Exception.Message
}
$result = @{ Code = $resultCode; Detail = $detail } | ConvertTo-Json -Compress
# The elevated writer reports through a parent-owned pipe, never a privileged
# write to a caller-controlled file path. Preferences stay in the ordinary process.
if (Test-Path variable:ResultPipe) {
	$resultConnectTimeoutMilliseconds = 5000
    $pipe = [IO.Pipes.NamedPipeClientStream]::new('.', $ResultPipe, [IO.Pipes.PipeDirection]::Out)
    try {
        $pipe.Connect($resultConnectTimeoutMilliseconds)
        $writer = [IO.StreamWriter]::new($pipe, [Text.UTF8Encoding]::new($false))
        try { $writer.WriteLine($result); $writer.Flush() } finally { $writer.Dispose() }
    } finally { $pipe.Dispose() }
} else { $result }
exit $resultCode
