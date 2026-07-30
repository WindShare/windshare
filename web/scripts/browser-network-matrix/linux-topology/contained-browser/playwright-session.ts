import type { NetworkMatrixRtcConfiguration } from '../../runtime-authority.ts'
import type { NetworkMatrixBrowser } from '../../vocabulary.ts'
import type { ContainedBrowserSession } from './contracts.ts'

export async function launchPlaywrightBrowser(
  requested: NetworkMatrixBrowser,
): Promise<ContainedBrowserSession> {
  const playwright = await import('@playwright/test')
  const browserType = {
    chromium: playwright.chromium,
    firefox: playwright.firefox,
    webkit: playwright.webkit,
  }[requested]
  const browser = await browserType.launch({ headless: true })
  const engine = browser.browserType().name()
  const page = await browser.newPage()
  return Object.freeze({
    engine,
    createOffer: (configuration: NetworkMatrixRtcConfiguration) =>
      page.evaluate(createOfferInPage, configuration),
    acceptAnswer: (answer: string) => page.evaluate(acceptAnswerInPage, answer),
    exchangeChallenge: (frame: string, deadlineMs: number) => page.evaluate(
      exchangeChallengeInPage,
      { expected: frame, timeoutMs: deadlineMs },
    ),
    getStats: () => page.evaluate(getStatsInPage),
    close: () => browser.close(),
  })
}

async function createOfferInPage(rtc: NetworkMatrixRtcConfiguration): Promise<string> {
  const peer = new RTCPeerConnection({
    iceServers: rtc.iceServers.map((server) => ({
      urls: [...server.urls],
      ...(server.username === null ? {} : { username: server.username }),
      ...(server.credential === null ? {} : { credential: server.credential }),
    })),
    iceTransportPolicy: rtc.iceTransportPolicy,
  })
  const channel = peer.createDataChannel('windshare-network-matrix')
  ;(globalThis as typeof globalThis & {
    __windshareNetworkMatrix?: { peer: RTCPeerConnection; channel: RTCDataChannel }
  }).__windshareNetworkMatrix = { peer, channel }
  const offer = await peer.createOffer()
  await peer.setLocalDescription(offer)
  if (peer.iceGatheringState !== 'complete') {
    await new Promise<void>((resolve) => {
      peer.addEventListener('icegatheringstatechange', () => {
        if (peer.iceGatheringState === 'complete') resolve()
      })
    })
  }
  const local = peer.localDescription
  if (local === null || local.sdp === '') throw new Error('browser offer is unavailable')
  return local.sdp
}

async function acceptAnswerInPage(sdp: string): Promise<void> {
  const state = (globalThis as typeof globalThis & {
    __windshareNetworkMatrix?: { peer: RTCPeerConnection }
  }).__windshareNetworkMatrix
  if (state === undefined) throw new Error('browser peer is unavailable')
  await state.peer.setRemoteDescription({ type: 'answer', sdp })
}

function exchangeChallengeInPage(input: {
  readonly expected: string
  readonly timeoutMs: number
}): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const state = (globalThis as typeof globalThis & {
      __windshareNetworkMatrix?: { channel: RTCDataChannel }
    }).__windshareNetworkMatrix
    if (state === undefined) {
      reject(new Error('browser data channel is unavailable'))
      return
    }
    const timeout = setTimeout(
      () => reject(new Error('browser challenge deadline exceeded')),
      input.timeoutMs,
    )
    const receive = (event: MessageEvent<unknown>): void => {
      clearTimeout(timeout)
      if (event.data !== input.expected) {
        reject(new Error('browser challenge echo was not exact'))
        return
      }
      resolve(event.data)
    }
    state.channel.addEventListener('message', receive, { once: true })
    const send = (): void => state.channel.send(input.expected)
    if (state.channel.readyState === 'open') send()
    else state.channel.addEventListener('open', send, { once: true })
  })
}

async function getStatsInPage(): Promise<unknown[]> {
  const state = (globalThis as typeof globalThis & {
    __windshareNetworkMatrix?: { peer: RTCPeerConnection }
  }).__windshareNetworkMatrix
  if (state === undefined) throw new Error('browser peer is unavailable')
  const records: unknown[] = []
  const report = await state.peer.getStats()
  report.forEach((record) => {
    assertNoSensitiveStatsFields(record)
    const projected = projectPublicStatsRecord(record)
    if (projected !== null) records.push(projected)
  })
  return records

  function assertNoSensitiveStatsFields(value: unknown): void {
    const pending: unknown[] = [value]
    const seen = new WeakSet<object>()
    while (pending.length !== 0) {
      const current = pending.pop()
      if (typeof current !== 'object' || current === null || seen.has(current)) continue
      seen.add(current)
      for (const [key, nested] of Object.entries(current)) {
        const normalized = key.replace(/[^a-z]/giu, '').toLowerCase()
        if (
          normalized.includes('authorization') || normalized.includes('credential') ||
          normalized.includes('password') || normalized.includes('secret') ||
          normalized.includes('token')
        ) throw new Error('browser stats contained a prohibited private field')
        pending.push(nested)
      }
    }
  }

  function projectPublicStatsRecord(value: RTCStats): Record<string, unknown> | null {
    const record = value as RTCStats & Record<string, unknown>
    const nullable = (field: string): unknown => record[field] ?? null
    if (record.type === 'transport') {
      return {
        id: record.id,
        type: record.type,
        selectedCandidatePairId: nullable('selectedCandidatePairId'),
      }
    }
    if (record.type === 'candidate-pair') {
      return {
        id: record.id,
        type: record.type,
        localCandidateId: nullable('localCandidateId'),
        remoteCandidateId: nullable('remoteCandidateId'),
        selected: nullable('selected'),
        nominated: nullable('nominated'),
        state: nullable('state'),
        protocol: nullable('protocol'),
      }
    }
    if (record.type === 'local-candidate' || record.type === 'remote-candidate') {
      return {
        id: record.id,
        type: record.type,
        candidateType: nullable('candidateType'),
        address: nullable('address'),
        ip: nullable('ip'),
        port: nullable('port'),
        protocol: nullable('protocol'),
      }
    }
    return null
  }
}
