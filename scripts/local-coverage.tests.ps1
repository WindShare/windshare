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

$coverageScript = Join-Path $PSScriptRoot 'local-coverage.ps1'
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
            SchemaVersion = 2
            Packages = $packages
            RetiredProgramTombstone = [ordered]@{}
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
        RetiredProgramTombstone = [ordered]@{}
    })
    Assert-Throws {
        & $coverageScript -ValidateNetworkManifestOnly -NetworkManifestPath $wrongSchema
    } 'unsupported schema'

    $missingPath = Join-Path $testRoot 'missing-path.json'
    Write-NetworkManifestFixture $missingPath ([ordered]@{
        SchemaVersion = 2
        Packages = @([ordered]@{ Name = 'package' })
        RetiredProgramTombstone = [ordered]@{}
    })
    Assert-Throws {
        & $coverageScript -ValidateNetworkManifestOnly -NetworkManifestPath $missingPath
    } 'exactly Name and Path'
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force
}

Write-Output 'local coverage manifest tests passed'
