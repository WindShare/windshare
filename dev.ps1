[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = $PSScriptRoot
$relayAddress = '127.0.0.1:8484'
$frontendUri = 'http://localhost:38384'
$publicRelayBaseUrl = 'ws://localhost:38384'
$relayHealthUri = "$frontendUri/healthz"
$readinessTimeout = [TimeSpan]::FromSeconds(60)
$readinessPollInterval = [TimeSpan]::FromMilliseconds(200)

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

function Start-LocalService {
    param(
        [Parameter(Mandatory)]
        [string]$Name,

        [Parameter(Mandatory)]
        [string]$FilePath,

        [Parameter(Mandatory)]
        [string[]]$ArgumentList
    )

    Write-Host "Starting $Name..."
    $process = Start-Process `
        -FilePath $FilePath `
        -ArgumentList $ArgumentList `
        -WorkingDirectory $repositoryRoot `
        -NoNewWindow `
        -PassThru
    return [pscustomobject]@{
        Name = $Name
        Process = $process
    }
}

function Assert-ServicesRunning {
    param(
        [Parameter(Mandatory)]
        [object[]]$Services
    )

    foreach ($service in $Services) {
        if ($service.Process.HasExited) {
            throw "$($service.Name) exited with code $($service.Process.ExitCode)."
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

function Wait-ForLocalServices {
    param(
        [Parameter(Mandatory)]
        [object[]]$Services
    )

    $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
    $relayReady = $false
    $frontendReady = $false

    while ($stopwatch.Elapsed -lt $readinessTimeout) {
        Assert-ServicesRunning -Services $Services
        if (-not $relayReady) {
            $relayReady = Test-HttpEndpoint -Uri $relayHealthUri
        }
        if (-not $frontendReady) {
            $frontendReady = Test-HttpEndpoint -Uri $frontendUri
        }
        if ($relayReady -and $frontendReady) {
            return
        }
        Start-Sleep -Milliseconds $readinessPollInterval.TotalMilliseconds
    }

    $missing = @()
    if (-not $relayReady) {
        $missing += "relay ($relayHealthUri)"
    }
    if (-not $frontendReady) {
        $missing += "frontend ($frontendUri)"
    }
    throw "Timed out waiting for $($missing -join ' and ')."
}

function Stop-LocalService {
    param(
        [Parameter(Mandatory)]
        [object]$Service
    )

    try {
        if (-not $Service.Process.HasExited) {
            Write-Host "Stopping $($Service.Name)..."
            # Kill the complete process tree because both go run and pnpm own a
            # child runtime that would otherwise keep the development port open.
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

$go = Resolve-RequiredApplication -Name 'go' -InstallHint 'Install Go and ensure it is on PATH.'
$pnpm = Resolve-RequiredApplication -Name 'pnpm' -InstallHint 'Install pnpm and ensure it is on PATH.'
$viteCommand = Join-Path $repositoryRoot 'web/node_modules/.bin/vite.cmd'
if (-not (Test-Path -LiteralPath $viteCommand -PathType Leaf)) {
    throw 'Web dependencies are missing. Run: pnpm -C web install --frozen-lockfile'
}

$services = @()
$exitCode = 0
try {
    $services += Start-LocalService `
        -Name 'relay' `
        -FilePath $go `
        -ArgumentList @(
            'run',
            './relay/cmd/wsrelay',
            '-listen',
            $relayAddress,
            '-relay-base-url',
            $publicRelayBaseUrl
        )
    $services += Start-LocalService `
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
            'localhost',
            '--port',
            '38384',
            '--strictPort'
        )

    Wait-ForLocalServices -Services $services
    Write-Host ''
    Write-Host 'WindShare development services are ready:' -ForegroundColor Green
    Write-Host "  Frontend: $frontendUri"
    Write-Host "  Relay:    $publicRelayBaseUrl"
    Write-Host ''
    Write-Host 'Create a local share from another terminal:'
    Write-Host '  go run ./cmd/wind share "<file-or-folder>"'
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
