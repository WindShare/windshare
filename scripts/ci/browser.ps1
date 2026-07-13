# CI-parity browser gate (Windows). ci.yml web-playwright runs two suites:
#  1. Main M1b+M1c scenario suite — runs here through the frozen D5 runner in
#     BrowserTests mode: web/playwright.config.ts hard-rejects direct
#     invocation on Windows, and the runner provisions the harness contract,
#     lease token and runner pipe the config demands.
#  2. D1/D2 WebRTC interop suite — EXCLUDED on Windows; its coverage lives in
#     CI's ubuntu web-playwright job and scripts/ci/browser.sh. The config
#     carries no Windows gate, but its Go Pion helper starts via `go run`
#     (a fresh random go-build temp exe every run) with the default Pion
#     config — no loopback filter — so it binds ICE sockets on all interfaces
#     and Windows Firewall mints a new "Query User" rule pair for the temp exe
#     on every run. The D5 firewall-ownership preflight hard-fails on exactly
#     such WindShare-attributable random/temp rules, so a direct run here
#     poisons every later coverage/network/browser gate. The D5 runner has no
#     mode for this config; until the helper is made loopback-only (compare
#     benchmarkLoopbackAPI in transport/webrtc/performance_test.go) there is
#     no sanctioned Windows invocation.
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

Write-Output 'note: D1/D2 WebRTC interop suite excluded on Windows (see header); it runs in ci.yml ubuntu web-playwright and scripts/ci/browser.sh'
Write-Output ('== browser: PASS in {0:mm\:ss} ==' -f $gateStopwatch.Elapsed)
