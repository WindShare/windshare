Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Test-WindShareInteractive {
    if ($env:CI -and $env:CI -notin @('false', '0')) { return $false }
    if (@([Environment]::GetCommandLineArgs() | Where-Object { $_ -match '^-NonI(nteractive)?$' }).Count -gt 0) { return $false }
    return [Environment]::UserInteractive -and -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected
}

function Invoke-WindShareNetworkSetup {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string]$StateDirectory,
        [Parameter(Mandatory)][ValidateSet('Ask', 'Configure', 'Skip', 'Remove')][string]$Choice,
        [bool]$Interactive = (Test-WindShareInteractive),
        [scriptblock]$Run = { param($program, $arguments) & $program @arguments; if ($LASTEXITCODE -ne 0) { throw "Network setup exited with code $LASTEXITCODE." } },
        [scriptblock]$Prompt = { Read-Host 'Allow WindShare through the firewall (inbound UDP/TCP, domain/private/public networks)? [y/N]' }
    )
    $preferencePath = Join-Path ([IO.Path]::GetFullPath($StateDirectory)) 'network-preference.json'
    if ($Choice -eq 'Ask') {
        # An unattended install has no user decision to persist. The explicit CLI
        # command remains available in an already privileged automation environment.
        if (-not $Interactive) { return }
        if (Test-Path -LiteralPath $preferencePath) {
            try {
                $saved = Get-Content -LiteralPath $preferencePath -Raw | ConvertFrom-Json
                if ($saved.schema -eq 2) {
                    if ($saved.choice -eq 'skip') { return }
                    if ($saved.choice -eq 'configure' -and $saved.executable -ieq [IO.Path]::GetFullPath($Executable)) { return }
                }
            } catch { Write-Warning 'Network setup preference is unreadable.' }
        }
        $Choice = if ((& $Prompt) -ieq 'y') { 'Configure' } else { 'Skip' }
    }
    $arguments = @('setup', 'network')
    if ($Choice -eq 'Skip') { $arguments += '--skip' }
    if ($Choice -eq 'Remove') { $arguments += '--remove' }
    if (-not $Interactive) { $arguments += '--non-interactive' }
    $previousPreferencePath = $env:WINDSHARE_NETWORK_PREFERENCE
    try {
        $env:WINDSHARE_NETWORK_PREFERENCE = $preferencePath
        & $Run $Executable $arguments
    } finally { $env:WINDSHARE_NETWORK_PREFERENCE = $previousPreferencePath }
}

Export-ModuleMember -Function Invoke-WindShareNetworkSetup
