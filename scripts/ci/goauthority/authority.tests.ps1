Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$authorityPath = Join-Path $PSScriptRoot 'authority.psm1'
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'windshare-go-authority-{0}' -f [Guid]::NewGuid().ToString('N')
)
New-Item -ItemType Directory -Path $temporaryRoot | Out-Null

function Invoke-IsolatedAuthority {
    param(
        [hashtable]$Environment = @{}
    )

    $escapedAuthorityPath = $authorityPath.Replace("'", "''", [StringComparison]::Ordinal)
    $pwshApplication = Get-Command pwsh -CommandType Application | Select-Object -First 1
    $process = [Diagnostics.ProcessStartInfo]::new($pwshApplication.Source)
    $process.UseShellExecute = $false
    $process.RedirectStandardOutput = $true
    $process.RedirectStandardError = $true
    foreach ($name in @(
        'GOFLAGS', 'GOWORK', 'GOOS', 'GOARCH', 'GOENV', 'GOTOOLCHAIN', 'GOROOT',
        'WINDSHARE_GO_EXECUTABLE', 'WINDSHARE_GO_AUTHORITY_ACTIVE',
        'WINDSHARE_GO_HOST_OS', 'WINDSHARE_GO_HOST_ARCH'
    )) {
        $null = $process.Environment.Remove($name)
    }
    foreach ($entry in $Environment.GetEnumerator()) {
        $process.Environment[$entry.Key] = [string]$entry.Value
    }
    $process.ArgumentList.Add('-NoProfile')
    $process.ArgumentList.Add('-NonInteractive')
    $process.ArgumentList.Add('-Command')
    $process.ArgumentList.Add(
        "Import-Module '$escapedAuthorityPath' -Force; `$null = Enter-WindShareGoAuthority"
    )
    $child = [Diagnostics.Process]::Start($process)
    $stdout = $child.StandardOutput.ReadToEnd()
    $stderr = $child.StandardError.ReadToEnd()
    $child.WaitForExit()
    return [pscustomobject]@{ ExitCode = $child.ExitCode; Stdout = $stdout; Stderr = $stderr }
}

try {
    $clean = Invoke-IsolatedAuthority
    if ($clean.ExitCode -ne 0) {
        throw "clean Go authority failed: $($clean.Stderr)"
    }

    foreach ($name in @(
        'GOFLAGS', 'GOWORK', 'GOOS', 'GOARCH', 'GOENV', 'GOTOOLCHAIN', 'GOROOT',
        'WINDSHARE_GO_EXECUTABLE', 'WINDSHARE_GO_AUTHORITY_ACTIVE',
        'WINDSHARE_GO_HOST_OS', 'WINDSHARE_GO_HOST_ARCH'
    )) {
        $result = Invoke-IsolatedAuthority -Environment @{ $name = '' }
        if ($result.ExitCode -eq 0 -or -not $result.Stderr.Contains("$name must be absent")) {
            throw "ambient $name did not fail closed: $($result.Stderr)"
        }
    }

    $fakeBin = Join-Path $temporaryRoot 'fake-bin'
    New-Item -ItemType Directory -Path $fakeBin | Out-Null
    [IO.File]::WriteAllText((Join-Path $fakeBin 'go.exe'), 'not-a-pe-application')
    $fakeResult = Invoke-IsolatedAuthority -Environment @{
        PATH = "$fakeBin$([IO.Path]::PathSeparator)$env:PATH"
    }
    if ($fakeResult.ExitCode -eq 0 -or -not $fakeResult.Stderr.Contains('native PE')) {
        throw "fake PATH Go application did not fail closed: $($fakeResult.Stderr)"
    }

    $persistedPath = Join-Path $temporaryRoot 'persisted-env'
    $module = Import-Module $authorityPath -Force -PassThru
    foreach ($name in @('GOFLAGS', 'GOWORK', 'GOOS', 'GOARCH', 'GOENV', 'GOTOOLCHAIN', 'GOROOT')) {
        [IO.File]::WriteAllText($persistedPath, "$name=hostile`n")
        $rejected = $false
        try {
            & $module { param($Path) Assert-NoPersistedGoSelection $Path } $persistedPath
        } catch {
            $rejected = $_.Exception.Message.Contains("$name must not be persisted")
        }
        if (-not $rejected) {
            throw "persisted $name did not fail closed"
        }
    }

    $authority = Enter-WindShareGoAuthority
    $ownedEnvironment = Invoke-WindShareGoConsumer node -e @'
const path = require('node:path')
const executable = process.env.WINDSHARE_GO_EXECUTABLE
if (process.env.GOENV !== 'off' || process.env.GOTOOLCHAIN !== 'local'
    || typeof executable !== 'string' || !path.isAbsolute(executable)) process.exit(91)
'@
    if ($LASTEXITCODE -ne 0) {
        throw "owned Go consumer environment exited with code $LASTEXITCODE"
    }
    if (-not $authority.RetainedStream.CanRead) {
        throw 'Windows Go application was not retained with a live read handle'
    }

    $visibilityFixture = Join-Path $temporaryRoot 'visibility-fixture'
    New-Item -ItemType Directory -Path $visibilityFixture | Out-Null
    [IO.File]::WriteAllText(
        (Join-Path $visibilityFixture 'go.mod'),
        "module example.invalid/windshare-visibility`ngo 1.24`n"
    )
    [IO.File]::WriteAllText(
        (Join-Path $visibilityFixture 'visibility_test.go'),
        @'
package visibility
import ("fmt"; "testing")
func TestPassingScenario(t *testing.T) {
    fmt.Println(`{"schema_version":"windshare.test-event/v1","outcome":"succeeded"}`)
}
'@
    )
    Push-Location $visibilityFixture
    try {
        $visibleRecords = @(
            Invoke-WindShareGoTestJSON -count=1 . | ForEach-Object { $_ | ConvertFrom-Json }
        )
        if ($LASTEXITCODE -ne 0) {
            throw "Go JSON visibility fixture exited with code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
    $visibleScenario = @(
        $visibleRecords |
            Where-Object { $_.Action -ceq 'output' -and $_.Output -is [string] } |
            ForEach-Object {
                try { $_.Output | ConvertFrom-Json } catch { }
            } |
            Where-Object { $_.schema_version -ceq 'windshare.test-event/v1' }
    )
    if ($visibleScenario.Count -ne 1 -or $visibleScenario[0].outcome -cne 'succeeded') {
        throw 'Go JSON wrapper hid or corrupted a passing scenario event'
    }

    $jsonContract = & $module {
        $script:TestJSONCalls = [Collections.Generic.List[object]]::new()
        function Invoke-WindShareGo {
            $script:TestJSONCalls.Add(@($args))
            $global:LASTEXITCODE = 0
            Write-Output '{"Action":"output","Output":"{\"schema_version\":\"windshare.test-event/v1\",\"outcome\":\"succeeded\"}\n"}'
        }
        $output = @(Invoke-WindShareGoTestJSON -count=1 ./integration/...)
        return [pscustomobject]@{
            Arguments = @($script:TestJSONCalls[0])
            CallCount = $script:TestJSONCalls.Count
            Output = @($output)
        }
    }
    if ($jsonContract.CallCount -ne 1 -or
        ($jsonContract.Arguments -join ' ') -cne 'test -json -count=1 ./integration/...' -or
        ($jsonContract.Output -join "`n") -notmatch 'windshare\.test-event/v1') {
        throw 'Go JSON test wrapper did not preserve one visible retained-Go invocation'
    }
    foreach ($ownedArgument in @('test', '-json', '--json', '-json=false', '--json=true')) {
        $rejected = $false
        try {
            & $module { param($Argument) Invoke-WindShareGoTestJSON $Argument ./integration/... } $ownedArgument
        } catch {
            $rejected = $_.Exception.Message.Contains('Go JSON test invocation owns')
        }
        if (-not $rejected) {
            throw "Go JSON test wrapper accepted owned argument: $ownedArgument"
        }
    }
    $emptyRejected = $false
    try {
        & $module { Invoke-WindShareGoTestJSON }
    } catch {
        $emptyRejected = $_.Exception.Message.Contains('explicit test selection')
    }
    if (-not $emptyRejected) {
        throw 'Go JSON test wrapper accepted an empty test selection'
    }
    $env:GOFLAGS = '-run=hidden'
    try {
        $environmentRejected = $false
        try {
            & $module { Invoke-WindShareGoTestJSON ./integration/... }
        } catch {
            $environmentRejected = $_.Exception.Message.Contains('GOFLAGS must be absent')
        }
        if (-not $environmentRejected) {
            throw 'Go JSON test wrapper accepted late GOFLAGS authority'
        }
    } finally {
        Remove-Item Env:GOFLAGS -ErrorAction SilentlyContinue
    }
    $finalCallCount = & $module { $script:TestJSONCalls.Count }
    if ($finalCallCount -ne 1) {
        throw 'Rejected Go JSON test invocations reached the retained Go process'
    }
} finally {
    Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction SilentlyContinue
}

$Error.Clear()
Write-Output 'Go authority Windows tests: PASS'
