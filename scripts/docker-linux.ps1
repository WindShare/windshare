[CmdletBinding()]
param(
    [Parameter(Position = 0, ValueFromRemainingArguments = $true)]
    [string[]]$Command,

    [string]$Image = "golang:1.26.5",

    [switch]$Interactive,

    [switch]$NoCache
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
Set-Location $repositoryRoot

# Determine target command inside Linux container
$cmdStr = if ($Command -and $Command.Count -gt 0) { $Command -join ' ' } else { 'check' }

$scriptToRun = switch -Regex ($cmdStr.Trim()) {
    '^vet$' { 'bash scripts/ci/linux/vet.sh' }
    '^lint$' { 'command -v golangci-lint >/dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest; bash scripts/ci/linux/lint.sh' }
    '^coverage$' { 'command -v go-test-coverage >/dev/null || go install github.com/vladopajic/go-test-coverage/v2@latest; bash scripts/ci/linux/coverage.sh' }
    '^short-go$' { 'bash scripts/ci/linux/short-go.sh' }
    '^check$' { 'bash scripts/ci/linux/check.sh' }
    '^hygiene$' { 'bash scripts/ci/linux/hygiene.sh' }
    '^gopls$' { 'bash scripts/ci/linux/gopls.sh' }
    default { $cmdStr }
}

$isInteractive = $Interactive.IsPresent -or ($cmdStr -in @('bash', 'sh'))
$ttyFlags = if ($isInteractive) { '-it' } else { '-i' }

$cacheFlags = @()
if (-not $NoCache) {
    $cacheFlags = @(
        '-v', 'windshare-go-mod-cache:/go/pkg/mod',
        '-v', 'windshare-go-build-cache:/root/.cache/go-build',
        '-v', 'windshare-go-bin-cache:/go/bin'
    )
}

$containerSetup = @'
set -euo pipefail
git config --global --add safe.directory '*'
mkdir -p /ws
cd /src
tar --exclude='./web/node_modules' --exclude='./web/dist' --exclude='./.gemini' -cf - . | (cd /ws && tar -xf -)
cd /ws
git config --global --add safe.directory /ws
'@

$fullBashCommand = if ($isInteractive -and $cmdStr -in @('bash', 'sh')) {
    "$containerSetup`nexec bash"
} else {
    "$containerSetup`n$scriptToRun"
}

$dockerArgs = @(
    'run', '--rm',
    $ttyFlags,
    '-v', "${repositoryRoot}:/src:ro",
    '-e', 'GOBIN=/go/bin',
    '-e', 'PATH=/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
) + $cacheFlags + @(
    $Image,
    'bash', '-c', $fullBashCommand
)

Write-Host "== Running in Linux Container ($Image) ==" -ForegroundColor Cyan
Write-Host "Target: $cmdStr" -ForegroundColor DarkGray

& docker @dockerArgs
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
