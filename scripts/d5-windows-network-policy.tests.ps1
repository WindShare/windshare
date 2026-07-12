Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repositoryRoot
try {
    & go run ./scripts/internal/d5networkpolicy `
        -root $repositoryRoot `
        -manifest scripts/d5-windows-network-packages.json
    if ($LASTEXITCODE -ne 0) {
        throw "D5 Windows network classification exited with code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}
