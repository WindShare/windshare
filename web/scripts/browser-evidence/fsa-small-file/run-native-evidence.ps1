[CmdletBinding()]
param(
  [ValidateSet('Edge', 'Chrome')]
  [string]$Browser = 'Edge',
  [ValidateRange(3, 10)]
  [int]$MeasuredPairs = 3,
  [switch]$DiagnosticProductOnly,
  [string]$ReplayRepository,
  [ValidateRange(120, 1800)]
  [int]$TimeoutSeconds = 600
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$moduleRoot = [IO.Path]::GetFullPath($PSScriptRoot)
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $moduleRoot '..\..\..\..'))
if ([string]::IsNullOrWhiteSpace($ReplayRepository)) {
  $ReplayRepository = [IO.Path]::GetFullPath((Join-Path $repositoryRoot '..\BrowserNativeUiReplay'))
}
$replayModulePath = Join-Path $ReplayRepository 'src\windows\BrowserNativeUiReplay.psm1'
if (-not (Test-Path -LiteralPath $replayModulePath -PathType Leaf)) {
  throw "BrowserNativeUiReplay module not found: $replayModulePath"
}
$replayModule = Import-Module $replayModulePath -Force -PassThru
$sessionId = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ') + '-' + [guid]::NewGuid().ToString('N').Substring(0, 8)

& $replayModule {
  param(
    [string]$Browser,
    [int]$MeasuredPairs,
    [bool]$DiagnosticProductOnly,
    [string]$ReplayRepository,
    [int]$TimeoutSeconds,
    [string]$ModuleRoot,
    [string]$RepositoryRoot,
    [string]$SessionId
  )

  function Invoke-NodeChecked {
    param([string[]]$Arguments)
    & $nodePath @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Node command failed with exit code ${LASTEXITCODE}: $($Arguments -join ' ')" }
  }

  function Wait-OwnedProcess {
    param(
      [Diagnostics.Process]$Process,
      [int]$Seconds,
      [string]$Label
    )
    $deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
    while ([DateTime]::UtcNow -lt $deadline) {
      $Process.Refresh()
      if ($Process.HasExited) { break }
      Start-Sleep -Milliseconds 100
    }
    $Process.Refresh()
    if (-not $Process.HasExited) { throw "$Label timed out" }
    if ($Process.ExitCode -ne 0) { throw "$Label exited with code $($Process.ExitCode)" }
  }

  function Invoke-W7NativeDirectorySelection {
    param(
      [object]$Picker,
      [string]$TargetPath,
      [string]$TreeEvidencePath,
      [string]$ActionEvidencePath
    )
    try {
      return Invoke-NativeDirectorySelection `
        -Picker $Picker `
        -FixturePath $TargetPath `
        -TreeEvidencePath $TreeEvidencePath `
        -ActionEvidencePath $ActionEvidencePath
    } catch {
      if ($_.Exception.Message -notlike '*Could not foreground the target window*') { throw }
    }

    if (-not ('WindShare.FsaEvidence.NativeForegroundRecoveryV1' -as [type])) {
      Add-Type -Path (Join-Path $ModuleRoot 'NativeForegroundRecovery.cs')
    }
    # Windows can deny SetForegroundWindow when the visible Ripple terminal owns the input queue.
    # Recover that desktop state, then reuse the replay module's full identity and path verification.
    [WindShare.FsaEvidence.NativeForegroundRecoveryV1]::Recover([long]$Picker.Handle)
    return Invoke-NativeDirectorySelection `
      -Picker $Picker `
      -FixturePath $TargetPath `
      -TreeEvidencePath $TreeEvidencePath `
      -ActionEvidencePath $ActionEvidencePath
  }

  function Invoke-NativeCase {
    param(
      [string]$Mode,
      [string]$Url,
      [string]$ExpectedOrigin,
      [string]$TargetPath,
      [string]$CaseDirectory
    )
    $paths = [ordered]@{
      trigger = Join-Path $CaseDirectory 'trigger.json'
      raw = Join-Path $CaseDirectory 'raw-cdp.json'
      screenshot = Join-Path $CaseDirectory 'page-final.png'
      pickerDiscovery = Join-Path $CaseDirectory 'picker-discovery.json'
      pickerTree = Join-Path $CaseDirectory 'picker-tree.json'
      pickerAction = Join-Path $CaseDirectory 'picker-action.json'
      permissionTree = Join-Path $CaseDirectory 'permission-tree.json'
      permissionPostInvokeTree = Join-Path $CaseDirectory 'permission-post-invoke-tree.json'
      permissionAction = Join-Path $CaseDirectory 'permission-action.json'
    }
    $driver = $null
    try {
      $baselineDialogs = @(Get-NativeDialogHandles)
      $driver = Start-ReplayProcess `
        -FilePath $nodePath `
        -ArgumentList @(
          (Join-Path $ModuleRoot 'native-cdp-driver.mjs'),
          '--endpoint', $cdpEndpoint,
          '--url', $Url,
          '--replay-root', $ReplayRepository,
          '--mode', $Mode,
          '--trigger', $paths.trigger,
          '--evidence', $paths.raw,
          '--screenshot', $paths.screenshot,
          '--timeout-ms', [string]($TimeoutSeconds * 1000)
        ) `
        -WorkingDirectory $ModuleRoot `
        -CreateNoWindow
      [void](Wait-ReplayFile -Path $paths.trigger -TimeoutSeconds 180)
      $picker = Wait-NativeDirectoryPicker `
        -BrowserDefinition $browserDefinition `
        -ProfilePath $profilePath `
        -BaselineDialogHandles $baselineDialogs `
        -DiagnosticPath $paths.pickerDiscovery `
        -TimeoutSeconds 45
      [void](Invoke-W7NativeDirectorySelection `
        -Picker $picker `
        -TargetPath $TargetPath `
        -TreeEvidencePath $paths.pickerTree `
        -ActionEvidencePath $paths.pickerAction)
      [void](Invoke-ChromiumPermissionPrompt `
        -BrowserDefinition $browserDefinition `
        -ProfilePath $profilePath `
        -PromptConfigurationPath (Join-Path $ReplayRepository 'config\prompt-labels.json') `
        -ExpectedOrigin $ExpectedOrigin `
        -FixturePath $TargetPath `
        -TreeEvidencePath $paths.permissionTree `
        -ActionEvidencePath $paths.permissionAction `
        -PostInvokeTreeEvidencePath $paths.permissionPostInvokeTree `
        -TimeoutSeconds 45)
      Wait-OwnedProcess -Process $driver -Seconds $TimeoutSeconds -Label "$Mode CDP driver"
      if (-not (Test-Path -LiteralPath $paths.raw -PathType Leaf)) {
        throw "$Mode CDP driver produced no raw evidence"
      }
      return $paths
    } finally {
      try { Stop-ReplayProcess -Process $driver -Label "$Mode CDP driver" } catch { Write-Warning $_.Exception.Message }
    }
  }

  [void](Test-BrowserNativeUiReplayEnvironment -Browser $Browser)
  $browserDefinition = Get-BrowserDefinition -Browser $Browser
  $nodePath = (Get-Command node.exe -ErrorAction Stop | Select-Object -First 1).Source
  $evidenceRoot = Join-Path $ModuleRoot 'evidence'
  if (-not (Test-Path -LiteralPath $evidenceRoot -PathType Container)) {
    [void](New-Item -ItemType Directory -Path $evidenceRoot -ErrorAction Stop)
  }
  $evidenceDirectory = New-ReplayChildDirectory -Parent $evidenceRoot -Name $SessionId
  $workParent = Join-Path $RepositoryRoot 'tmp\fsa-small-file-native'
  if (-not (Test-Path -LiteralPath $workParent -PathType Container)) {
    [void](New-Item -ItemType Directory -Path $workParent -ErrorAction Stop)
  }
  $workDirectory = New-ReplayChildDirectory -Parent $workParent -Name $SessionId
  $profilePath = New-ReplayChildDirectory -Parent $workDirectory -Name 'browser-profile'
  $targetsPath = New-ReplayChildDirectory -Parent $workDirectory -Name 'targets'
  $stackArtifacts = New-ReplayChildDirectory -Parent $workDirectory -Name 'product-stack'

  $staticReadyPath = Join-Path $workDirectory 'static-ready.json'
  $stackReadyPath = Join-Path $workDirectory 'product-stack-ready.json'
  $environmentPath = Join-Path $evidenceDirectory 'environment.json'
  $workloadProofPath = Join-Path $evidenceDirectory 'workload-proof.json'
  $pairedPath = Join-Path $evidenceDirectory 'paired-evidence.json'
  $observationsPath = Join-Path $evidenceDirectory 'native-observations.json'
  $diagnosticPath = Join-Path $evidenceDirectory 'diagnostic-observations.json'
  $staticProcess = $null
  $stackProcess = $null
  $browserProcess = $null
  $stackReady = $null
  $cdpEndpoint = $null
  $baselineResults = [Collections.Generic.List[string]]::new()
  $productResults = [Collections.Generic.List[string]]::new()
  $rawProducts = [Collections.Generic.List[string]]::new()
  try {
    Invoke-NodeChecked -Arguments @(
      (Join-Path $ModuleRoot 'cli.mjs'), 'validate-workload', '--output', $workloadProofPath
    )
    $workloadProof = Get-Content -LiteralPath $workloadProofPath -Raw | ConvertFrom-Json -Depth 100

    if (-not $DiagnosticProductOnly) {
      $staticProcess = Start-ReplayProcess `
        -FilePath $nodePath `
        -ArgumentList @(
          (Join-Path $ModuleRoot 'native-static-server.mjs'),
          '--host', '127.0.0.1',
          '--port', '0',
          '--web-root', (Join-Path $RepositoryRoot 'web'),
          '--workload', (Join-Path $RepositoryRoot 'testdata\browser-evidence\v1\fsa-small-file-workload.json'),
          '--ready-file', $staticReadyPath
        ) `
        -WorkingDirectory $ModuleRoot `
        -CreateNoWindow
      $staticReady = Wait-ReplayFile -Path $staticReadyPath -TimeoutSeconds 30
      $baselineOrigin = "http://127.0.0.1:$($staticReady.port)"
      $baselineUrl = "$baselineOrigin/scripts/browser-evidence/fsa-small-file/native-probe/index.html"
    }

    $vitePort = Get-FreeTcpPort
    $relayPort = Get-FreeTcpPort
    $stackProcess = Start-ReplayProcess `
      -FilePath $nodePath `
      -ArgumentList @(
        (Join-Path $ModuleRoot 'native-product-stack.mjs'),
        '--artifacts', $stackArtifacts,
        '--ready-file', $stackReadyPath,
        '--vite-port', [string]$vitePort,
        '--relay-port', [string]$relayPort,
        '--result-count', [string]$(if ($DiagnosticProductOnly) { 2 } else { $MeasuredPairs + 1 })
      ) `
      -WorkingDirectory $RepositoryRoot `
      -CreateNoWindow
    $stackReady = Wait-ReplayFile -Path $stackReadyPath -TimeoutSeconds 180
    if ($stackReady.workloadSha256 -cne $workloadProof.workloadSha256) {
      throw 'Product stack did not consume the authenticated canonical workload'
    }

    $cdpPort = Get-FreeTcpPort
    $cdpEndpoint = "http://127.0.0.1:$cdpPort"
    $browserArguments = @(
      "--user-data-dir=$profilePath",
      "--remote-debugging-port=$cdpPort",
      '--remote-allow-origins=*',
      '--disable-extensions',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-session-crashed-bubble',
      '--new-window',
      'about:blank'
    )
    if ($Browser -eq 'Edge') {
      $browserArguments = @('--disable-features=msEdgeFirstRunExperience') + $browserArguments
    }
    $browserProcess = Start-ReplayProcess `
      -FilePath $browserDefinition.Executable `
      -ArgumentList $browserArguments `
      -WorkingDirectory $workDirectory
    [void](Wait-ExactCdpTarget -Endpoint $cdpEndpoint -ExpectedUrl 'about:blank' -TimeoutSeconds 30)

    $driveRoot = [IO.Path]::GetPathRoot($workDirectory)
    $driveLetter = $driveRoot.Substring(0, 1)
    $volume = Get-Volume -DriveLetter $driveLetter -ErrorAction Stop
    $partition = Get-Partition -DriveLetter $driveLetter -ErrorAction Stop
    $disk = Get-Disk -Number $partition.DiskNumber -ErrorAction Stop
    $cpu = Get-CimInstance Win32_Processor -ErrorAction Stop | Select-Object -First 1
    $commit = (& git -C $RepositoryRoot rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Could not resolve repository commit' }
    $browserItem = Get-Item -LiteralPath $browserDefinition.Executable -ErrorAction Stop
    $environment = [ordered]@{
      evidenceSessionId = $SessionId
      repositoryCommit = $commit
      os = [ordered]@{
        platform = 'win32'
        release = [Environment]::OSVersion.Version.ToString()
        architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
      }
      hardware = [ordered]@{ cpuModel = $cpu.Name.Trim() }
      browser = [ordered]@{
        name = $browserDefinition.Name
        version = $browserItem.VersionInfo.ProductVersion
        executableSha256 = (Get-FileHash -LiteralPath $browserItem.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
      }
      targetVolume = [ordered]@{
        fileSystem = $volume.FileSystem
        volumeType = $disk.BusType.ToString()
        volumeIdentity = $volume.UniqueId
      }
    }
    Write-ReplayJson -Path $environmentPath -Value $environment

    $lastRepetition = if ($DiagnosticProductOnly) { 1 } else { $MeasuredPairs }
    for ($repetition = 0; $repetition -le $lastRepetition; $repetition += 1) {
      $pairId = if ($DiagnosticProductOnly) { "diagnostic-$repetition" } else { "pair-$repetition" }
      $pairDirectory = New-ReplayChildDirectory -Parent $evidenceDirectory -Name $pairId
      $productDirectory = New-ReplayChildDirectory -Parent $pairDirectory -Name 'product'
      $productTarget = New-ReplayChildDirectory -Parent $targetsPath -Name "product-$repetition"
      if (-not $DiagnosticProductOnly) {
        $baselineDirectory = New-ReplayChildDirectory -Parent $pairDirectory -Name 'baseline'
        $baselineTarget = New-ReplayChildDirectory -Parent $targetsPath -Name "baseline-$repetition"
        Write-Host "[$SessionId] $pairId pure-FSA"
        $baselineUrlForRun = "$baselineUrl`?run=baseline-$repetition"
        $baselineRaw = Invoke-NativeCase `
          -Mode 'baseline' `
          -Url $baselineUrlForRun `
          -ExpectedOrigin $baselineOrigin `
          -TargetPath $baselineTarget `
          -CaseDirectory $baselineDirectory
        $baselineHost = Join-Path $baselineDirectory 'host-verification.json'
        Invoke-NodeChecked -Arguments @(
          (Join-Path $ModuleRoot 'cli.mjs'), 'verify-host',
          '--root', $baselineTarget,
          '--output', $baselineHost
        )
        $baselineResult = Join-Path $baselineDirectory 'result.json'
        Invoke-NodeChecked -Arguments @(
          (Join-Path $ModuleRoot 'native-results.mjs'), 'assemble',
          '--kind', 'baseline',
          '--raw', $baselineRaw.raw,
          '--host', $baselineHost,
          '--environment', $environmentPath,
          '--repetition', [string]$repetition,
          '--run-id', "baseline-$SessionId-$repetition",
          '--pair-id', $pairId,
          '--output', $baselineResult
        )
        $baselineResults.Add($baselineResult)
      }

      Write-Host "[$SessionId] $pairId product DirectTree"
      $share = @($stackReady.shares) | Where-Object { $_.ordinal -eq $repetition } | Select-Object -First 1
      if ($null -eq $share) { throw "Product share is missing for repetition $repetition" }
      $productRaw = Invoke-NativeCase `
        -Mode 'product' `
        -Url $share.url `
        -ExpectedOrigin $stackReady.viteUrl `
        -TargetPath $productTarget `
        -CaseDirectory $productDirectory
      $materializedRoot = Join-Path $productTarget ($stackReady.materializedRootRelativePath.Replace('/', [IO.Path]::DirectorySeparatorChar))
      $productHost = Join-Path $productDirectory 'host-verification.json'
      Invoke-NodeChecked -Arguments @(
        (Join-Path $ModuleRoot 'cli.mjs'), 'verify-host',
        '--root', $materializedRoot,
        '--output', $productHost
      )
      $productResult = Join-Path $productDirectory 'result.json'
      Invoke-NodeChecked -Arguments @(
        (Join-Path $ModuleRoot 'native-results.mjs'), 'assemble',
        '--kind', 'product',
        '--raw', $productRaw.raw,
        '--host', $productHost,
        '--environment', $environmentPath,
        '--repetition', [string]$repetition,
        '--run-id', "product-$SessionId-$repetition",
        '--pair-id', $pairId,
        '--output', $productResult
      )
      $productResults.Add($productResult)
      $rawProducts.Add($productRaw.raw)
    }

    if ($DiagnosticProductOnly) {
      $diagnosticArguments = [Collections.Generic.List[string]]::new()
      $diagnosticArguments.Add((Join-Path $ModuleRoot 'native-results.mjs'))
      $diagnosticArguments.Add('summarize-diagnostic')
      for ($index = 0; $index -lt $productResults.Count; $index += 1) {
        $diagnosticArguments.Add('--product')
        $diagnosticArguments.Add($productResults[$index])
        $diagnosticArguments.Add('--raw-product')
        $diagnosticArguments.Add($rawProducts[$index])
      }
      $diagnosticArguments.Add('--output')
      $diagnosticArguments.Add($diagnosticPath)
      Invoke-NodeChecked -Arguments $diagnosticArguments.ToArray()
      $diagnostic = Get-Content -LiteralPath $diagnosticPath -Raw | ConvertFrom-Json -Depth 100
      [pscustomobject]@{
        EvidenceRan = $true
        SessionId = $SessionId
        Status = 'diagnostic-complete'
        Product = $diagnostic.measured.result.timing
        EvidenceDirectory = $evidenceDirectory
        WorkDirectory = $workDirectory
      }
    } else {
      $pairedArguments = [Collections.Generic.List[string]]::new()
      $pairedArguments.Add((Join-Path $ModuleRoot 'cli.mjs'))
      $pairedArguments.Add('summarize')
      for ($index = 0; $index -lt $baselineResults.Count; $index += 1) {
        $pairedArguments.Add('--baseline')
        $pairedArguments.Add($baselineResults[$index])
        $pairedArguments.Add('--product')
        $pairedArguments.Add($productResults[$index])
      }
      $pairedArguments.Add('--output')
      $pairedArguments.Add($pairedPath)
      Invoke-NodeChecked -Arguments $pairedArguments.ToArray()

      $observationArguments = [Collections.Generic.List[string]]::new()
      $observationArguments.Add((Join-Path $ModuleRoot 'native-results.mjs'))
      $observationArguments.Add('summarize-native')
      $observationArguments.Add('--paired')
      $observationArguments.Add($pairedPath)
      for ($index = 0; $index -lt $baselineResults.Count; $index += 1) {
        $observationArguments.Add('--baseline')
        $observationArguments.Add($baselineResults[$index])
        $observationArguments.Add('--product')
        $observationArguments.Add($productResults[$index])
        $observationArguments.Add('--raw-product')
        $observationArguments.Add($rawProducts[$index])
      }
      $observationArguments.Add('--output')
      $observationArguments.Add($observationsPath)
      Invoke-NodeChecked -Arguments $observationArguments.ToArray()

      $paired = Get-Content -LiteralPath $pairedPath -Raw | ConvertFrom-Json -Depth 100
      [pscustomobject]@{
        EvidenceRan = $true
        SessionId = $SessionId
        Status = $paired.performanceTarget.status
        Baseline = $paired.durations.baseline
        Product = $paired.durations.product
        Ratio = $paired.diagnostics.productToBaselineMedianRatio
        EvidenceDirectory = $evidenceDirectory
        WorkDirectory = $workDirectory
      }
    }
  } finally {
    try { Stop-IsolatedBrowserProcesses -BrowserDefinition $browserDefinition -ProfilePath $profilePath } catch { Write-Warning $_.Exception.Message }
    try { Stop-ReplayProcess -Process $browserProcess -Label 'isolated browser launcher' } catch { Write-Warning $_.Exception.Message }
    if ($null -ne $stackReady) {
      $ownedPids = @($stackReady.pids.senders) + @($stackReady.pids.relay, $stackReady.pids.vite, $stackReady.pids.stack)
      foreach ($ownedPid in $ownedPids) {
        if ($null -ne (Get-Process -Id $ownedPid -ErrorAction SilentlyContinue)) {
          try { Stop-Process -Id $ownedPid -ErrorAction Stop } catch { Write-Warning $_.Exception.Message }
        }
      }
    }
    try { Stop-ReplayProcess -Process $stackProcess -Label 'product stack' } catch { Write-Warning $_.Exception.Message }
    try { Stop-ReplayProcess -Process $staticProcess -Label 'baseline server' } catch { Write-Warning $_.Exception.Message }
  }
} -Browser $Browser -MeasuredPairs $MeasuredPairs -DiagnosticProductOnly ([bool]$DiagnosticProductOnly) `
  -ReplayRepository $ReplayRepository `
  -TimeoutSeconds $TimeoutSeconds -ModuleRoot $moduleRoot -RepositoryRoot $repositoryRoot `
  -SessionId $sessionId
