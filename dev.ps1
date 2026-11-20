[CmdletBinding()]
param(
    [switch]$ResetRelayState
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = $PSScriptRoot
$developmentDirectory = Join-Path $repositoryRoot '.windshare-dev'
$relayStateDirectory = Join-Path $developmentDirectory 'relay'
$relayTargetDirectory = Join-Path $developmentDirectory 'target'
$logDirectory = Join-Path $developmentDirectory 'logs'
$relayManifestPath = Join-Path $repositoryRoot 'relay-rust/Cargo.toml'
$relayPackage = 'windshare-relay-native'
$relayBinaryName = 'relay-native'
$relayConfigFileName = 'node.json'
$relayDatabaseFileName = 'relay.sqlite'
$relayIdentityFileName = 'identity.json'
$relayAddress = '127.0.0.1:8484'
$relayDirectHealthUri = "http://$relayAddress/healthz"
$frontend = [Uri]'http://localhost:38384'
$frontendUri = $frontend.GetLeftPart([UriPartial]::Authority)
$publicRelayBaseUrl = "ws://$($frontend.Authority)"
$relayProxyHealthUri = "$frontendUri/healthz"
$readinessTimeout = [TimeSpan]::FromSeconds(60)
$readinessPollInterval = [TimeSpan]::FromMilliseconds(200)
$diagnosticTailLines = 40
$incompatibleRelayDatabaseErrors = @(
    'unsupported native authority schema',
    'database belongs to a different public endpoint'
)

function Resolve-RequiredApplication {
    param(
        [Parameter(Mandatory)]
        [string]$Name,

        [Parameter(Mandatory)]
        [string]$InstallHint
    )

    $command = Get-Command -Name $Name -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($null -eq $command) {
        throw "Required command '$Name' was not found. $InstallHint"
    }
    return $command.Source
}

function Build-NativeRelay {
    param(
        [Parameter(Mandatory)]
        [string]$RustupPath,

        [Parameter(Mandatory)]
        [string]$ManifestPath,

        [Parameter(Mandatory)]
        [string]$PackageName,

        [Parameter(Mandatory)]
        [string]$BinaryName,

        [Parameter(Mandatory)]
        [string]$TargetDirectory
    )

    # Resolve the compiled executable from Cargo artifact messages so the host runs
    # directly without a nested cargo-run wrapper and without assuming target layout.
    $artifactLines = @(
        & $RustupPath run stable cargo build --locked `
            --manifest-path $ManifestPath `
            --package $PackageName `
            --bin $BinaryName `
            --target-dir $TargetDirectory `
            --message-format=json-render-diagnostics
    )
    if ($LASTEXITCODE -ne 0) {
        throw 'Native relay build failed.'
    }

    foreach ($line in $artifactLines) {
        if ([string]::IsNullOrWhiteSpace($line)) {
            continue
        }
        try {
            $message = $line | ConvertFrom-Json
        }
        catch {
            continue
        }
        $reasonProperty = $message.PSObject.Properties['reason']
        if ($null -eq $reasonProperty -or $reasonProperty.Value -ne 'compiler-artifact') {
            continue
        }
        $targetProperty = $message.PSObject.Properties['target']
        $executableProperty = $message.PSObject.Properties['executable']
        if ($null -eq $targetProperty -or $null -eq $targetProperty.Value -or $null -eq $executableProperty) {
            continue
        }
        $targetNameProperty = $targetProperty.Value.PSObject.Properties['name']
        $executable = [string]$executableProperty.Value
        if ($null -ne $targetNameProperty -and $targetNameProperty.Value -eq $BinaryName -and -not [string]::IsNullOrWhiteSpace($executable)) {
            if (-not (Test-Path -LiteralPath $executable -PathType Leaf)) {
                throw "Built relay executable was not found at '$executable'."
            }
            return $executable
        }
    }

    throw "Cargo build completed without reporting the '$BinaryName' executable."
}

function Read-LogTail {
    param(
        [Parameter(Mandatory)]
        [string]$Path,

        [int]$MaxLines = $diagnosticTailLines
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return ''
    }

    # Share ReadWrite so diagnostics stay readable while a timed-out service still holds the file open.
    $stream = [IO.FileStream]::new(
        $Path,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        [IO.FileShare]::ReadWrite
    )
    try {
        $reader = [IO.StreamReader]::new($stream, [Text.Encoding]::UTF8, $true)
        try {
            $content = $reader.ReadToEnd()
        }
        finally {
            $reader.Dispose()
        }
    }
    finally {
        $stream.Dispose()
    }

    if ([string]::IsNullOrWhiteSpace($content)) {
        return ''
    }

    $lines = @($content -split "`r?`n" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    if ($lines.Count -eq 0) {
        return ''
    }
    if ($lines.Count -gt $MaxLines) {
        $lines = @($lines[($lines.Count - $MaxLines)..($lines.Count - 1)])
    }
    return ($lines -join [Environment]::NewLine)
}

function Format-ServiceDiagnostics {
    param(
        [Parameter(Mandatory)]
        [object]$Service
    )

    $sections = [Collections.Generic.List[string]]::new()
    $stderr = Read-LogTail -Path $Service.StderrLog
    if (-not [string]::IsNullOrWhiteSpace($stderr)) {
        $sections.Add("[$($Service.Name) stderr]$([Environment]::NewLine)$stderr")
    }
    $stdout = Read-LogTail -Path $Service.StdoutLog
    if (-not [string]::IsNullOrWhiteSpace($stdout)) {
        $sections.Add("[$($Service.Name) stdout]$([Environment]::NewLine)$stdout")
    }
    $sections.Add("Logs ($($Service.Name)): $($Service.StderrLog) | $($Service.StdoutLog)")
    return ($sections -join [Environment]::NewLine)
}

function Start-LocalService {
    param(
        [Parameter(Mandatory)]
        [string]$Name,

        [Parameter(Mandatory)]
        [string]$FilePath,

        [Parameter(Mandatory)]
        [string[]]$ArgumentList
    )

    [IO.Directory]::CreateDirectory($logDirectory) | Out-Null
    $stdoutLog = Join-Path $logDirectory "$Name.stdout.log"
    $stderrLog = Join-Path $logDirectory "$Name.stderr.log"

    Write-Host "Starting $Name..."
    $process = Start-Process `
        -FilePath $FilePath `
        -ArgumentList $ArgumentList `
        -WorkingDirectory $repositoryRoot `
        -RedirectStandardOutput $stdoutLog `
        -RedirectStandardError $stderrLog `
        -WindowStyle Hidden `
        -PassThru
    return [pscustomobject]@{
        Name = $Name
        Process = $process
        StdoutLog = $stdoutLog
        StderrLog = $stderrLog
    }
}

function Test-IncompatibleRelayDatabase {
    param(
        [Parameter(Mandatory)]
        [object]$Service
    )

    $stderr = Read-LogTail -Path $Service.StderrLog
    if ([string]::IsNullOrWhiteSpace($stderr)) {
        return $false
    }
    foreach ($pattern in $incompatibleRelayDatabaseErrors) {
        if ($stderr.IndexOf($pattern, [StringComparison]::OrdinalIgnoreCase) -ge 0) {
            return $true
        }
    }
    return $false
}

function Reset-RelayAuthorityDatabase {
    param(
        [Parameter(Mandatory)]
        [string]$StateDirectory,

        [Parameter(Mandatory)]
        [string]$DatabaseFileName
    )

    # Enumerate only the authority SQLite files so identity.json and the separate
    # console database (<database>.console.sqlite) are preserved across resets.
    $files = @(
        $DatabaseFileName,
        "$DatabaseFileName-wal",
        "$DatabaseFileName-shm",
        "$DatabaseFileName.lock"
    )
    foreach ($file in $files) {
        $path = Join-Path $StateDirectory $file
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            Remove-Item -LiteralPath $path -Force
        }
    }
}

function Assert-ServicesRunning {
    param(
        [Parameter(Mandatory)]
        [object[]]$Services
    )

    foreach ($service in $Services) {
        if ($service.Process.HasExited) {
            # Ensure redirected stdout/stderr streams have finished flushing before reading logs.
            $service.Process.WaitForExit()
            $diagnostics = Format-ServiceDiagnostics -Service $service
            throw "$($service.Name) exited with code $($service.Process.ExitCode).$([Environment]::NewLine)$diagnostics"
        }
    }
}

function Test-HttpEndpoint {
    param(
        [Parameter(Mandatory)]
        [string]$Uri
    )

    try {
        $response = Invoke-WebRequest -Uri $Uri -Method Get -TimeoutSec 1
        return $response.StatusCode -ge 200 -and $response.StatusCode -lt 300
    }
    catch {
        return $false
    }
}

function Wait-ForEndpoints {
    param(
        [Parameter(Mandatory)]
        [object[]]$Services,

        [Parameter(Mandatory)]
        [object[]]$Checks
    )

    $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
    $ready = @{}
    foreach ($check in $Checks) {
        $ready[$check.Label] = $false
    }

    while ($stopwatch.Elapsed -lt $readinessTimeout) {
        Assert-ServicesRunning -Services $Services
        $allReady = $true
        foreach ($check in $Checks) {
            if (-not $ready[$check.Label]) {
                $ready[$check.Label] = Test-HttpEndpoint -Uri $check.Uri
            }
            if (-not $ready[$check.Label]) {
                $allReady = $false
            }
        }
        if ($allReady) {
            return
        }
        Start-Sleep -Milliseconds $readinessPollInterval.TotalMilliseconds
    }

    $missing = [Collections.Generic.List[string]]::new()
    $seenServices = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    $details = [Collections.Generic.List[string]]::new()
    foreach ($check in $Checks) {
        if (-not $ready[$check.Label]) {
            $missing.Add("$($check.Label) ($($check.Uri))")
            foreach ($service in $check.Services) {
                if ($seenServices.Add($service.Name)) {
                    $details.Add((Format-ServiceDiagnostics -Service $service))
                }
            }
        }
    }
    $message = "Timed out waiting for $($missing -join ' and ')."
    if ($details.Count -gt 0) {
        $message += [Environment]::NewLine + ($details -join [Environment]::NewLine)
    }
    throw $message
}

function Stop-LocalService {
    param(
        [Parameter(Mandatory)]
        [object]$Service
    )

    try {
        if (-not $Service.Process.HasExited) {
            Write-Host "Stopping $($Service.Name)..."
            # Kill the complete process tree because pnpm owns a child Vite runtime
            # that would otherwise keep the development port open.
            $Service.Process.Kill($true)
            $Service.Process.WaitForExit()
        }
    }
    catch {
        Write-Warning "Could not stop $($Service.Name): $($_.Exception.Message)"
    }
    finally {
        $Service.Process.Dispose()
    }
}

function Start-RelayService {
    param(
        [Parameter(Mandatory)]
        [string]$ExecutablePath,

        [Parameter(Mandatory)]
        [string]$ConfigPath,

        [Parameter(Mandatory)]
        [string]$StateDirectory,

        [Parameter(Mandatory)]
        [string]$DatabaseFileName,

        [Parameter(Mandatory)]
        [string]$HealthUri
    )

    $databasePath = Join-Path $StateDirectory $DatabaseFileName
    for ($attempt = 1; $attempt -le 2; $attempt += 1) {
        $relay = Start-LocalService `
            -Name 'relay' `
            -FilePath $ExecutablePath `
            -ArgumentList @(
                '--config',
                ('"{0}"' -f $ConfigPath)
            )
        $handOff = $false
        $retryWithFreshDatabase = $false
        try {
            $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
            while ($stopwatch.Elapsed -lt $readinessTimeout) {
                if ($relay.Process.HasExited) {
                    $relay.Process.WaitForExit()
                    if ($attempt -eq 1 -and (Test-Path -LiteralPath $databasePath -PathType Leaf) -and (Test-IncompatibleRelayDatabase -Service $relay)) {
                        $retryWithFreshDatabase = $true
                        break
                    }
                    $diagnostics = Format-ServiceDiagnostics -Service $relay
                    throw "relay exited with code $($relay.Process.ExitCode).$([Environment]::NewLine)$diagnostics"
                }
                if (Test-HttpEndpoint -Uri $HealthUri) {
                    $handOff = $true
                    return $relay
                }
                Start-Sleep -Milliseconds $readinessPollInterval.TotalMilliseconds
            }
            if (-not $retryWithFreshDatabase) {
                $diagnostics = Format-ServiceDiagnostics -Service $relay
                throw "Timed out waiting for relay ($HealthUri).$([Environment]::NewLine)$diagnostics"
            }
        }
        finally {
            if (-not $handOff) {
                Stop-LocalService -Service $relay
            }
        }

        # Pre-v1.0 schema updates retire legacy migrations; reset only the local
        # authority database so node identity and console state stay intact.
        Write-Warning "Local relay authority database ($databasePath) is incompatible with this build; resetting it and retrying..."
        Reset-RelayAuthorityDatabase -StateDirectory $StateDirectory -DatabaseFileName $DatabaseFileName
    }

    throw 'relay failed to start.'
}

$rustup = Resolve-RequiredApplication -Name 'rustup' -InstallHint 'Install Rust stable and ensure rustup is on PATH.'
$pnpm = Resolve-RequiredApplication -Name 'pnpm' -InstallHint 'Install pnpm and ensure it is on PATH.'
$viteCommand = Join-Path $repositoryRoot 'web/node_modules/.bin/vite.cmd'
if (-not (Test-Path -LiteralPath $viteCommand -PathType Leaf)) {
    throw 'Web dependencies are missing. Run: pnpm -C web install --frozen-lockfile'
}

$services = @()
$exitCode = 0
try {
    if ($ResetRelayState -and (Test-Path -LiteralPath $relayStateDirectory -PathType Container)) {
        Write-Host "Resetting local relay state in $relayStateDirectory..."
        Remove-Item -LiteralPath $relayStateDirectory -Recurse -Force
    }

    # Keep the node identity and database together under the node's configuration.
    [IO.Directory]::CreateDirectory($relayStateDirectory) | Out-Null
    $relayConfigPath = Join-Path $relayStateDirectory $relayConfigFileName
    $relayConfig = @{
        listen = $relayAddress
        database = $relayDatabaseFileName
        identity_file = $relayIdentityFileName
        endpoint = "$publicRelayBaseUrl/v2/ws"
        origins = @{ kind = 'allowlist'; origins = @($frontendUri) }
        stun = @{ udp_addresses = @(); admin_address = $null }
        node = @{ services = @{ relay = $true; stun = $false } }
    }
    [IO.File]::WriteAllText($relayConfigPath, ($relayConfig | ConvertTo-Json -Depth 5))

    # Complete compilation before starting the service readiness deadline.
    $relayExecutable = Build-NativeRelay `
        -RustupPath $rustup `
        -ManifestPath $relayManifestPath `
        -PackageName $relayPackage `
        -BinaryName $relayBinaryName `
        -TargetDirectory $relayTargetDirectory

    $relayService = Start-RelayService `
        -ExecutablePath $relayExecutable `
        -ConfigPath $relayConfigPath `
        -StateDirectory $relayStateDirectory `
        -DatabaseFileName $relayDatabaseFileName `
        -HealthUri $relayDirectHealthUri
    $services += $relayService

    $frontendService = Start-LocalService `
        -Name 'frontend' `
        -FilePath $pnpm `
        -ArgumentList @(
            '-C',
            'web',
            'exec',
            'vite',
            '--mode',
            'windshare-local',
            '--host',
            $frontend.Host,
            '--port',
            [string]$frontend.Port,
            '--strictPort'
        )
    $services += $frontendService

    Wait-ForEndpoints -Services $services -Checks @(
        @{ Label = 'frontend'; Uri = $frontendUri; Services = @($frontendService) },
        @{ Label = 'relay proxy'; Uri = $relayProxyHealthUri; Services = @($frontendService, $relayService) }
    )

    Write-Host ''
    Write-Host 'WindShare development services are ready:' -ForegroundColor Green
    Write-Host "  Frontend: $frontendUri"
    Write-Host "  Relay:    $publicRelayBaseUrl"
    Write-Host "  Logs:     $logDirectory"
    Write-Host ''
    Write-Host 'Create a local share from another terminal:'
    Write-Host ('  go run ./cmd/wind share "<file-or-folder>" --front-url {0} --relay {0}' -f $frontendUri)
    Write-Host ''
    Write-Host 'Press Ctrl+C to stop both services.'

    while ($true) {
        Assert-ServicesRunning -Services $services
        Start-Sleep -Milliseconds $readinessPollInterval.TotalMilliseconds
    }
}
catch [System.Management.Automation.PipelineStoppedException] {
    # Ctrl+C is the normal shutdown path for this foreground supervisor.
}
catch {
    Write-Error $_ -ErrorAction Continue
    $exitCode = 1
}
finally {
    # Unwind in reverse startup order so the frontend stops dialing before the
    # relay releases its listener and durable session state.
    for ($index = $services.Count - 1; $index -ge 0; $index -= 1) {
        Stop-LocalService -Service $services[$index]
    }
}

exit $exitCode
