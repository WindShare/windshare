import { execFile, spawn } from 'node:child_process'
import { mkdir, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { contentBytes } from './content.mjs'
import { loadCanonicalWorkload } from './workload.mjs'

const execFileAsync = promisify(execFile)
const moduleDirectory = dirname(fileURLToPath(import.meta.url))
const repositoryRoot = resolve(moduleDirectory, '../../../..')
const webRoot = join(repositoryRoot, 'web')
const options = parseArguments(process.argv.slice(2))
const children = []

function parseArguments(argv) {
  const values = { artifacts: null, readyFile: null, vitePort: null, relayPort: null, resultCount: 4 }
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index]
    if (argument === '--artifacts') values.artifacts = argv[++index]
    else if (argument === '--ready-file') values.readyFile = argv[++index]
    else if (argument === '--vite-port') values.vitePort = Number.parseInt(argv[++index], 10)
    else if (argument === '--relay-port') values.relayPort = Number.parseInt(argv[++index], 10)
    else if (argument === '--result-count') values.resultCount = Number.parseInt(argv[++index], 10)
    else throw new Error(`Unknown argument: ${argument}`)
  }
  for (const name of ['artifacts', 'readyFile', 'vitePort', 'relayPort']) {
    if (!values[name]) throw new Error(`Missing required --${name.replace(/[A-Z]/g, letter => `-${letter.toLowerCase()}`)}`)
  }
  if (!Number.isSafeInteger(values.resultCount) || values.resultCount < 1) throw new Error('--result-count must be positive')
  return values
}

async function createSource(root) {
  const canonical = await loadCanonicalWorkload()
  for (const path of canonical.workload.directories) {
    await mkdir(resolve(root, ...path.split('/')), { recursive: true })
  }
  await Promise.all(canonical.workload.files.map(file =>
    writeFile(resolve(root, ...file.path.split('/')), contentBytes(file.ordinal, file.sizeBytes), { flag: 'wx' })))
  return canonical
}

function start(command, arguments_, cwd) {
  const child = spawn(command, arguments_, {
    cwd,
    env: { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' },
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  })
  children.push(child)
  return child
}

function waitForOutput(child, streamName, pattern, timeoutMilliseconds, label) {
  const stream = child[streamName]
  let captured = ''
  return new Promise((resolveReady, rejectReady) => {
    const timeout = setTimeout(() => fail(new Error(`${label} readiness timed out; output=${captured}`)), timeoutMilliseconds)
    const cleanup = () => {
      clearTimeout(timeout)
      stream.off('data', onData)
      child.off('error', fail)
      child.off('exit', onExit)
    }
    const fail = error => {
      cleanup()
      rejectReady(error)
    }
    const onExit = (code, signal) => fail(new Error(`${label} exited before readiness: code=${code} signal=${signal}; output=${captured}`))
    const onData = chunk => {
      captured = (captured + chunk.toString('utf8')).slice(-256 * 1024)
      const match = captured.match(pattern)
      if (match !== null) {
        cleanup()
        resolveReady(match)
      }
    }
    stream.on('data', onData)
    child.once('error', fail)
    child.once('exit', onExit)
  })
}

async function waitForHttp(url, timeoutMilliseconds) {
  const deadline = Date.now() + timeoutMilliseconds
  while (Date.now() < deadline) {
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(1_000) })
      if (response.ok) return
    } catch {}
    await new Promise(resolveDelay => setTimeout(resolveDelay, 100))
  }
  throw new Error(`HTTP readiness timed out: ${url}`)
}

async function buildBinaries(bin) {
  await mkdir(bin, { recursive: true })
  const relay = join(bin, 'wsrelay.exe')
  const wind = join(bin, 'wind.exe')
  await Promise.all([
    execFileAsync('go', ['build', '-o', relay, './relay/cmd/wsrelay'], {
      cwd: repositoryRoot,
      env: { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' },
      timeout: 120_000,
      windowsHide: true,
    }),
    execFileAsync('go', ['build', '-o', wind, './cmd/wind'], {
      cwd: repositoryRoot,
      env: { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' },
      timeout: 120_000,
      windowsHide: true,
    }),
  ])
  return { relay, wind }
}

function capabilityUrl(bareLink, key) {
  const receiver = new URL(bareLink)
  receiver.hash = key
  return receiver.toString()
}

async function startSender(executable, source, relayUrl, frontUrl, ordinal) {
  const child = start(executable, [
    'share', source,
    '--relay', relayUrl,
    '--front-url', frontUrl,
    '--block-size', String(64 * 1024),
    '--split-key',
  ], repositoryRoot)
  const [bare, key] = await Promise.all([
    waitForOutput(child, 'stdout', /^Bare link: (.+)$/mu, 30_000, `sender ${ordinal} bare link`),
    waitForOutput(child, 'stdout', /^Key: (.+)$/mu, 30_000, `sender ${ordinal} key`),
  ])
  return { ordinal, pid: child.pid, url: capabilityUrl(bare[1], key[1]) }
}

async function shutdown() {
  for (const child of [...children].reverse()) {
    if (child.exitCode === null && child.signalCode === null) child.kill()
  }
  await Promise.allSettled(children.map(child => new Promise(resolveExit => {
    if (child.exitCode !== null || child.signalCode !== null) resolveExit()
    else child.once('exit', resolveExit)
  })))
}

try {
  const artifacts = resolve(options.artifacts)
  await mkdir(artifacts, { recursive: true })
  const sourceParent = join(artifacts, 'source-parent')
  await mkdir(sourceParent, { recursive: true })
  const source = join(sourceParent, 'canonical-workload')
  await mkdir(source, { recursive: false })
  const canonical = await createSource(source)
  const binaries = await buildBinaries(join(artifacts, 'bin'))
  const viteUrl = `http://127.0.0.1:${options.vitePort}`
  const vite = start(process.execPath, [
    join(webRoot, 'node_modules', 'vite', 'bin', 'vite.js'),
    '--host', '127.0.0.1',
    '--port', String(options.vitePort),
    '--strictPort',
  ], webRoot)
  await waitForHttp(viteUrl, 30_000)

  const relayUrl = `ws://127.0.0.1:${options.relayPort}`
  const relayState = join(artifacts, 'relay-state')
  await mkdir(relayState, { recursive: true })
  const relay = start(binaries.relay, [
    '-listen', `127.0.0.1:${options.relayPort}`,
    '-state-dir', relayState,
  ], repositoryRoot)
  await waitForOutput(relay, 'stderr', /wsrelay: listening on ([^\s]+) /u, 30_000, 'relay')

  const shares = []
  for (let ordinal = 0; ordinal < options.resultCount; ordinal += 1) {
    shares.push(await startSender(binaries.wind, source, relayUrl, viteUrl, ordinal))
  }
  await writeFile(options.readyFile, `${JSON.stringify({
    schema: 'windshare/fsa-small-file-native-product-stack/v1',
    readyAt: new Date().toISOString(),
    source,
    sourceLeaf: 'canonical-workload',
    resultRootLeaf: 'windshare',
    materializedRootRelativePath: 'windshare/canonical-workload',
    workloadSha256: canonical.sha256,
    viteUrl,
    relayUrl,
    pids: { stack: process.pid, vite: vite.pid, relay: relay.pid, senders: shares.map(share => share.pid) },
    shares,
  }, null, 2)}\n`, { encoding: 'utf8', flag: 'wx' })

  await new Promise(resolveStop => {
    process.once('SIGINT', resolveStop)
    process.once('SIGTERM', resolveStop)
  })
} finally {
  await shutdown()
}
