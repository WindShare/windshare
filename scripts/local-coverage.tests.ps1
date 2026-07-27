Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-Throws([scriptblock]$Action, [string]$Pattern) {
    try {
        & $Action
    } catch {
        if ([string]$_ -notmatch $Pattern) {
            throw "Unexpected error: $_"
        }
        return
    }
    throw "Expected an error matching: $Pattern"
}

function Write-NetworkManifestFixture([string]$Path, [object]$Document) {
    [IO.File]::WriteAllText(
        $Path,
        ($Document | ConvertTo-Json -Depth 8),
        [Text.UTF8Encoding]::new($false)
    )
}

function Assert-SourceContainsAll([string]$Path, [string]$Label, [string[]]$Expected) {
    $source = [IO.File]::ReadAllText($Path)
    foreach ($text in $Expected) {
        if (-not $source.Contains($text, [StringComparison]::Ordinal)) {
            throw "$Label lost the core suite timeout contract: $text"
        }
    }
}

$coverageScript = Join-Path $PSScriptRoot 'local-coverage.ps1'
Assert-SourceContainsAll $coverageScript 'Local coverage' @(
    "`$coreSuiteTestTimeout = '30m'",
    '"-timeout=$coreSuiteTestTimeout"'
)
Assert-SourceContainsAll (Join-Path $PSScriptRoot 'ci\race.ps1') 'Windows race gate' @(
    "`$coreSuiteTestTimeout = '30m'",
    'go -C core test -race -count=1 "-timeout=$coreSuiteTestTimeout" ./...'
)
Assert-SourceContainsAll (Join-Path $PSScriptRoot 'ci\race.sh') 'Linux race gate' @(
    "core_suite_test_timeout='30m'",
    'go -C core test -race -count=1 -timeout="$core_suite_test_timeout" ./...'
)
Assert-SourceContainsAll (Join-Path $PSScriptRoot 'ci\coverage.sh') 'Linux coverage gate' @(
    "core_suite_test_timeout='30m'",
    'go -C core test -count=1 -timeout="$core_suite_test_timeout" ./...'
)
Assert-SourceContainsAll (Join-Path $PSScriptRoot '..\.github\workflows\ci.yml') 'CI workflow' @(
    'CORE_SUITE_TEST_TIMEOUT: 30m',
    'run: go test -race -count=1 ./...',
    'run: go test -count=1 ./... -covermode=atomic -coverprofile=cover.out',
    'run: go test -race -count=1 -timeout="$CORE_SUITE_TEST_TIMEOUT" ./...',
    'run: go test -count=1 -timeout="$CORE_SUITE_TEST_TIMEOUT" ./... -covermode=atomic -coverprofile=cover.out',
    'run: go -C core test -race -count=1 "-timeout=$env:CORE_SUITE_TEST_TIMEOUT" ./...'
)
$hostIndependentValidationScripts = @(
    $coverageScript,
    (Join-Path $PSScriptRoot 'd5-windows-performance.ps1'),
    (Join-Path $PSScriptRoot 'ci\network.ps1'),
    (Join-Path $PSScriptRoot 'ci\browser.ps1')
)
$forbiddenHostStateCommands = @(
    'Get-CimInstance',
    'Get-CimClass',
    'Invoke-CimMethod',
    'New-CimSession',
    'Get-WmiObject',
    'Invoke-WmiMethod',
    'Get-NetFirewall',
    'Set-NetFirewall',
    'New-NetFirewall',
    'Remove-NetFirewall',
    'netsh.exe',
    'advfirewall'
)
foreach ($validationScript in $hostIndependentValidationScripts) {
    $validationSource = [IO.File]::ReadAllText($validationScript)
    foreach ($forbiddenCommand in $forbiddenHostStateCommands) {
        if ($validationSource.Contains($forbiddenCommand, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Normal validation must not depend on Firewall/WBEM host state: $validationScript contains $forbiddenCommand"
        }
    }
}
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('windshare-local-coverage-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($testRoot)
try {
    foreach ($count in @(1, 3)) {
        $packages = @(
            1..$count | ForEach-Object {
                [ordered]@{ Name = "package-$_"; Path = "./package-$_" }
            }
        )
        $path = Join-Path $testRoot "valid-$count.json"
        Write-NetworkManifestFixture $path ([ordered]@{
            SchemaVersion = 3
            Packages = $packages
        })
        $output = @(& $coverageScript -ValidateNetworkManifestOnly -NetworkManifestPath $path)
        if ($output -notcontains "Validated $count coverage network package(s)") {
            throw "Valid $count-package manifest was not preserved: $output"
        }
    }

    $wrongSchema = Join-Path $testRoot 'wrong-schema.json'
    Write-NetworkManifestFixture $wrongSchema ([ordered]@{
        SchemaVersion = 1
        Packages = @([ordered]@{ Name = 'package'; Path = './package' })
    })
    Assert-Throws {
        & $coverageScript -ValidateNetworkManifestOnly -NetworkManifestPath $wrongSchema
    } 'unsupported schema'

    $missingPath = Join-Path $testRoot 'missing-path.json'
    Write-NetworkManifestFixture $missingPath ([ordered]@{
        SchemaVersion = 3
        Packages = @([ordered]@{ Name = 'package' })
    })
    Assert-Throws {
        & $coverageScript -ValidateNetworkManifestOnly -NetworkManifestPath $missingPath
    } 'exactly Name and Path'
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force
}

Write-Output 'local coverage manifest tests passed'
