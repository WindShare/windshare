# CI-parity browser gate (Windows). ci.yml web-playwright runs two suites:
#  1. The suite-02 receiver matrix — runs here through the frozen D5 runner in
#     BrowserTests mode: web/playwright.config.ts hard-rejects direct
#     invocation on Windows, and the runner provisions the harness contract,
#     lease token and runner pipe the config demands.
#  2. The production Pion WebRTC matrix — direct invocation, same as Linux. Its Go
#     Pion helper is loopback-only (SetIPFilter + SetIncludeLoopbackCandidate,
#     mDNS disabled), so the `go run` temp exe binds no non-loopback socket
#     and Windows Firewall mints no "Query User" rules; the D5 preflight of
#     later gates therefore stays clean. Evidence in
#     docs/.orchestration/make-ci.md "Windows interop enablement".
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repositoryRoot
$gateStopwatch = [Diagnostics.Stopwatch]::StartNew()
Write-Output '== browser =='

Write-Output '-- ensure pinned Chromium/Firefox/WebKit executables'
pnpm -C web exec playwright install chromium firefox webkit
if ($LASTEXITCODE -ne 0) {
    throw "Playwright browser install exited with code $LASTEXITCODE"
}

Write-Output '-- Playwright Chromium/Firefox/WebKit executable preflight'
pnpm -C web run test:browser:preflight
if ($LASTEXITCODE -ne 0) {
    throw "Playwright browser preflight exited with code $LASTEXITCODE"
}

Write-Output '-- D5 runner BrowserTests (main three-engine Playwright suite)'
& (Join-Path $repositoryRoot 'scripts\d5-windows-performance.ps1') -Mode BrowserTests
if ($LASTEXITCODE -ne 0) {
    throw "D5 runner BrowserTests exited with code $LASTEXITCODE"
}

Write-Output '-- Playwright production Pion WebRTC browser matrix'
pnpm -C web exec playwright test --config test/transport/webrtc/browser.playwright.config.ts
if ($LASTEXITCODE -ne 0) {
    throw "Pion WebRTC browser matrix exited with code $LASTEXITCODE"
}

Write-Output ('== browser: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
