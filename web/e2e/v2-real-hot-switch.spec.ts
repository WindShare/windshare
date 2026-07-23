import { createHash } from 'node:crypto'

import { expect, test } from '@playwright/test'

import {
  V2RealStack,
  acquireRealStackBinaries,
  releaseRealStackBinaries,
  replaceRelayHint,
} from './fixtures/v2-real-stack'
import type { BinaryPaths } from './fixtures/windows-stable-runner'
import {
  r8PerformanceSampleCount,
  reportR8Trend,
  summarizeR8Metric,
} from '../test/performance/r8-trend'

const TRANSFER_BYTES = 16 * 1024 * 1024
const STREAM_WRITE_DELAY_MILLISECONDS = 25
const ROUTE_OBSERVATION_TIMEOUT_MILLISECONDS = 60_000
const TRANSFER_COMPLETION_TIMEOUT_MILLISECONDS = 120_000
const STATE_POLL_INTERVAL_MILLISECONDS = 100

interface ObservedRoute {
  readonly laneId: number
  readonly route: 'relay' | 'peer'
  readonly fileId: string
  readonly localBlockIndex: string
}

interface HotSwitchState {
  readonly routes: readonly ObservedRoute[]
  readonly done: boolean
  readonly peerCapable?: boolean
  readonly timings: {
    readonly clickedAt?: number
    readonly peerStartedAt?: number
    readonly firstRelayAt?: number
    readonly firstPeerAt?: number
    relayCutAt?: number
    readonly firstPeerAfterCutAt?: number
    readonly transferCompletedAt?: number
  }
  readonly byteLength?: number
  readonly sha256?: string
  readonly outcome?: string
  readonly error?: string
}

interface HotSwitchTrendSample {
  readonly peerCapable: boolean
  readonly peerAttemptStartMilliseconds: number
  readonly firstRelayByteMilliseconds: number
  readonly firstPeerByteMilliseconds?: number
  readonly peerAfterRelayCutMilliseconds?: number
  readonly transferCompletionMilliseconds: number
}

let binaries: BinaryPaths | undefined

test.use({ trace: 'off', screenshot: 'off', video: 'off' })

test.beforeAll(async () => {
  binaries = await acquireRealStackBinaries()
})

test.afterAll(async () => {
  if (binaries !== undefined) await releaseRealStackBinaries(binaries)
})

test('uses the active runtime peer capability without corrupting one real transfer', async ({
  baseURL,
  browserName,
  page,
}) => {
  if (binaries === undefined) throw new Error('Real-stack binaries are unavailable')
  if (baseURL === undefined) throw new Error('Real-stack browser project requires a base URL')
  const sampleCount = r8PerformanceSampleCount()
  test.setTimeout(
    sampleCount * (
      TRANSFER_COMPLETION_TIMEOUT_MILLISECONDS +
      2 * ROUTE_OBSERVATION_TIMEOUT_MILLISECONDS
    ),
  )
  const expected = deterministicBytes(TRANSFER_BYTES)
  const expectedHash = createHash('sha256').update(expected).digest('hex')
  const samples: HotSwitchTrendSample[] = []
  for (let sample = 0; sample < sampleCount; sample += 1) {
    samples.push(await runHotSwitchSample({
      baseURL,
      expected,
      expectedHash,
      page,
      sample,
    }))
  }
  const peerCapable = samples.every((sample) => sample.peerCapable)
  expect(samples.every((sample) => sample.peerCapable === peerCapable)).toBe(true)
  reportR8Trend({
    browser: browserName,
    scenario: peerCapable ? 'real-relay-to-peer-hot-switch' : 'real-relay-fallback',
    workload: {
      samples: sampleCount,
      bytesPerSample: TRANSFER_BYTES,
      outputWriteDelayMilliseconds: STREAM_WRITE_DELAY_MILLISECONDS,
      authenticatedRouteProvenance: true,
    },
    capabilities: { peerConnection: peerCapable, realRelay: true },
    unavailable: peerCapable
      ? {}
      : { peerConnection: 'RTCPeerConnection is unavailable in this fixed browser runtime' },
    metrics: {
      peerAttemptStartMilliseconds: summarizeR8Metric(
        samples.map((sample) => sample.peerAttemptStartMilliseconds),
      ),
      firstRelayByteMilliseconds: summarizeR8Metric(
        samples.map((sample) => sample.firstRelayByteMilliseconds),
      ),
      transferCompletionMilliseconds: summarizeR8Metric(
        samples.map((sample) => sample.transferCompletionMilliseconds),
      ),
      ...(peerCapable
        ? {
            firstPeerByteMilliseconds: summarizeR8Metric(samples.map((sample) => {
              if (sample.firstPeerByteMilliseconds === undefined) {
                throw new Error('Peer-capable trend sample lost its first peer byte')
              }
              return sample.firstPeerByteMilliseconds
            })),
            peerAfterRelayCutMilliseconds: summarizeR8Metric(samples.map((sample) => {
              if (sample.peerAfterRelayCutMilliseconds === undefined) {
                throw new Error('Peer-capable trend sample lost its post-cut peer byte')
              }
              return sample.peerAfterRelayCutMilliseconds
            })),
          }
        : {}),
    },
  })
})

async function runHotSwitchSample(options: {
  readonly baseURL: string
  readonly expected: Uint8Array
  readonly expectedHash: string
  readonly page: import('@playwright/test').Page
  readonly sample: number
}): Promise<HotSwitchTrendSample> {
  if (binaries === undefined) throw new Error('Real-stack binaries are unavailable')
  const stack = new V2RealStack(binaries)
  try {
    await stack.start()
    const proxy = await stack.createRelayProxy()
    const filePath = await stack.createFile(`hot-switch-${options.sample}.bin`, options.expected)
    const share = await stack.share(filePath, options.baseURL)
    const receiverLink = replaceRelayHint(share.bareLink, proxy.url)

    await options.page.goto(receiverLink)
    await options.page.evaluate(
      ({ key, writeDelay }) => {
        const state: {
          routes: ObservedRoute[]
          done: boolean
          peerCapable?: boolean
          timings: {
            clickedAt?: number
            peerStartedAt?: number
            firstRelayAt?: number
            firstPeerAt?: number
            relayCutAt?: number
            firstPeerAfterCutAt?: number
            transferCompletedAt?: number
          }
          byteLength?: number
          sha256?: string
          outcome?: string
          error?: string
        } = { routes: [], done: false, timings: {} }
        Object.assign(window, { __windshareHotSwitch: state })
        let releasePeer!: () => void
        const peerRelease = new Promise<void>((resolve) => { releasePeer = resolve })
        const waitForPeerRelease = (signal: AbortSignal): Promise<void> => Promise.race([
          peerRelease,
          new Promise<void>((_resolve, reject) => {
            signal.addEventListener('abort', reject, { once: true })
          }),
        ])
        const waitForWriteDelay = (milliseconds: number): Promise<void> => new Promise(
          (resolve) => setTimeout(resolve, milliseconds),
        )

        void (async () => {
          const gatewayPath = '/src/ui/v2-gateway.ts'
          const offerPath = '/src/connectivity/peer-offer.ts'
          const streamPath = '/src/output/streams/single-file.ts'
          const gatewayModule = await import(gatewayPath) as typeof import('../src/ui/v2-gateway')
          const offerModule = await import(offerPath) as typeof import(
            '../src/connectivity/peer-offer'
          )
          const streamModule = await import(streamPath) as typeof import(
            '../src/output/streams/single-file'
          )
          state.peerCapable = offerModule.browserPeerConnectionAvailable()
          let joined: Awaited<ReturnType<
            InstanceType<typeof gatewayModule.V2BrowserReceiverGateway>['join']
          >> | undefined
          let activation: ReturnType<
            NonNullable<typeof joined>['beginDownloadConnectivity']
          > | undefined
          try {
            const realOffers = new offerModule.BrowserOfferChannelFactory()
            const gatedOffers = {
              offer: async (
                route: Parameters<typeof realOffers.offer>[0],
                signal: AbortSignal,
              ) => {
                state.timings.peerStartedAt ??= performance.now()
                const [peer] = await Promise.all([
                  realOffers.offer(route, signal),
                  waitForPeerRelease(signal),
                ])
                return peer
              },
            }
            const gateway = new gatewayModule.V2BrowserReceiverGateway({
              offersFactory: () => gatedOffers,
              onBlockFetched: (observation) => {
                const observedAt = performance.now()
                state.routes.push({
                  laneId: observation.laneId,
                  route: observation.route,
                  fileId: observation.fileId,
                  localBlockIndex: observation.localBlockIndex.toString(),
                })
                if (observation.route === 'relay') state.timings.firstRelayAt ??= observedAt
                if (observation.route === 'peer') {
                  state.timings.firstPeerAt ??= observedAt
                  if (state.timings.relayCutAt !== undefined) {
                    state.timings.firstPeerAfterCutAt ??= observedAt
                  }
                }
                if (state.routes.length === 1 && observation.route === 'relay') releasePeer()
              },
            })
            joined = await gateway.join(key, window.location.href)
            // Negotiation starts at click time so the test cannot consume the
            // production peer deadline. Only lane admission waits for the first
            // relay block, preserving the exact hot-switch boundary under test.
            state.timings.clickedAt = performance.now()
            activation = joined.beginDownloadConnectivity('large')
            const chunks: Uint8Array[] = []
            const output = new streamModule.SingleFileStreamOutputSession(
              `browser-${crypto.randomUUID()}`,
              new WritableStream<Uint8Array>({
                async write(chunk) {
                  chunks.push(chunk.slice())
                  await waitForWriteDelay(writeDelay)
                },
              }),
            )
            const result = await joined.transferJob(output, activation).run()
            state.timings.transferCompletedAt = performance.now()
            const length = chunks.reduce((total, chunk) => total + chunk.byteLength, 0)
            const bytes = new Uint8Array(length)
            let offset = 0
            for (const chunk of chunks) {
              bytes.set(chunk, offset)
              offset += chunk.byteLength
            }
            const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))
            state.byteLength = bytes.byteLength
            state.sha256 = Array.from(
              digest,
              (byte) => byte.toString(16).padStart(2, '0'),
            ).join('')
            state.outcome = result.outcome.status
          } catch (error) {
            state.error = error instanceof Error ? `${error.name}: ${error.message}` : String(error)
          } finally {
            activation?.close()
            await joined?.close().catch(() => undefined)
            state.done = true
          }
        })()
      },
      { key: share.key, writeDelay: STREAM_WRITE_DELAY_MILLISECONDS },
    )

    const firstRouteState = await waitForHotSwitchState(
      options.page,
      'the first authenticated route',
      ROUTE_OBSERVATION_TIMEOUT_MILLISECONDS,
      (state) => state.routes.length > 0,
    )
    const firstRoute = firstRouteState.routes[0]
    if (firstRoute === undefined) throw new Error('Relay block observation disappeared')
    expect(firstRoute.route).toBe('relay')

    if (firstRouteState.peerCapable === undefined) {
      throw new Error('Browser peer capability observation disappeared')
    }
    let firstPeerRoute: ObservedRoute | undefined
    if (firstRouteState.peerCapable) {
      const firstPeerState = await waitForHotSwitchState(
        options.page,
        'an authenticated peer route',
        ROUTE_OBSERVATION_TIMEOUT_MILLISECONDS,
        (state) => state.routes.some((route) => route.route === 'peer'),
      )
      firstPeerRoute = firstPeerState.routes.find((route) => route.route === 'peer')
      if (firstPeerRoute === undefined) throw new Error('P2P block observation disappeared')

      const peerRoutesBeforeCut = (await hotSwitchState(options.page)).routes.filter(
        (route) => route.laneId === firstPeerRoute?.laneId,
      ).length
      proxy.cutConnections()
      await options.page.evaluate(() => {
        const state = (
          window as Window & { __windshareHotSwitch?: HotSwitchState }
        ).__windshareHotSwitch
        if (state === undefined) throw new Error('Hot-switch state is unavailable at relay cut')
        state.timings.relayCutAt = performance.now()
      })

      await waitForHotSwitchState(
        options.page,
        'a peer block after relay loss',
        ROUTE_OBSERVATION_TIMEOUT_MILLISECONDS,
        (state) => state.routes.filter(
          (route) => route.laneId === firstPeerRoute?.laneId,
        ).length > peerRoutesBeforeCut,
      )
    }
    await waitForHotSwitchState(
      options.page,
      'successful transfer completion',
      TRANSFER_COMPLETION_TIMEOUT_MILLISECONDS,
      (state) => state.done,
    )

    const final = await hotSwitchState(options.page)
    expect(final.error).toBeUndefined()
    expect(final.outcome).toBe('Succeeded')
    expect(final.byteLength).toBe(options.expected.byteLength)
    expect(final.sha256).toBe(options.expectedHash)
    expect(new Set(final.routes.map((route) => route.fileId)).size).toBe(1)
    expect(final.routes[0]).toMatchObject({ laneId: firstRoute.laneId, route: 'relay' })
    if (firstRouteState.peerCapable) {
      expect(firstPeerRoute).toMatchObject({ route: 'peer' })
      expect(firstPeerRoute?.laneId).not.toBe(firstRoute.laneId)
    } else {
      expect(final.routes.every((route) => route.route === 'relay')).toBe(true)
      expect(new Set(final.routes.map((route) => route.laneId))).toEqual(
        new Set([firstRoute.laneId]),
      )
    }
    const clickedAt = requiredTiming(final.timings.clickedAt, 'click')
    const peerStartedAt = requiredTiming(final.timings.peerStartedAt, 'peer attempt start')
    const firstRelayAt = requiredTiming(final.timings.firstRelayAt, 'first relay byte')
    const transferCompletedAt = requiredTiming(
      final.timings.transferCompletedAt,
      'transfer completion',
    )
    return Object.freeze({
      peerCapable: firstRouteState.peerCapable,
      peerAttemptStartMilliseconds: peerStartedAt - clickedAt,
      firstRelayByteMilliseconds: firstRelayAt - clickedAt,
      transferCompletionMilliseconds: transferCompletedAt - clickedAt,
      ...(firstRouteState.peerCapable
        ? {
            firstPeerByteMilliseconds: requiredTiming(
              final.timings.firstPeerAt,
              'first peer byte',
            ) - clickedAt,
            peerAfterRelayCutMilliseconds: requiredTiming(
              final.timings.firstPeerAfterCutAt,
              'post-cut peer byte',
            ) - requiredTiming(final.timings.relayCutAt, 'relay cut'),
          }
        : {}),
    })
  } finally {
    await stack.dispose()
  }
}

async function hotSwitchState(
  page: import('@playwright/test').Page,
): Promise<HotSwitchState> {
  return page.evaluate(() => {
    const state = (
      window as Window & { __windshareHotSwitch?: HotSwitchState }
    ).__windshareHotSwitch
    if (state === undefined) throw new Error('Hot-switch state is unavailable')
    return {
      ...state,
      routes: state.routes.map((route) => ({ ...route })),
    }
  })
}

async function waitForHotSwitchState(
  page: import('@playwright/test').Page,
  waitingFor: string,
  timeoutMilliseconds: number,
  ready: (state: HotSwitchState) => boolean,
): Promise<HotSwitchState> {
  const deadline = Date.now() + timeoutMilliseconds
  while (true) {
    const state = await hotSwitchState(page)
    if (state.error !== undefined) throw transferProgressError(state, waitingFor)
    if (ready(state)) return state
    if (state.done) throw transferProgressError(state, waitingFor)
    if (Date.now() >= deadline) {
      throw new Error(
        `Timed out waiting for ${waitingFor}; state=${JSON.stringify(state)}`,
      )
    }
    await page.waitForTimeout(STATE_POLL_INTERVAL_MILLISECONDS)
  }
}

function transferProgressError(state: HotSwitchState, waitingFor: string): Error {
  const reason = state.error ?? 'transfer completed before the expected observation'
  return new Error(
    `Hot-switch transfer cannot reach ${waitingFor}: ${reason}; state=${JSON.stringify(state)}`,
  )
}

function requiredTiming(value: number | undefined, label: string): number {
  if (value === undefined || !Number.isFinite(value)) {
    throw new Error(`Hot-switch trend lost ${label} timing`)
  }
  return value
}

function deterministicBytes(length: number): Uint8Array {
  const bytes = new Uint8Array(length)
  let state = 0x6d2b79f5
  for (let index = 0; index < bytes.length; index += 1) {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
    bytes[index] = state >>> 24
  }
  return bytes
}
