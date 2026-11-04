Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'network.psm1') -Force
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-network-test-' + [Guid]::NewGuid().ToString('N'))
$previousPreference = $env:WINDSHARE_NETWORK_PREFERENCE
try {
    [IO.Directory]::CreateDirectory($testRoot) | Out-Null
    $executable = Join-Path $testRoot 'installed app\wind.exe'
    $script:calls = [Collections.Generic.List[object]]::new()
    $run = { param($program, $arguments) $script:calls.Add(@{ program = $program; arguments = $arguments; preference = $env:WINDSHARE_NETWORK_PREFERENCE }) }
    $failPrompt = { throw 'Unexpected interactive prompt.' }
    Invoke-WindShareNetworkSetup $executable $testRoot Ask -Interactive $false -Run $run -Prompt $failPrompt
    if ($script:calls.Count -ne 0 -or (Test-Path (Join-Path $testRoot 'network-preference.json'))) { throw 'Unattended install changed preferences or ran setup.' }
    Invoke-WindShareNetworkSetup $executable $testRoot Configure -Interactive $false -Run $run -Prompt $failPrompt
    if ($script:calls.Count -ne 1 -or ($script:calls[0].arguments -join ' ') -ne 'setup network --non-interactive') { throw 'Unattended explicit setup can elevate.' }
    Invoke-WindShareNetworkSetup $executable $testRoot Skip -Interactive $false -Run $run -Prompt $failPrompt
    if (($script:calls[1].arguments -join ' ') -ne 'setup network --skip --non-interactive') { throw 'Skip bypassed the shared preference capability.' }
    Invoke-WindShareNetworkSetup $executable $testRoot Remove -Interactive $false -Run $run -Prompt $failPrompt
    if (($script:calls[2].arguments -join ' ') -ne 'setup network --remove --non-interactive') { throw 'Uninstall bypassed owned-rule cleanup.' }
    Invoke-WindShareNetworkSetup $executable $testRoot Ask -Interactive $true -Run $run -Prompt { 'y' }
    if (($script:calls[3].arguments -join ' ') -ne 'setup network') { throw 'Interactive consent did not reach shared setup.' }
    Invoke-WindShareNetworkSetup $executable $testRoot Ask -Interactive $true -Run $run -Prompt { 'n' }
    if (($script:calls[4].arguments -join ' ') -ne 'setup network --skip') { throw 'Explicit decline was not saved.' }
    foreach ($choice in @('skip', 'configure')) {
        [IO.File]::WriteAllText((Join-Path $testRoot 'network-preference.json'), "{`"schema`":1,`"choice`":`"$choice`"}")
        Invoke-WindShareNetworkSetup $executable $testRoot Ask -Interactive $true -Run $run -Prompt $failPrompt
    }
    if ($script:calls.Count -ne 5) { throw 'Saved user choice was repeated.' }
    foreach ($call in $script:calls) {
        if ($call.program -ne $executable -or $call.preference -ne (Join-Path $testRoot 'network-preference.json')) { throw 'Setup targeted another installation or user.' }
    }
    try { Invoke-WindShareNetworkSetup $executable $testRoot Configure -Run { throw 'test failure' } } catch { }
    if ($env:WINDSHARE_NETWORK_PREFERENCE -ne $previousPreference) { throw 'Installer leaked preference routing.' }
} finally {
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $ownedPrefix = Join-Path ([IO.Path]::GetFullPath([IO.Path]::GetTempPath())) 'windshare-network-test-'
    if (-not $resolved.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Unexpected test cleanup path.' }
    Remove-Item -LiteralPath $resolved -Recurse -Force
}
Write-Output 'Windows installer network contracts: PASS (injected commands only)'
