[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot

go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Push-Location core
try {
    go vet ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally {
    Pop-Location
}
pnpm -C web lint
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
pnpm -C web exec tsc -b --force
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
pnpm -C web run test:unit:remainder
exit $LASTEXITCODE
