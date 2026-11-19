import assert from 'node:assert/strict'
import { readFile, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../../..')
const helpers = join(root, 'web/test/.tmp/local-transfer-ab')
const launchers = join(root, '.tmp/local-transfer-tools')
const driverName = 'run-rust-current.mjs'

async function readTemplate(path) {
  return (await readFile(path, 'utf8')).replaceAll('\r\n', '\n')
}

function replace(source, anchor, replacement) {
  const count = typeof anchor === 'string' ? source.split(anchor).length - 1 : [...source.matchAll(anchor)].length
  assert.equal(count, 1, 'Expected one benchmark template anchor: ' + anchor)
  return source.replace(anchor, () => replacement)
}

let driver = await readTemplate(join(helpers, 'run-current.mjs'))
driver = replace(driver, 'mkdir, readFile, writeFile, access', 'mkdir, readFile, writeFile, access, copyFile')
driver = replace(driver, "import { sourceIdentity } from './source-identity.mjs'",
  "import { sourceIdentity } from './source-identity-rust.mjs'\nimport { localRelayArguments } from '../../../scripts/browser-evidence/local-transfer-benchmark/relay.mjs'")
driver = replace(driver, "const measuredVersion = 'current'", `const measuredVersion = process.argv[7] ?? 'current'
if (!['current', 'main', 'workspace'].includes(measuredVersion)) throw new Error('Invalid measured version')
const source = process.env.WINDSHARE_BENCHMARK_SOURCE
if (!source) throw new Error('Set WINDSHARE_BENCHMARK_SOURCE')
process.env.GOWORK = 'off'`)
driver = replace(driver, /source: '[^'\r\n]*', root, version:/g, 'source: resolve(source), root, version:')
driver = replace(driver, "for (const version of ['current'])", 'for (const version of [measuredVersion])')
driver = replace(driver, "  await promisify(execFile)('go', ['build', '-o', join(output, 'wsrelay.exe'), './relay/cmd/wsrelay'], { cwd: root, windowsHide: true })", `  const cargo = await promisify(execFile)('rustup', ['run', 'stable', 'cargo', 'build', '--locked', '--release', '-p', 'windshare-relay-native', '--bin', 'relay-native', '--message-format=json-render-diagnostics'], { cwd: join(root, 'relay-rust'), windowsHide: true, maxBuffer: 16 * 1024 * 1024 })
  await save('cargo-build.log', cargo.stdout + cargo.stderr)
  const artifact = cargo.stdout.split('\\n').filter(line => line.trim()).map(JSON.parse).find(message => message.reason === 'compiler-artifact' && message.target?.name === 'relay-native' && message.executable)
  if (!artifact) throw new Error('Cargo did not produce relay-native')
  await copyFile(artifact.executable, join(output, 'relay-native.exe'))
  report.build = { relay: 'windshare-relay-native', profile: 'release', go: (await promisify(execFile)('go', ['version'], { windowsHide: true })).stdout.trim(), rust: (await promisify(execFile)('rustup', ['run', 'stable', 'rustc', '--version'], { windowsHide: true })).stdout.trim() }`)
driver = replace(driver,
  "  const relay = launch(join(output, 'wsrelay.exe'), ['-listen', '127.0.0.1:0', '-state-dir', join(output, 'relay-state')], 'relay')\n  const relayAddress = await ready(relay, /wsrelay: listening on ([^\\s]+) /)",
  '')
driver = replace(driver, "  const sender = launch(join(output, 'wind.exe'), ['share', report.source, '--relay', 'ws://' + relayAddress,", `  const relay = launch(join(output, 'relay-native.exe'), await localRelayArguments(output, origin), 'relay')
  const readiness = JSON.parse(await ready(relay, /^(\\{[^\\r\\n]*"event":"listening"[^\\r\\n]*\\})\\r?$/m))
  const endpoint = new URL(readiness.endpoint)
  if (endpoint.pathname !== '/v2/ws') throw new Error('Unexpected relay endpoint: ' + endpoint.href)
  report.relay = { endpoint: endpoint.href, profile: 'release' }
  const sender = launch(join(output, 'wind.exe'), ['share', report.source, '--relay', readiness.relay_url,`)
driver = replace(driver, "  report.browser = browser.browser()?.version()", "  report.browser = (await (await browser.newCDPSession(bootstrap)).send('Browser.getVersion')).product")
driver = replace(driver, "const expectedEntry = version === 'baseline' ? '/test/.tmp/sender-auth-baseline/web/src/main.tsx' : '/src/main.tsx'", "const expectedEntry = '/src/main.tsx'")
driver = replace(driver, "if (!report.sourceUnchanged) throw new Error('Source changed during measurement; repeat required')", "if (!report.sourceUnchanged || report.sourceBefore.head !== report.sourceAfter.head || report.sourceBefore.indexSha256 !== report.sourceAfter.indexSha256 || report.sourceBefore.workingTreeStatus !== report.sourceAfter.workingTreeStatus) throw new Error('Checkout changed during measurement; investigate preservation')")
driver = replace(driver, "  for (const item of children.toReversed()) { item.child.kill(); await save(item.label + '.log', item.text) }", `  for (const item of children.toReversed()) {
    if (item.child.exitCode === null && item.child.signalCode === null) {
      const exited = once(item.child, 'exit')
      item.child.kill()
      await exited
    }
    await save(item.label + '.log', item.text)
  }`)

const template = await readTemplate(join(launchers, 'run-local-transfer-current.ps1'))
const replayEntry = '& $replay {'
const replayStart = template.indexOf(replayEntry)
assert(replayStart !== -1, 'Native picker launcher is missing its replay block')
// Reuse the native picker and permission handshake with run-local paths from the caller.
let replay = replace(template.slice(replayStart), 'local-transfer-ab/run-current.mjs', 'local-transfer-ab/' + driverName)
replay = replace(replay, "'native-ready.json') -TimeoutSeconds 240", "'native-ready.json') -TimeoutSeconds 900")
const launcher = String.raw`param(
  [string]$RunId = (Get-Date -Format 'yyyyMMdd-HHmmss'),
  [int]$Repetitions = 3,
  [string]$SourceRoot,
  [string]$Output,
  [string]$Target,
  [ValidateSet('current', 'main', 'workspace')][string]$Version = 'current'
)
$ErrorActionPreference = 'Stop'
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
if (-not $SourceRoot) { $SourceRoot = $repositoryRoot }
if (-not $Output) { $Output = Join-Path $repositoryRoot ".tmp/local-transfer-ab-$($RunId)_stignore" }
if (-not $Target) { $Target = Join-Path ([Environment]::GetFolderPath('UserProfile')) "Downloads/windshare-ab-$RunId" }
$SourceRoot = (Resolve-Path -LiteralPath $SourceRoot).Path
$Output = [IO.Path]::GetFullPath($Output)
$Target = [IO.Path]::GetFullPath($Target)
if (-not $env:WINDSHARE_BENCHMARK_SOURCE) { throw 'Set WINDSHARE_BENCHMARK_SOURCE' }
$env:WINDSHARE_BENCHMARK_SOURCE = (Resolve-Path -LiteralPath $env:WINDSHARE_BENCHMARK_SOURCE).Path
if (-not $env:WINDSHARE_NATIVE_UI_REPLAY_MODULE) { throw 'Set WINDSHARE_NATIVE_UI_REPLAY_MODULE' }
$modulePath = (Resolve-Path -LiteralPath $env:WINDSHARE_NATIVE_UI_REPLAY_MODULE).Path
$replayRoot = [IO.Path]::GetFullPath((Join-Path (Split-Path -Parent $modulePath) '../..'))
if (Test-Path -LiteralPath $Output) { throw 'Evidence output already exists' }
if (Test-Path -LiteralPath $Target) { throw 'Evidence target already exists' }
[void](New-Item -ItemType Directory -Path $Output)
[void](New-Item -ItemType Directory -Path $Target)
$replay = Import-Module $modulePath -Force -PassThru
` + replay

await writeFile(join(helpers, driverName), driver)
await writeFile(join(launchers, 'run-local-transfer-rust-current.ps1'), launcher)
console.log('Prepared the Rust relay benchmark driver and native picker launcher')
