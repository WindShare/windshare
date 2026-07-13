# CI-parity browser gate (Windows). ci.yml web-playwright runs two suites:
#  1. Main M1b+M1c scenario suite — runs here through the frozen D5 runner in
#     BrowserTests mode: web/playwright.config.ts hard-rejects direct
#     invocation on Windows, and the runner provisions the harness contract,
#     lease token and runner pipe the config demands.
#  2. D1/D2 WebRTC interop suite — direct invocation, same as Linux. Its Go
#     Pion helper is loopback-only (SetIPFilter + SetIncludeLoopbackCandidate,
#     mDNS disabled), so the `go run` temp exe binds no non-loopback socket
#     and Windows Firewall mints no "Query User" rules; the D5 preflight of
#     later gates therefore stays clean. Evidence in
#     docs/.orchestration/make-ci.md "Windows interop enablement".
# Prerequisite: Chromium installed once via
# `pnpm -C web exec playwright install chromium` (CI reinstalls per run).
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== browser =='

Write-Output '-- D5 runner BrowserTests (main M1b+M1c Playwright suite)'
& (Join-Path $repositoryRoot 'scripts\d5-windows-performance.ps1') -Mode BrowserTests
if ($LASTEXITCODE -ne 0) {
    throw "D5 runner BrowserTests exited with code $LASTEXITCODE"
}

Write-Output '-- Playwright D1/D2 WebRTC interop suite'
pnpm -C web exec playwright test --config test/transport/webrtc/browser.playwright.config.ts
if ($LASTEXITCODE -ne 0) {
    throw "D1/D2 interop Playwright suite exited with code $LASTEXITCODE"
}

Write-Output ('== browser: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
