[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$OutputDirectory,
    [string]$Candidate = '/scripts/browser-evidence/folder-recovery/browser-harness.mjs',
    [string]$Baseline = '/scripts/browser-evidence/folder-recovery/browser-harness.mjs',
    [ValidateRange(1, 5)][int]$Repetitions = 3,
    [ValidateSet('Edge', 'Chrome')][string]$Browser = 'Edge',
    [string]$ReplayRepository
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../../..'))
if ([string]::IsNullOrWhiteSpace($ReplayRepository)) {
    $ReplayRepository = [IO.Path]::GetFullPath((Join-Path $repositoryRoot '../BrowserNativeUiReplay'))
}
$replay = Import-Module (Join-Path $ReplayRepository 'src/windows/BrowserNativeUiReplay.psm1') -Force -PassThru
$evidenceDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if ((Test-Path -LiteralPath $evidenceDirectory) -and @(Get-ChildItem -LiteralPath $evidenceDirectory -Force).Count -ne 0) {
    throw 'Native evidence output directory must be empty so old authority receipts cannot be reused'
}
[void](New-Item -ItemType Directory -Path $evidenceDirectory -Force)
& $replay {
    param($RepositoryRoot, $EvidenceDirectory, $ReplayRepository, $Browser, $Candidate, $Baseline, $Repetitions)
    $environment = Test-BrowserNativeUiReplayEnvironment -Browser $Browser
    $browserDefinition = Get-BrowserDefinition -Browser $Browser
    $runId = [guid]::NewGuid().ToString('N')
    $workParent = Join-Path $RepositoryRoot '.tmp/folder-recovery-native'
    [void](New-Item -ItemType Directory -Path $workParent -Force)
    $workDirectory = New-ReplayChildDirectory -Parent $workParent -Name $runId
    $target = New-ReplayChildDirectory -Parent $workDirectory -Name 'targets'
    $readyFile = Join-Path $EvidenceDirectory 'native-ready.json'
    $resultFile = Join-Path $EvidenceDirectory 'native-result.json'
    $runner = $null
    $ready = $null
    try {
        $baselineDialogs = @(Get-NativeDialogHandles)
        $runner = Start-ReplayProcess -FilePath (Get-Command node.exe).Source -CreateNoWindow `
            -WorkingDirectory $RepositoryRoot -ArgumentList @(
                (Join-Path $RepositoryRoot 'web/scripts/browser-evidence/folder-recovery/run.mjs'),
                '--output', $resultFile, '--candidate', $Candidate, '--baseline', $Baseline,
                '--repetitions', [string]$Repetitions, '--native-target', $target,
                '--native-ready', $readyFile, '--browser-executable', $browserDefinition.Executable
            )
        $ready = Wait-ReplayFile -Path $readyFile -TimeoutSeconds 30
        $picker = Wait-NativeDirectoryPicker -BrowserDefinition $browserDefinition `
            -ProfilePath $ready.profile -BaselineDialogHandles $baselineDialogs `
            -DiagnosticPath (Join-Path $EvidenceDirectory 'picker-discovery.json') -TimeoutSeconds 30
        try {
            [void](Invoke-NativeDirectorySelection -Picker $picker -FixturePath $target `
                -TreeEvidencePath (Join-Path $EvidenceDirectory 'picker-tree.json') `
                -ActionEvidencePath (Join-Path $EvidenceDirectory 'picker-action.json'))
        } catch {
            if ($_.Exception.Message -notlike '*Could not foreground the target window*') { throw }
            if (-not ('WindShare.FsaEvidence.NativeForegroundRecoveryV1' -as [type])) {
                Add-Type -Path (Join-Path $RepositoryRoot 'web/scripts/browser-evidence/fsa-small-file/NativeForegroundRecovery.cs')
            }
            [WindShare.FsaEvidence.NativeForegroundRecoveryV1]::Recover([long]$picker.Handle)
            [void](Invoke-NativeDirectorySelection -Picker $picker -FixturePath $target `
                -TreeEvidencePath (Join-Path $EvidenceDirectory 'picker-tree-retry.json') `
                -ActionEvidencePath (Join-Path $EvidenceDirectory 'picker-action-retry.json'))
        }
        [void](Invoke-ChromiumPermissionPrompt -BrowserDefinition $browserDefinition -ProfilePath $ready.profile `
            -PromptConfigurationPath (Join-Path $ReplayRepository 'config/prompt-labels.json') `
            -ExpectedOrigin $ready.origin -FixturePath $target `
            -TreeEvidencePath (Join-Path $EvidenceDirectory 'permission-tree.json') `
            -ActionEvidencePath (Join-Path $EvidenceDirectory 'permission-action.json') `
            -PostInvokeTreeEvidencePath (Join-Path $EvidenceDirectory 'permission-after.json') -TimeoutSeconds 30)
        $result = Wait-ReplayFile -Path $resultFile -TimeoutSeconds 60
        if ($result.status -ne 'completed') { throw "Native evidence failed: $($result.error.message)" }
        $remainder = @(Get-ChildItem -LiteralPath $target -Recurse -Force)
        if ($remainder.Count -ne 0) { throw 'Native target cleanup left owned files behind' }
        $resolvedWork = [IO.Path]::GetFullPath($workDirectory)
        $resolvedParent = [IO.Path]::GetFullPath($workParent).TrimEnd('\') + '\'
        if (-not $resolvedWork.StartsWith($resolvedParent, [StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing unexpected native evidence cleanup path' }
        Remove-Item -LiteralPath $resolvedWork -Recurse -Force
        $targetDrive = [IO.DriveInfo]::new([IO.Path]::GetPathRoot($target))
        $profileDrive = [IO.DriveInfo]::new([IO.Path]::GetPathRoot($ready.profile))
        Write-ReplayJson -Path (Join-Path $EvidenceDirectory 'native-acquisition.json') -Value @{
            targetVolume = @{ root = $targetDrive.Name; fileSystem = $targetDrive.DriveFormat }
            profileVolume = @{ root = $profileDrive.Name; fileSystem = $profileDrive.DriveFormat }
            status = 'completed'; environment = $environment; target = $target; targetRemoved = $true
            result = $resultFile; repositoryCommit = (& git -C $RepositoryRoot rev-parse HEAD).Trim()
        }
        Write-Output $resultFile
    } catch {
        Write-ReplayJson -Path (Join-Path $EvidenceDirectory 'native-acquisition-failure.json') -Value @{
            status = 'failed'; environment = $environment; target = $target; error = $_.Exception.Message
            result = $resultFile; ready = $ready
        }
        throw
    } finally {
        if ($null -ne $ready) {
            try { Stop-IsolatedBrowserProcesses -BrowserDefinition $browserDefinition -ProfilePath $ready.profile } catch { Write-Warning $_.Exception.Message }
        }
        Stop-ReplayProcess -Process $runner -Label 'folder recovery evidence runner'
    }
} $repositoryRoot $evidenceDirectory $ReplayRepository $Browser $Candidate $Baseline $Repetitions
