[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
. scripts/ci/windows/go-package-sets.ps1
$nonCorePackages = Get-WindShareGoPackageSet -Set non-core
$corePackages = Get-WindShareGoPackageSet -Set core
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()

function Invoke-Step([string]$Label, [scriptblock]$Body) {
    Write-Output "-- $Label"
    $global:LASTEXITCODE = 0
    & $Body
    if ($LASTEXITCODE -ne 0) {
        throw "$Label exited with code $LASTEXITCODE"
    }
}

$temporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$profileDirectory = [IO.Path]::GetFullPath((
    Join-Path $temporaryRoot ("windshare-coverage-{0}" -f [guid]::NewGuid().ToString('N'))
))
$ownedPrefix = [IO.Path]::GetFullPath((Join-Path $temporaryRoot 'windshare-coverage-'))
if (-not $profileDirectory.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Coverage profile directory escaped the temporary root: $profileDirectory"
}
[IO.Directory]::CreateDirectory($profileDirectory) | Out-Null
$rootProfile = Join-Path $profileDirectory 'root.cover.out'
$coreProfile = Join-Path $profileDirectory 'core.cover.out'

Write-Output '== coverage =='
try {
    Invoke-Step 'non-core short atomic coverage sweep' {
        go test -short -count=1 -covermode=atomic "-coverprofile=$rootProfile" $nonCorePackages
    }
    Invoke-Step 'non-core coverage verdict' {
        go-test-coverage --config=.testcoverage.yml "--profile=$rootProfile"
    }
    Invoke-Step 'core short atomic coverage sweep' {
        go test -short -count=1 -covermode=atomic "-coverprofile=$coreProfile" $corePackages
    }
    Invoke-Step 'core coverage verdict' {
        go-test-coverage --config=core/.testcoverage.yml "--profile=$coreProfile" --source-dir=core
    }

    Write-Output ('== coverage: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
} finally {
    if (Test-Path -LiteralPath $profileDirectory -PathType Container) {
        Remove-Item -LiteralPath $profileDirectory -Recurse -Force
    }
}
