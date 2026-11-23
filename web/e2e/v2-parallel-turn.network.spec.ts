import { execFile } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'
import { expect, test, type Page } from '@playwright/test'
import { DirectProcess } from './fixtures/direct-process'
import { DirectProductStack, relayReceiverUrl, type DirectShare } from './fixtures/direct-product-stack'
import type { HotSwitchPageEvent } from './fixtures/hot-switch-contract'
import { releasePageOutput, startPageTransfer } from './fixtures/hot-switch-page-transfer'
import { NetworkEventLog } from './fixtures/network-event-log'

const execute = promisify(execFile)
const root = fileURLToPath(new URL('../../', import.meta.url))
const hash = (data: Uint8Array) => createHash('sha256').update(data).digest('hex')
type ServerStats = Array<{ bytes: number; allocations: number }>
type Connectivity = { ApplicationRelayBytes: number; TURNBytes: number; DirectBytes: number }

async function browserDownload(
  page: Page,
  share: DirectShare,
  link: string,
  size: number,
  expected: string,
  timeout = 30_000,
) {
  const events = new NetworkEventLog(timeout)
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
    transferBytes: size,
  })
  await releasePageOutput(page)
  const browser = await events.waitFor('delivery', () => true, 'parallel browser download')
  expect(browser.evidence.receivedSha256).toBe(expected)

  expect(events.snapshot().some((e) => e.kind === 'dispatch' && e.observation.route === 'application-relay')).toBe(true)
  await events.waitFor('runtime-settled', () => true, 'closed browser session')
  return lanes
}

async function startTURNFixture(directory: string) {
  const binary = join(directory, process.platform === 'win32' ? 'turn.exe' : 'turn')
  await execute('go', ['build', '-o', binary, './transport/webrtc/testdata/parallelturn'], {
    cwd: root,
    windowsHide: true,
  })
  const processOwner = new DirectProcess(binary, [], {
    cwd: root,
    operationId: 'parallel-turn-servers',
    disclosure: { stdout: 'capability', stderr: 'private' },
  })
  try {
    const ready = await processOwner.waitFor('stdout', /^\{[^\r\n]+\}$/mu)
    const { services, stats } = JSON.parse(ready[0]) as { services: object[]; stats: string }
    processOwner.consumeReadiness('stdout')
    return { processOwner, services, counters: async () => (await (await fetch(stats)).json()) as ServerStats }
  } catch (error) {
    await processOwner.stop(5000)
    throw error
  }
}

test('Go and Chromium download over two TURN servers and the application relay', async ({ page }) => {
  test.setTimeout(240_000)
  const dir = await mkdtemp(join(tmpdir(), 'windshare-parallel-turn-'))
  const receiver = join(dir, process.platform === 'win32' ? 'receive.exe' : 'receive')
  const stack = new DirectProductStack('parallel-turn')
  let server: DirectProcess | undefined
  let interrupted: DirectProcess | undefined
  try {
    await Promise.all([
      stack.start(),
      execute('go', ['test', '-c', '-o', receiver, './engine'], { cwd: root, windowsHide: true }),
    ])
    const fixture = await startTURNFixture(dir)
    server = fixture.processOwner
    const { services, counters } = fixture
    // Several scheduler windows must remain after the first ICE consent checks.
    const bytes = randomBytes(48 * 1024 * 1024)
    const expected = hash(bytes)
    const name = 'parallel.bin'
    const source = await stack.createFile(name, bytes)
    const share = await stack.share(source, {
      turnServices: services,
      senderTrace: true,
      blockSizeBytes: 256 * 1024,
    })
    const proxy = await stack.createRelayCutProxy(undefined, 3 * 1024 * 1024)
    const link = relayReceiverUrl(share, proxy.url)
    const lanes = await browserDownload(page, share, link, bytes.length, expected)
    expect(lanes.size).toBe(2)
    const before = await counters()
    expect(before.every((s) => s.bytes > 256 * 1024)).toBe(true)
    await expect.poll(async () => (await counters()).map((s) => s.allocations)).toEqual([0, 0])

    const native = await execute(receiver, ['-test.run=^TestTURNNetworkReceive$'], {
      cwd: root,
      windowsHide: true,
      timeout: 65_000,
      env: { ...process.env, WINDSHARE_TURN_TEST_DESTINATION: dir, WINDSHARE_TEST_CAPABILITY: link + '#' + share.key },
    }).then(
      (result) => JSON.parse(result.stdout.split(/\r?\n/).find((line) => line.startsWith('{'))!) as Connectivity,
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

    const trace = (await stack.senderTraceRecords(share)) as ReadonlyArray<{ event: string }>
    expect(trace.some((record) => record.event === 'turn_demand_received')).toBe(true)
    expect(trace.filter((record) => record.event === 'turn_grant_issued').length).toBeGreaterThanOrEqual(4)

    const resumeDir = join(dir, 'recovery')
    await mkdir(resumeDir)
    const finalLane = await stack.share(source, { turnServices: services.slice(0, 1), blockSizeBytes: 256 * 1024 })
    interrupted = new DirectProcess(receiver, ['-test.run=^TestTURNNetworkReceive$'], {
      cwd: root,
      operationId: 'last-turn-resume',
      environment: {
        ...process.env,
        WINDSHARE_TURN_TEST_DESTINATION: resumeDir,
        WINDSHARE_TURN_TEST_RESUME: 'true',
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

const configuration = process.env.WINDSHARE_TURN_SMOKE_CONFIG
if (configuration) {
  test('Chromium downloads in parallel through an isolated Cloudflare Worker and TURN', async ({ page }) => {
    test.setTimeout(180_000)
    const saved = JSON.parse(await readFile(configuration, 'utf8')) as {
      profiles: { url: string; access_key: string; managed_turn: boolean }[]
    }
    const profile = saved.profiles[0]!
    const managed = process.env.WINDSHARE_TURN_SMOKE_SOURCE === 'managed'
    const dir = await mkdtemp(join(tmpdir(), 'windshare-real-turn-'))
    const stack = new DirectProductStack('cloudflare-parallel-turn')
    let fixture: Awaited<ReturnType<typeof startTURNFixture>> | undefined
    try {
      await stack.start()
      if (!managed) fixture = await startTURNFixture(dir)
      const bytes = randomBytes(16 * 1024 * 1024),
        digest = hash(bytes)
      const share = await stack.share(await stack.createFile('cloud.bin', bytes), {
        relayProfile: profile,
        turnServices: fixture?.services ?? [],
        blockSizeBytes: 256 * 1024,
      })
      if (managed) {
        // Only the application relay is shaped. Standard browser ICE uses the actual TURN servers.
        await page.routeWebSocket(
          (url) => url.host === new URL(profile.url).host,
          (route) => {
            const upstream = route.connectToServer()
            let delivery = Promise.resolve()
            upstream.onMessage((message) => {
              const size = typeof message === 'string' ? Buffer.byteLength(message) : message.byteLength
              delivery = delivery
                .then(async () => {
                  if (size > 4096)
                    await new Promise((resolve) => setTimeout(resolve, Math.ceil((size * 1000) / (512 * 1024))))
                  route.send(message)
                })
                .catch(() => {})
            })
          },
        )
      }
      const lanes = await browserDownload(page, share, share.bareLink, bytes.length, digest, 120_000)
      expect(lanes.size).toBe(managed ? 1 : 2)
      if (fixture) expect((await fixture.counters()).every((server) => server.bytes > 256 * 1024)).toBe(true)
      console.info(
        JSON.stringify({
          event: 'cloudflare_parallel_download',
          turnSource: managed ? 'cloudflare' : 'local',
          bytes: bytes.length,
          turnLanes: lanes.size,
          sha256: digest,
        }),
      )
    } finally {
      await Promise.allSettled([stack.dispose(), fixture?.processOwner.stop(5000)])
      await rm(dir, { recursive: true, force: true })
    }
  })
}
