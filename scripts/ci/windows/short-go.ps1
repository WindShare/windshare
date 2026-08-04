[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
Set-Location $repositoryRoot
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
    Join-Path $temporaryRoot ("windshare-short-go-{0}" -f [guid]::NewGuid().ToString('N'))
))
$ownedPrefix = [IO.Path]::GetFullPath((Join-Path $temporaryRoot 'windshare-short-go-'))
if (-not $profileDirectory.StartsWith($ownedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Short-Go profile directory escaped the temporary root: $profileDirectory"
}
[IO.Directory]::CreateDirectory($profileDirectory) | Out-Null
$rootProfile = Join-Path $profileDirectory 'root.cover.out'
$coreProfile = Join-Path $profileDirectory 'core.cover.out'

Write-Output '== short-go =='
try {
    Invoke-Step 'root short race and atomic coverage sweep' {
        go test -short -race -count=1 -covermode=atomic "-coverprofile=$rootProfile" ./...
    }
    Invoke-Step 'root coverage verdict' {
        go-test-coverage --config=.testcoverage.yml "--profile=$rootProfile"
    }
    Invoke-Step 'core short race and atomic coverage sweep' {
        go -C core test -short -race -count=1 -covermode=atomic "-coverprofile=$coreProfile" ./...
    }

    Push-Location (Join-Path $repositoryRoot 'core')
    try {
        Invoke-Step 'core coverage verdict' {
            go-test-coverage --config=.testcoverage.yml "--profile=$coreProfile"
        }
    } finally {
        Pop-Location
    }

    Write-Output ('== short-go: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
} finally {
    if (Test-Path -LiteralPath $profileDirectory -PathType Container) {
        Remove-Item -LiteralPath $profileDirectory -Recurse -Force
    }
}
