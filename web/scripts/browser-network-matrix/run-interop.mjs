import { execFile, spawn } from 'node:child_process'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { createServer } from 'vite'

const execFileAsync = promisify(execFile)
const WEB_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const REPOSITORY_ROOT = resolve(WEB_ROOT, '..')
const SCENARIO_ID = 'browser-pion-interop'
const SERVER_OPERATION_ID = 'pion-browser-server'
const PLAYWRIGHT_OPERATION_ID = 'pion-browser-playwright'
const BUILD_TIMEOUT_MILLISECONDS = 60_000
const READY_TIMEOUT_MILLISECONDS = 20_000
const STOP_TIMEOUT_MILLISECONDS = 5_000
const MAXIMUM_CAPTURED_CHARACTERS = 256 * 1024
const PLAYWRIGHT_CLI = join(WEB_ROOT, 'node_modules', '@playwright', 'test', 'cli.js')
const PLAYWRIGHT_CONFIG = join(
  WEB_ROOT,
  'test',
  'transport',
  'webrtc',
  'browser.playwright.config.ts',
)

async function run() {
  const temporary = await mkdtemp(join(tmpdir(), 'windshare-pion-interop-'))
  let pion
  let vite
  let playwright
  let primaryFailure
  const cleanupFailures = []
  trace(PLAYWRIGHT_OPERATION_ID, 'started')
  try {
    const executable = join(temporary, executableName('pion-browser-server'))
    await execFileAsync(
      process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
      ['build', '-o', executable, './transport/webrtc/testdata/browser/server'],
      {
        cwd: REPOSITORY_ROOT,
        env: localGoEnvironment(),
        timeout: BUILD_TIMEOUT_MILLISECONDS,
        windowsHide: true,
        maxBuffer: 1_000_000,
      },
    )
    pion = await startPionServer(executable)
    vite = await startViteServer(pion.address)
    trace(PLAYWRIGHT_OPERATION_ID, 'listeners-ready', {
      webOrigin: vite.origin,
      pionOrigin: `http://${pion.address}`,
    })
    playwright = startChild(process.execPath, [
      PLAYWRIGHT_CLI,
      'test',
      '--config',
      PLAYWRIGHT_CONFIG,
      '--project',
      'chromium',
    ], {
      cwd: WEB_ROOT,
      environment: {
        ...process.env,
        WINDSHARE_WEB_BASE_URL: vite.origin,
        WINDSHARE_PION_HTTP_ADDRESS: pion.address,
      },
      inheritOutput: true,
      operationId: PLAYWRIGHT_OPERATION_ID,
    })
    const terminal = await Promise.race([playwright.completion, pion.unexpectedExit])
    if (terminal.code !== 0 || terminal.signal !== null) {
      throw new Error(`Playwright failed (${formatTerminal(terminal)})`)
    }
    trace(PLAYWRIGHT_OPERATION_ID, 'completed')
  } catch (error) {
    primaryFailure = error
    trace(PLAYWRIGHT_OPERATION_ID, 'failed', { error: errorMessage(error) })
  } finally {
    if (playwright !== undefined) {
      await playwright.stop().catch((error) => cleanupFailures.push(error))
    }
    if (vite !== undefined) {
      await vite.server.close().catch((error) => cleanupFailures.push(error))
    }
    if (pion !== undefined) {
      await pion.stop().catch((error) => cleanupFailures.push(error))
    }
    await rm(temporary, { recursive: true, force: true })
      .catch((error) => cleanupFailures.push(error))
  }
  if (primaryFailure !== undefined || cleanupFailures.length > 0) {
    throw new AggregateError(
      [...(primaryFailure === undefined ? [] : [primaryFailure]), ...cleanupFailures],
      'Direct browser/Pion interop failed',
    )
  }
}

async function startPionServer(executable) {
  const child = startChild(executable, [], {
    cwd: REPOSITORY_ROOT,
    environment: {
      ...localGoEnvironment(),
      WINDSHARE_D1_BROWSER_ADDR: '127.0.0.1:0',
      WINDSHARE_TEST_SCENARIO: SCENARIO_ID,
      WINDSHARE_TEST_OPERATION_ID: SERVER_OPERATION_ID,
    },
    inheritOutput: false,
    operationId: SERVER_OPERATION_ID,
  })
  try {
    const ready = await withTimeout(
      child.waitForStdout((line) => parseReadyRecord(line)),
      READY_TIMEOUT_MILLISECONDS,
      'Pion browser server readiness',
    )
    trace(SERVER_OPERATION_ID, 'listener-ready', { address: ready.address })
    let stopping = false
    return Object.freeze({
      address: ready.address,
      unexpectedExit: child.completion.then((terminal) => {
        if (stopping) return terminal
        throw new Error(`Pion browser server exited unexpectedly (${formatTerminal(terminal)}); ${child.diagnostic()}`)
      }),
      stop: async () => {
        stopping = true
        await child.stop()
      },
    })
  } catch (error) {
    await child.stop().catch(() => undefined)
    throw error
  }
}

async function startViteServer(pionAddress) {
  const configFile = join(WEB_ROOT, 'test', 'transport', 'webrtc', 'vite.config.ts')
  const previous = process.env.WINDSHARE_PION_HTTP_ADDRESS
  process.env.WINDSHARE_PION_HTTP_ADDRESS = pionAddress
  let server
  try {
    server = await createServer({
      root: WEB_ROOT,
      configFile,
      clearScreen: false,
      logLevel: 'error',
      server: { host: '127.0.0.1', strictPort: true },
    })
    await listenOnEphemeralLoopback(server)
  } finally {
    if (previous === undefined) delete process.env.WINDSHARE_PION_HTTP_ADDRESS
    else process.env.WINDSHARE_PION_HTTP_ADDRESS = previous
  }
  const address = server.httpServer?.address()
  if (address === null || address === undefined || typeof address === 'string') {
    await server.close()
    throw new Error('Vite did not publish its interop listener')
  }
  return Object.freeze({ server, origin: `http://127.0.0.1:${address.port}` })
}

async function listenOnEphemeralLoopback(server) {
  const httpServer = server.httpServer
  if (httpServer === null) throw new Error('Vite did not create its interop HTTP server')
  await new Promise((resolveListen, rejectListen) => {
    const listening = () => {
      httpServer.off('error', failed)
      resolveListen()
    }
    const failed = (error) => {
      httpServer.off('listening', listening)
      rejectListen(error)
    }
    httpServer.once('listening', listening)
    httpServer.once('error', failed)
    // Vite normalizes server.listen(0) to its default port. Its public HTTP
    // server preserves Node's port-zero contract while still running Vite's
    // wrapped initialization path, so parallel direct entries cannot collide.
    void httpServer.listen(0, '127.0.0.1')
  })
}

function startChild(executable, arguments_, options) {
  const child = spawn(executable, arguments_, {
    cwd: options.cwd,
    env: options.environment,
    shell: false,
    windowsHide: true,
    stdio: options.inheritOutput ? 'inherit' : ['ignore', 'pipe', 'pipe'],
  })
  let stopped = false
  let stdout = ''
  let stderr = ''
  const stdoutWaiters = new Set()
  if (!options.inheritOutput) {
    child.stdout.on('data', (chunk) => {
      stdout = bounded(stdout + chunk.toString('utf8'))
      for (const waiter of [...stdoutWaiters]) waiter(stdout)
    })
    child.stderr.on('data', (chunk) => {
      stderr = bounded(stderr + chunk.toString('utf8'))
    })
  }
  const completion = new Promise((resolveCompletion, rejectCompletion) => {
    child.once('error', (error) => rejectCompletion(
      new Error(`${options.operationId} failed to spawn`, { cause: error }),
    ))
    child.once('close', (code, signal) => resolveCompletion({ code, signal }))
  })
  completion.catch(() => undefined)
  return Object.freeze({
    completion,
    diagnostic: () => JSON.stringify({ operationId: options.operationId, stdout, stderr }),
    waitForStdout: (parse) => new Promise((resolveValue, rejectValue) => {
      const inspect = (captured) => {
        for (const line of captured.split(/\r?\n/u)) {
          try {
            const value = parse(line)
            if (value !== null) {
              stdoutWaiters.delete(inspect)
              resolveValue(value)
              return
            }
          } catch (error) {
            stdoutWaiters.delete(inspect)
            rejectValue(error)
            return
          }
        }
      }
      stdoutWaiters.add(inspect)
      inspect(stdout)
      completion.then((terminal) => {
        if (stdoutWaiters.delete(inspect)) {
          rejectValue(new Error(
            `${options.operationId} exited before readiness (${formatTerminal(terminal)}); ` +
            JSON.stringify({ stdout, stderr }),
          ))
        }
      }, rejectValue)
    }),
    stop: async () => {
      if (stopped) return
      stopped = true
      if (child.exitCode !== null || child.signalCode !== null) return
      child.kill()
      await withTimeout(completion, STOP_TIMEOUT_MILLISECONDS, `${options.operationId} cleanup`)
    },
  })
}

function parseReadyRecord(line) {
  if (!line.startsWith('{')) return null
  const value = JSON.parse(line)
  if (
    value?.component !== 'pion-browser-interop-server' ||
    value?.scenarioId !== SCENARIO_ID || value?.operationId !== SERVER_OPERATION_ID ||
    value?.milestone !== 'listener-ready' ||
    typeof value?.address !== 'string' || !/^127\.0\.0\.1:[1-9][0-9]{0,4}$/u.test(value.address)
  ) throw new Error('Pion browser server readiness record is invalid')
  const port = Number(value.address.slice(value.address.lastIndexOf(':') + 1))
  if (!Number.isSafeInteger(port) || port > 65_535) {
    throw new Error('Pion browser server readiness port is invalid')
  }
  return Object.freeze({ address: value.address })
}

async function withTimeout(task, milliseconds, label) {
  let timer
  try {
    return await Promise.race([
      task,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${label} timed out`)), milliseconds)
      }),
    ])
  } finally {
    clearTimeout(timer)
  }
}

function bounded(value) {
  return value.length <= MAXIMUM_CAPTURED_CHARACTERS
    ? value
    : value.slice(-MAXIMUM_CAPTURED_CHARACTERS)
}

function localGoEnvironment() {
  return { ...process.env, GOTOOLCHAIN: 'local' }
}

function executableName(base) {
  return process.platform === 'win32' ? `${base}.exe` : base
}

function formatTerminal(terminal) {
  return terminal.signal === null ? `code=${terminal.code}` : `signal=${terminal.signal}`
}

function trace(operationId, milestone, context = {}) {
  console.info(JSON.stringify({
    component: 'browser-pion-interop-runner',
    scenarioId: SCENARIO_ID,
    operationId,
    milestone,
    ...context,
  }))
}

function errorMessage(error) {
  return error instanceof Error ? error.message : String(error)
}

run().catch((error) => {
  console.error(error)
  process.exitCode = 1
})
