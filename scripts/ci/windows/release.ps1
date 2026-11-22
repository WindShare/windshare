# Deterministic root module release gate (Windows). The gate reads one exact
# commit object, extracts its complete source bundle outside the repository, and
# validates it without workspace or worktree state. The optional native profile
# is reserved for certified CI runners and turns any missing or skipped
# certification test into a hard error.
[CmdletBinding()]
param(
    [string]$Version = '',

    [string]$CommitSHA = '',

    [string]$NativeProfile = '',

    [ValidateSet('all', 'verify', 'package')]
    [string]$Mode = 'all'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (Test-Path Variable:PSNativeCommandUseErrorActionPreference) {
    $PSNativeCommandUseErrorActionPreference = $false
}

if ([string]::IsNullOrWhiteSpace($Version) -or $CommitSHA -cnotmatch '^[0-9a-f]{40}$') {
    throw 'usage: scripts/ci/windows/release.ps1 -Version <version> -CommitSHA <40-char-sha> [-NativeProfile windows-ntfs] [-Mode all|verify|package]'
}
if ($NativeProfile -cnotin @('', 'windows-ntfs')) {
    throw "unsupported Windows release native profile: $NativeProfile"
}
if ($Mode -ceq 'package' -and $NativeProfile -cne '') {
    throw 'Windows release package mode does not execute native certification profiles'
}

$runVerify = $Mode -cin @('all', 'verify')
$runPackage = $Mode -cin @('all', 'package')

# The extracted osfs suite legitimately approaches Go's 10-minute default on
# hosted Windows. The workflow job remains the outer hang bound; this package
# timeout prevents cumulative suite work from being mistaken for a stuck test.
$moduleSuiteTestTimeout = '30m'
$windowsNativeWorkerTimeoutMinutes = 35
$windowsProfileDeletionTimeoutMilliseconds = 30000
$windowsProfileDeletionPollMilliseconds = 250
$ciRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $ciRoot)
$repositoryRoot = [IO.Path]::GetFullPath($repositoryRoot)
$originalLocation = Get-Location
$temporaryRoot = ''
$temporaryRootOwnership = $null
$zipPath = ''
$artifactRoot = ''
$releaseRepository = ''
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
$releaseEnvironmentState = $null

$goApplication = @(Get-Command go -CommandType Application -ErrorAction Stop)[0]
$goExecutable = [IO.Path]::GetFullPath($goApplication.Path)
$govulncheckExecutable = ''
if ($runVerify) {
    $govulncheckApplications = @(Get-Command govulncheck -CommandType Application -All -ErrorAction Ignore)
    if ($govulncheckApplications.Count -eq 0) {
        throw 'release requires developer-installed govulncheck on PATH'
    }
    $govulncheckExecutable = [IO.Path]::GetFullPath($govulncheckApplications[0].Path)
}
Import-Module (Join-Path $ciRoot 'release-environment.psm1') -Force
Import-Module (Join-Path $ciRoot 'release-checkout.psm1') -Force
Import-Module (Join-Path $ciRoot 'native-output/windows/certify.psm1') -Force

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

function Remove-OwnedTemporaryRoot {
    if ($NativeProfile -ceq 'windows-ntfs') {
        if ($null -ne $temporaryRootOwnership) {
            Remove-WindowsNativeCoordinatorReleaseRoot -Ownership $temporaryRootOwnership
        }
        return
    }
    if (-not (Test-Path -LiteralPath $temporaryRoot)) {
        return
    }

    $resolvedTemporaryRoot = [IO.Path]::GetFullPath($temporaryRoot)
    $resolvedSystemTemp = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
    $ownedPrefix = Join-Path $resolvedSystemTemp 'windshare-release-'
    if (-not $resolvedTemporaryRoot.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "refusing to remove unowned temporary path: $resolvedTemporaryRoot"
    }
    Remove-Item -LiteralPath $resolvedTemporaryRoot -Recurse -Force
}

function New-EphemeralWindowsUserPassword {
    $password = [Security.SecureString]::new()
    foreach ($character in 'aA1!'.ToCharArray()) {
        $password.AppendChar($character)
    }
    $alphabet = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#$%^&*-_'
    for ($index = 0; $index -lt 28; $index++) {
        $randomIndex = [Security.Cryptography.RandomNumberGenerator]::GetInt32($alphabet.Length)
        $password.AppendChar($alphabet[$randomIndex])
    }
    $password.MakeReadOnly()
    return $password
}

function Grant-EphemeralWindowsUserAccess(
    [string]$Path,
    [string]$UserSID,
    [ValidateSet('RootTraverse', 'ReadExecute', 'MutableDirectory')]
    [string]$AccessProfile
) {
    $icacls = Join-Path $env:SystemRoot 'System32\icacls.exe'
    switch ($AccessProfile) {
        'RootTraverse' {
            & $icacls $Path '/grant:r' "*${UserSID}:RX" '/Q'
        }
        'ReadExecute' {
            & $icacls $Path '/grant:r' "*${UserSID}:(OI)(CI)RX" '/T' '/Q'
        }
        'MutableDirectory' {
            & $icacls $Path '/grant:r' "*${UserSID}:(OI)(CI)(M,DC)" '/T' '/Q'
        }
    }
    if ($LASTEXITCODE -ne 0) {
        throw "grant $AccessProfile access to the ephemeral native worker failed with code $LASTEXITCODE"
    }
}

function Stop-EphemeralWindowsWorkerProcess([Diagnostics.Process]$Process) {
    if ($null -eq $Process -or $Process.HasExited) {
        return
    }

    $taskkill = Join-Path $env:SystemRoot 'System32\taskkill.exe'
    & $taskkill '/PID' $Process.Id '/T' '/F' | Out-Null
    $taskkillExitCode = $LASTEXITCODE
    $processExited = $Process.WaitForExit([int][TimeSpan]::FromSeconds(30).TotalMilliseconds)
    if (-not $processExited) {
        throw 'ephemeral native worker process tree did not terminate within 30 seconds'
    }
    if ($taskkillExitCode -ne 0 -and -not $Process.HasExited) {
        throw "terminate ephemeral native worker process tree failed with code $taskkillExitCode"
    }
}

function Remove-EphemeralWindowsUser([string]$UserName, [string]$UserSID) {
    $cleanupErrors = [Collections.Generic.List[string]]::new()
    if (-not [string]::IsNullOrWhiteSpace($UserSID)) {
        try {
            Remove-WindowsNativeEphemeralUserProfile `
                -UserSID $UserSID `
                -TimeoutMilliseconds $windowsProfileDeletionTimeoutMilliseconds `
                -PollMilliseconds $windowsProfileDeletionPollMilliseconds
        } catch {
            $cleanupErrors.Add("remove ephemeral profile: $($_.Exception.Message)")
        }
    }

    try {
        if ($null -ne (Get-LocalUser -Name $UserName -ErrorAction SilentlyContinue)) {
            Remove-LocalUser -Name $UserName
        }
    } catch {
        $cleanupErrors.Add("remove ephemeral local user: $($_.Exception.Message)")
    }

    try {
        if ($null -ne (Get-LocalUser -Name $UserName -ErrorAction SilentlyContinue)) {
            throw 'ephemeral local user still exists after cleanup'
        }
    } catch {
        $cleanupErrors.Add("verify ephemeral account cleanup: $($_.Exception.Message)")
    }

    if ($cleanupErrors.Count -ne 0) {
        throw ($cleanupErrors -join '; ')
    }
}

function Invoke-RequiredWindowsNativeTestsAsStandardUser {
    $currentIdentity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $currentPrincipal = [Security.Principal.WindowsPrincipal]::new($currentIdentity)
    if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'the Windows/NTFS release gate needs an elevated coordinator to create its standard-user worker'
    }

    $userName = 'WSGate{0}' -f [Guid]::NewGuid().ToString('N').Substring(0, 12)
    $password = New-EphemeralWindowsUserPassword
    $credential = $null
    $userCreated = $false
    $userSID = ''
    $process = $null
    try {
        $user = New-LocalUser `
            -Name $userName `
            -Password $password `
            -AccountNeverExpires `
            -PasswordNeverExpires `
            -UserMayNotChangePassword `
            -Description 'Temporary WindShare native certification worker'
        $userCreated = $true
        $userSID = $user.SID.Value
        $usersGroup = Get-LocalGroup -SID ([Security.Principal.SecurityIdentifier]::new('S-1-5-32-545'))
        $existingUsersMembership = @(Get-LocalGroupMember -Group $usersGroup | Where-Object {
            $_.SID.Value -ceq $userSID
        })
        if ($existingUsersMembership.Count -eq 0) {
            Add-LocalGroupMember -Group $usersGroup -Member $user
        }
        $confirmedUsersMembership = @(Get-LocalGroupMember -Group $usersGroup | Where-Object {
            $_.SID.Value -ceq $userSID
        })
        if ($confirmedUsersMembership.Count -ne 1) {
            throw 'ephemeral native worker is not an unambiguous member of the Users group'
        }
        $administratorsGroup = Get-LocalGroup -SID (
            [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544')
        )
        $unexpectedAdministrator = @(Get-LocalGroupMember -Group $administratorsGroup | Where-Object {
            $_.SID.Value -ceq $userSID
        })
        if ($unexpectedAdministrator.Count -ne 0) {
            throw 'ephemeral native worker unexpectedly belongs to the Administrators group'
        }
        if ($null -eq $temporaryRootOwnership) {
            throw 'native release root ownership was not established before worker access'
        }

        $binariesRoot = Join-Path $temporaryRoot 'windows-native-binaries'
        $workerRoot = Join-Path $temporaryRoot 'windows-native-worker'
        New-Item -ItemType Directory -Path @($binariesRoot, $workerRoot) | Out-Null

        # Precompiling the required test binaries under the coordinator avoids
        # duplicating GOROOT and mirrors Linux loop-ext4 certification.
        $requiredSelectors = @(Get-WindowsNativeRequiredTestSelectors)
        Write-Output '-- compile Windows native certification test binaries'
        foreach ($selector in $requiredSelectors) {
            $binaryOutput = Join-Path $binariesRoot ([string]$selector.BinaryName)
            & $goExecutable test -c -o $binaryOutput ([string]$selector.PackageArgument)
            if ($LASTEXITCODE -ne 0) {
                throw "compile Windows native test binary for $($selector.PackageArgument) exited with code $LASTEXITCODE"
            }
        }

        $workerVerifierPaths = @(
            'scripts/ci/native-output/windows/certify.psm1',
            'scripts/ci/native-output/windows/worker.ps1'
        )
        # Re-prove the two late worker inputs immediately before copying them
        # into the worker's immutable read-only binaries directory.
        Assert-ExactReleaseFileProjection `
            -RepositoryRoot $releaseRepository `
            -ExpectedCommit $CommitSHA `
            -VerifierPaths $workerVerifierPaths
        foreach ($helperPath in $workerVerifierPaths) {
            Copy-Item `
                -LiteralPath (Join-Path $releaseRepository $helperPath) `
                -Destination $binariesRoot
        }

        Grant-EphemeralWindowsUserAccess `
            -Path $temporaryRoot `
            -UserSID $userSID `
            -AccessProfile RootTraverse
        $temporaryRootOwnership.WorkerSID = $userSID
        Grant-EphemeralWindowsUserAccess `
            -Path $binariesRoot `
            -UserSID $userSID `
            -AccessProfile ReadExecute
        $binariesMutationDeny = Set-WindowsNativeTreeMutationDeny `
            -RootPath $binariesRoot `
            -UserSID $userSID `
            -Label 'the native test binaries'
        Write-Output ('-- installed mutation-only deny ACLs: binaries_entries={0}' -f @(
            $binariesMutationDeny.EntryCount
        ))
        # The output root opens a directory handle with FILE_DELETE_CHILD. icacls
        # Modify does not imply that directory right, so grant it explicitly while
        # keeping the release root and immutable binaries read/execute-only.
        Grant-EphemeralWindowsUserAccess `
            -Path $workerRoot `
            -UserSID $userSID `
            -AccessProfile MutableDirectory

        $powershellExecutable = [IO.Path]::GetFullPath((Get-Process -Id $PID).Path)
        $workerScript = Join-Path $binariesRoot 'worker.ps1'
        $workerArgumentLine = New-WindowsNativeWorkerArgumentLine `
            -PowerShellExecutable $powershellExecutable `
            -WorkerScript $workerScript `
            -BinariesRoot $binariesRoot `
            -WorkRoot $workerRoot `
            -ExpectedUserSID $userSID
        $stdoutPath = Join-Path $workerRoot 'worker-stdout.log'
        $stderrPath = Join-Path $workerRoot 'worker-stderr.log'
        $credential = [Management.Automation.PSCredential]::new(
            "$env:COMPUTERNAME\$userName",
            $password
        )

        Write-Output '-- required Windows/NTFS certification under an isolated standard-user token'
        $process = Start-Process `
            -FilePath $powershellExecutable `
            -ArgumentList $workerArgumentLine `
            -Credential $credential `
            -WorkingDirectory $workerRoot `
            -RedirectStandardOutput $stdoutPath `
            -RedirectStandardError $stderrPath `
            -LoadUserProfile `
            -UseNewEnvironment `
            -WindowStyle Hidden `
            -PassThru
        $workerTimeoutMilliseconds = [int][TimeSpan]::FromMinutes(
            $windowsNativeWorkerTimeoutMinutes
        ).TotalMilliseconds
        if (-not $process.WaitForExit($workerTimeoutMilliseconds)) {
            Stop-EphemeralWindowsWorkerProcess -Process $process
            throw "standard-user Windows/NTFS native worker exceeded its $windowsNativeWorkerTimeoutMinutes minute timeout"
        }
        if (Test-Path -LiteralPath $stdoutPath) {
            Get-Content -LiteralPath $stdoutPath | ForEach-Object { Write-Output $_ }
        }
        if (Test-Path -LiteralPath $stderrPath) {
            Get-Content -LiteralPath $stderrPath | ForEach-Object {
                Write-Output "[native worker stderr] $_"
            }
        }

        for ($selectorIndex = 0; $selectorIndex -lt $requiredSelectors.Count; $selectorIndex++) {
            $selector = $requiredSelectors[$selectorIndex]
            $selectorLogID = '{0:D2}' -f ($selectorIndex + 1)
            $rawOutputPath = Join-Path $workerRoot "native-test-raw-$selectorLogID.log"
            $exitCodePath = Join-Path $workerRoot "native-test-exit-$selectorLogID.txt"
            $jsonLogPath = Join-Path $workerRoot "native-test-events-$selectorLogID.jsonl"
            if (-not (Test-Path -LiteralPath $exitCodePath -PathType Leaf)) {
                throw "standard-user Windows/NTFS native worker omitted exit status for $($selector.PackageArgument)"
            }
            $binaryExitCode = [int]([IO.File]::ReadAllText($exitCodePath).Trim())
            $jsonLines = @(ConvertFrom-WindowsNativeTest2JsonOutput `
                -GoExecutable $goExecutable `
                -PackageImportPath ([string]$selector.ImportPath) `
                -RawOutputPath $rawOutputPath)
            $jsonLines | Set-Content -LiteralPath $jsonLogPath -Encoding utf8
            $jsonLines | ForEach-Object { Write-Output $_ }
            $events = @(ConvertFrom-WindowsNativeTestJSONLines -Lines $jsonLines)
            $testExpression = Get-WindowsNativeRequiredTestExpression -TestNames @($selector.TestNames)
            Assert-WindowsNativeTestEvents `
                -ExitCode $binaryExitCode `
                -Events $events `
                -RequiredTests @($selector.TestNames) `
                -SelectorPackage ([string]$selector.PackageArgument) `
                -SelectorExpression $testExpression
        }
        if ($process.ExitCode -ne 0) {
            throw "standard-user Windows/NTFS native worker exited with code $($process.ExitCode)"
        }
        Write-Output '-- required Windows/NTFS certification passed under the standard-user token'
    } finally {
        $cleanupErrors = [Collections.Generic.List[string]]::new()
        try {
            Stop-EphemeralWindowsWorkerProcess -Process $process
        } catch {
            $cleanupErrors.Add("terminate ephemeral worker: $($_.Exception.Message)")
        }
        if ($null -ne $process) {
            $process.Dispose()
        }
        $credential = $null
        try {
            if ($userCreated) {
                Remove-EphemeralWindowsUser -UserName $userName -UserSID $userSID
            }
        } catch {
            $cleanupErrors.Add($_.Exception.Message)
        } finally {
            $password.Dispose()
        }
        if ($cleanupErrors.Count -ne 0) {
            throw ($cleanupErrors -join '; ')
        }
    }
}

Write-Output '== release =='
Assert-ExactReleaseCheckout -RepositoryRoot $repositoryRoot -ExpectedCommit $CommitSHA

try {
    if ($NativeProfile -ceq 'windows-ntfs') {
        $temporaryRootOwnership = New-WindowsNativeCoordinatorReleaseRoot
        $temporaryRoot = $temporaryRootOwnership.RootPath
    } else {
        $temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) (
            'windshare-release-{0}' -f [Guid]::NewGuid().ToString('N')
        )
        New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
    }
    $zipPath = Join-Path $temporaryRoot 'source.zip'
    $artifactRoot = Join-Path $temporaryRoot 'extracted-module'
    $releaseRepository = Join-Path $temporaryRoot 'release-repository'
    $releaseEnvironmentState = Enter-ReleaseGoEnvironment -ReleaseRoot $temporaryRoot
    Set-Location $repositoryRoot
    $currentPowerShell = [IO.Path]::GetFullPath((Get-Process -Id $PID).Path)
    if ($runVerify) {
        Invoke-Step 'exact release checkout contract' {
            & $currentPowerShell -NoLogo -NoProfile -NonInteractive -File `
                (Join-Path $ciRoot 'release-checkout.tests.ps1')
        }
        Invoke-Step 'private Go environment contract' {
            & $currentPowerShell -NoLogo -NoProfile -NonInteractive -File `
                (Join-Path $ciRoot 'release-environment.tests.ps1')
        }
        Invoke-Step 'GOWORK=off go vet release helper' {
            & $goExecutable vet ./scripts/ci/_sourcebundle
        }
        Invoke-Step 'GOWORK=off go test release helper' {
            & $goExecutable test -count=1 ./scripts/ci/_sourcebundle ./scripts/ci/_releaseassets
        }
        Invoke-Step 'Windows first-setup contract (fake firewall commands)' {
            & $currentPowerShell -NoLogo -NoProfile -NonInteractive -File ./scripts/install/windows/network.tests.ps1
        }
        # Helper tests execute repository code. Re-proving the clean exact checkout
        # prevents a test from replacing the later archive builder in place.
        Assert-ExactReleaseCheckout -RepositoryRoot $repositoryRoot -ExpectedCommit $CommitSHA
    }
    Invoke-Step 'materialize private exact-commit verifier checkout' {
        New-ExactReleaseCheckout `
            -SourceRepository $repositoryRoot `
            -ExpectedCommit $CommitSHA `
            -Destination $releaseRepository `
            -VerifierPaths (Get-ReleaseVerifierPaths)
    }
    $sourceBundleDigest = ''
    Invoke-Step "construct deterministic source bundle ($Version at $CommitSHA)" {
        Set-Location $releaseRepository
        $bundleOutput = @(& $goExecutable run ./scripts/ci/_sourcebundle `
            -repo $releaseRepository `
            -commit $CommitSHA `
            -zip $zipPath `
            -extract $artifactRoot `
            -version $Version)
        foreach ($line in $bundleOutput) {
            Write-Output $line
            if ([string]$line -cmatch '^sha256=([0-9a-f]{64})$') {
                $script:sourceBundleDigest = $matches[1]
            }
        }
    }
    if ($sourceBundleDigest -cnotmatch '^[0-9a-f]{64}$') {
        throw 'source bundle builder did not report a valid SHA-256 digest'
    }
    if (-not [string]::IsNullOrWhiteSpace($env:WINDSHARE_RELEASE_SOURCE_SHA256_FILE)) {
        [IO.File]::WriteAllText(
            $env:WINDSHARE_RELEASE_SOURCE_SHA256_FILE,
            "$sourceBundleDigest`n",
            [Text.UTF8Encoding]::new($false)
        )
    }

    $resolvedArtifactRoot = [IO.Path]::GetFullPath($artifactRoot)
    if ($resolvedArtifactRoot.StartsWith(
        $repositoryRoot + [IO.Path]::DirectorySeparatorChar,
        [StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'extracted root module artifact must live outside the repository'
    }
    if (Test-Path -LiteralPath (Join-Path $artifactRoot 'go.work')) {
        throw 'root module artifact must not contain go.work'
    }

    Set-Location $artifactRoot
    Invoke-Step 'pinned provider source and patch reproduction (source bundle)' { & $goExecutable run ./scripts/ci/_piondeps -reproduce }
    if ($runVerify) {
        Invoke-Step 'GOWORK=off go mod tidy -diff (extracted module)' { & $goExecutable mod tidy -diff }
        Invoke-Step 'GOWORK=off go mod verify (extracted module)' { & $goExecutable mod verify }
        Invoke-Step 'GOWORK=off go list ./... (extracted module)' { & $goExecutable list ./... }
        Invoke-Step 'core production dependency boundary (extracted module)' {
            & $goExecutable run ./scripts/ci/_coreboundary
            if ($LASTEXITCODE -ne 0) { throw "Core dependency boundary failed" }
            & $goExecutable run ./scripts/ci/_engineboundary
        }
        Invoke-Step 'GOWORK=off go vet ./... (extracted module)' { & $goExecutable vet ./... }
        Invoke-Step 'GOWORK=off go build ./... (extracted module)' { & $goExecutable build ./... }
        Invoke-Step 'govulncheck (extracted module)' {
            # The setup boundary owns scanner upgrades; the repository depends only
            # on govulncheck's stable source-scan package-pattern contract.
            & $govulncheckExecutable ./...
        }
        $cliInstallRoot = Join-Path $temporaryRoot 'installed-cli'
        New-Item -ItemType Directory -Path $cliInstallRoot | Out-Null
        Invoke-Step 'install wind from the supported source installation entry' {
            & ./scripts/install/windows/install.ps1 -Destination $cliInstallRoot -Firewall Skip `
                -StateDirectory (Join-Path $temporaryRoot 'setup-state')
        }
        Invoke-Step 'execute the installed wind CLI' {
            & (Join-Path $cliInstallRoot 'wind.exe') --help | Out-Null
        }
        if ($NativeProfile -ceq 'windows-ntfs') {
            Invoke-RequiredWindowsNativeTestsAsStandardUser
        }
    }
    if ($runPackage) {
        $assetsRoot = if ([string]::IsNullOrEmpty($env:WINDSHARE_RELEASE_ASSETS)) { Join-Path $temporaryRoot 'assets' } else { $env:WINDSHARE_RELEASE_ASSETS }
        Invoke-Step 'build release binaries and preserve verified source bundle' {
            Set-Location $releaseRepository
            & $goExecutable run ./scripts/ci/_releaseassets -source $artifactRoot -source-zip $zipPath `
                -out $assetsRoot -version $Version -commit $CommitSHA
        }
    }
    if ($runVerify) {
        Set-Location $artifactRoot
        # Module tests execute arbitrary repository code and can mutate their source tree.
        # Keeping them last prevents later consumers from silently validating changed bytes.
        Invoke-Step 'GOWORK=off go test ./... (extracted module)' {
            & $goExecutable test -count=1 "-timeout=$moduleSuiteTestTimeout" ./...
        }
    }
} finally {
    $cleanupErrors = [Collections.Generic.List[string]]::new()
    try {
        Set-Location $originalLocation
    } catch {
        $cleanupErrors.Add("restore location: $($_.Exception.Message)")
    }
    try {
        if ($null -ne $releaseEnvironmentState) {
            Exit-ReleaseGoEnvironment -State $releaseEnvironmentState
        }
    } catch {
        $cleanupErrors.Add("restore Go environment: $($_.Exception.Message)")
    }
    try {
        Remove-OwnedTemporaryRoot
    } catch {
        $cleanupErrors.Add("remove release temp root: $($_.Exception.Message)")
    }
    if ($cleanupErrors.Count -ne 0) {
        throw ($cleanupErrors -join '; ')
    }
}

Write-Output ('== release: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
