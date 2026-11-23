import { execFile } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { expect, test } from '@playwright/test'
import { DirectProcess } from './fixtures/direct-process'
import { DirectProductStack, relayReceiverUrl } from './fixtures/direct-product-stack'
import type { HotSwitchPageEvent } from './fixtures/hot-switch-contract'
import { releasePageOutput, startPageTransfer } from './fixtures/hot-switch-page-transfer'
import { NetworkEventLog } from './fixtures/network-event-log'

const execute = promisify(execFile)
const root = fileURLToPath(new URL('../../', import.meta.url))
const hash = (data: Uint8Array) => createHash('sha256').update(data).digest('hex')
type ServerStats = Array<{ bytes: number; allocations: number }>
type Connectivity = { ApplicationRelayBytes: number; TURNBytes: number; DirectBytes: number }

test('Go and Chromium download over two TURN servers and the application relay', async ({ page }) => {
  test.setTimeout(240_000)
  const dir = await mkdtemp(join(tmpdir(), 'windshare-parallel-turn-'))
  const binary = join(dir, process.platform === 'win32' ? 'turn.exe' : 'turn')
  const receiver = join(dir, process.platform === 'win32' ? 'receive.exe' : 'receive')
  const stack = new DirectProductStack('parallel-turn')
  let server: DirectProcess | undefined
  let interrupted: DirectProcess | undefined
  try {
    await Promise.all([
      stack.start(),
      execute('go', ['build', '-o', receiver, './engine/testdata/turnreceiver'], { cwd: root, windowsHide: true }),
      execute('go', ['build', '-o', binary, './transport/webrtc/testdata/parallelturn'], {
        cwd: root,
        windowsHide: true,
      }),
    ])
    server = new DirectProcess(binary, [], {
      cwd: root,
      operationId: 'parallel-turn-servers',
      disclosure: { stdout: 'capability', stderr: 'private' },
    })
    const ready = await server.waitFor('stdout', /^\{[^\r\n]+\}$/mu)
    const { services, stats } = JSON.parse(ready[0]) as { services: object[]; stats: string }
    server.consumeReadiness('stdout')
    const counters = async () => (await (await fetch(stats)).json()) as ServerStats
    // Several scheduler windows must remain after the first ICE consent checks.
    const bytes = randomBytes(48 * 1024 * 1024)
    const expected = hash(bytes)
    const name = 'parallel.bin'
    const source = await stack.createFile(name, bytes)
    const share = await stack.share(source, {
      turnServices: services,
      blockSizeBytes: 256 * 1024,
    })
    const proxy = await stack.createRelayCutProxy(undefined, 3 * 1024 * 1024)
    const link = relayReceiverUrl(share, proxy.url)
    const events = new NetworkEventLog()
    const lanes = new Set<number>()
    await page.exposeFunction('__windshareHotSwitchEvent', (e: HotSwitchPageEvent) => {
      if (e.kind === 'dispatch' && e.observation.route === 'turn') lanes.add(e.observation.laneId)
      events.accept(e)
    })
    await page.goto(link)
    await startPageTransfer(page, {
      expectedHash: expected,
      key: share.key,
      nativePeerUsable: true,
      rtcConfiguration: { iceServers: [], iceTransportPolicy: 'relay' },
      transferBytes: bytes.length,
    })
    await releasePageOutput(page)
    const browser = await events.waitFor('delivery', () => true, 'parallel browser download')
    expect(browser.evidence.receivedSha256).toBe(expected)
    const stages = events
      .snapshot()
      .filter((e) => e.kind === 'recovery')
      .map((e) => e.evidence.stage)
    expect(lanes.size, JSON.stringify({ stages, servers: await counters() })).toBe(2)
    expect(events.snapshot().some((e) => e.kind === 'dispatch' && e.observation.route === 'application-relay')).toBe(
      true,
    )
    const before = await counters()
    expect(before.every((s) => s.bytes > 256 * 1024)).toBe(true)
    await events.waitFor('runtime-settled', () => true, 'closed browser session')
    await expect.poll(async () => (await counters()).map((s) => s.allocations)).toEqual([0, 0])

    const native = await execute(receiver, [dir], {
      cwd: root,
      windowsHide: true,
      timeout: 65_000,
      env: { ...process.env, WINDSHARE_TEST_CAPABILITY: link + '#' + share.key },
    }).then(
      (result) => JSON.parse(result.stdout) as Connectivity,
      () => undefined,
    )
    expect(native, 'native download completed').toBeDefined()
    expect(hash(await readFile(join(dir, name)))).toBe(expected)
    expect(native?.DirectBytes).toBe(0)
    expect(native?.ApplicationRelayBytes).toBeGreaterThan(0)
    expect(native?.TURNBytes, JSON.stringify(await counters())).toBeGreaterThan(0)
    const after = await counters()
    expect(after.every((s, i) => s.bytes - before[i]!.bytes > 256 * 1024)).toBe(true)
    await expect.poll(async () => (await counters()).map((s) => s.allocations)).toEqual([0, 0])

    const resumeDir = join(dir, 'recovery')
    await mkdir(resumeDir)
    const finalLane = await stack.share(source, { turnServices: services.slice(0, 1), blockSizeBytes: 256 * 1024 })
    interrupted = new DirectProcess(receiver, [resumeDir, 'resume'], {
      cwd: root,
      operationId: 'last-turn-resume',
      environment: {
        ...process.env,
        WINDSHARE_TEST_CAPABILITY: relayReceiverUrl(finalLane, proxy.url) + '#' + finalLane.key,
      },
      disclosure: { stdout: 'safe', stderr: 'private' },
    })
    const transferred = async () => (await counters())[0]!.bytes - after[0]!.bytes
    await expect.poll(transferred, { timeout: 25_000 }).toBeGreaterThan(2 * 1024 * 1024)
    proxy.setAvailable(false)
    const onlyTURN = await transferred()
    await expect.poll(transferred, { timeout: 15_000 }).toBeGreaterThan(onlyTURN + 2 * 1024 * 1024)
    await server.stop(5000)
    const checkpoint = await interrupted.waitFor('stdout', /^checkpoint (\d+)$/mu, { timeoutMilliseconds: 65_000 })
    expect(Number(checkpoint[1])).toBeGreaterThan(0)
    proxy.setAvailable(true)
    await writeFile(join(resumeDir, 'reconnect'), '')
    const complete = await interrupted.waitFor('stdout', /^\{[^\r\n]+\}$/mu, { timeoutMilliseconds: 40_000 })
    expect(JSON.parse(complete[0]).ResumedFiles).toBe(1)
    expect(hash(await readFile(join(resumeDir, name)))).toBe(expected)
    console.info(
      JSON.stringify({
        scenario: 'parallel-turn',
        sha256: expected,
        bytes: bytes.length,
        browserTurnLanes: lanes.size,
        nativeRelayBytes: native!.ApplicationRelayBytes,
        nativeTurnBytes: native!.TURNBytes,
        recoveredBytes: Number(checkpoint[1]),
      }),
    )
  } finally {
    await Promise.allSettled([interrupted?.stop(5000), stack.dispose(), server?.stop(5000)])
    await rm(dir, { recursive: true, force: true })
  }
})
