import {
  DATA_CHANNEL_HIGH_WATER_BYTES,
  DATA_CHANNEL_LABEL,
  DATA_CHANNEL_PROTOCOL,
  type WebRTCFrameChannel,
  createWindShareDataChannel,
  wrapWindShareDataChannel,
} from '../../../src/transport/webrtc'

const MAX_FRAME_BYTES = 65_536
const MAXIMUM_BURSTS = 256
const TERMINAL_MARKER = 0xf0
const CANCELED_MARKER = 0xcc
const BARRIER_MARKER = 0xcb

interface FrameSummary {
  readonly marker: number
  readonly size: number
  readonly checksum: number
}

interface PeerSide {
  readonly peer: RTCPeerConnection
  readonly raw: RTCDataChannel
  readonly channel: WebRTCFrameChannel
}

interface PeerPair {
  readonly left: PeerSide
  readonly right: PeerSide
}

interface PionConfig {
  readonly channelLabel: string
  readonly channelProtocol: string
  readonly maxFrameSize: number
  readonly highWaterBytes: number
  readonly clientProbeMarker: number
  readonly clientBurstMarker: number
  readonly clientFinishedMarker: number
  readonly serverProbeMarker: number
  readonly serverBurstMarker: number
  readonly serverFinishedMarker: number
  readonly serverTerminalMarker: number
  readonly canceledSendMarker: number
  readonly terminalFrameBytes: number
}

export interface BrowserLoopbackResult {
  readonly connected: boolean
  readonly exactLeftToRight: boolean
  readonly exactRightToLeft: boolean
  readonly highWaterObserved: boolean
  readonly lowWaterObserved: boolean
  readonly cancellationWaitObserved: boolean
  readonly cancellationError: string
  readonly canceledMarkerReceived: boolean
  readonly barrierReceived: boolean
  readonly terminalLast: boolean
  readonly terminalAcknowledged: boolean
  readonly leftState: string
  readonly rightState: string
}

export interface BrowserRemoteCloseResult {
  readonly highWaterObserved: boolean
  readonly capacityWaitObserved: boolean
  readonly sendError: string
  readonly leftReason: string
  readonly rightReason: string
  readonly lateMarkerReceived: boolean
}

export interface BrowserDataChannelCloseResult {
  readonly leftReason: string
  readonly rightReason: string
  readonly leftState: string
  readonly rightState: string
  readonly leftRawState: string
  readonly rightRawState: string
}

export interface BrowserInvalidConfigurationResult {
  readonly errorName: string
  readonly errorMessage: string
  readonly rawLabel: string
  readonly rawProtocol: string
  readonly rawOrdered: boolean
  readonly rawReliable: boolean
  readonly rawNegotiated: boolean
}

export interface PionInteropResult {
  readonly browser: {
    readonly label: string
    readonly protocol: string
    readonly ordered: boolean
    readonly reliable: boolean
    readonly negotiated: boolean
    readonly maximumMessageSize: number
    readonly highWaterObserved: boolean
    readonly lowWaterObserved: boolean
    readonly cancellationWaitObserved: boolean
    readonly cancellationError: string
    readonly canceledMarkerReceived: boolean
    readonly exactServerProbe: boolean
    readonly serverBurstMessages: number
    readonly serverFinished: boolean
    readonly terminalLast: boolean
    readonly channelState: string
    readonly channelReason: string
    readonly clientBurstMessages: number
    readonly serverFrames: readonly FrameSummary[]
  }
  readonly server: Record<string, unknown>
}

export async function runBrowserLoopback(): Promise<BrowserLoopbackResult> {
  const pair = await createPeerPair()
  const leftFramesPromise = collectFrames(pair.left.channel.frames)
  const rightFramesPromise = collectFrames(pair.right.channel.frames)
  let lowWaterObserved = false
  pair.left.raw.addEventListener('bufferedamountlow', () => {
    lowWaterObserved = true
  }, { once: true })

  const leftProbe = patternedFrame(0x11, MAX_FRAME_BYTES)
  const rightProbe = patternedFrame(0x22, MAX_FRAME_BYTES)
  await pair.left.channel.send(leftProbe)
  await pair.right.channel.send(rightProbe)

  const burst = patternedFrame(0x33, MAX_FRAME_BYTES)
  const burstCount = await fillToHighWater(pair.left.channel, pair.left.raw, burst)
  const highWaterObserved = pair.left.raw.bufferedAmount >= DATA_CHANNEL_HIGH_WATER_BYTES
  const cancellation = new AbortController()
  let cancellationSettled = false
  const canceledSend = pair.left.channel.send(
    Uint8Array.of(CANCELED_MARKER),
    cancellation.signal,
  )
  canceledSend.then(() => {
    cancellationSettled = true
  }).catch(() => undefined)
  await Promise.resolve()
  const cancellationWaitObserved = highWaterObserved && !cancellationSettled
  cancellation.abort(new DOMException('browser cancellation', 'AbortError'))
  const cancellationError = await errorName(canceledSend)

  await pair.left.channel.send(Uint8Array.of(BARRIER_MARKER))
  const terminal = patternedFrame(TERMINAL_MARKER, 257)
  await pair.left.channel.sendTerminal(terminal)
  await Promise.all([pair.left.channel.done, pair.right.channel.done])

  const [leftFrames, rightFrames] = await Promise.all([
    leftFramesPromise,
    rightFramesPromise,
  ])
  await Promise.all([pair.left.channel.close(), pair.right.channel.close()])
  pair.left.peer.close()
  pair.right.peer.close()

  return {
    connected: pair.left.peer.connectionState === 'closed' &&
      pair.right.peer.connectionState === 'closed',
    exactLeftToRight: containsSummary(rightFrames, leftProbe),
    exactRightToLeft: containsSummary(leftFrames, rightProbe),
    highWaterObserved: highWaterObserved && burstCount > 0,
    lowWaterObserved,
    cancellationWaitObserved,
    cancellationError,
    canceledMarkerReceived: rightFrames.some((frame) => frame.marker === CANCELED_MARKER),
    barrierReceived: rightFrames.some((frame) => frame.marker === BARRIER_MARKER),
    terminalLast: summariesEndWith(rightFrames, terminal),
    terminalAcknowledged: pair.left.channel.reason === undefined,
    leftState: pair.left.channel.state,
    rightState: pair.right.channel.state,
  }
}

export async function runBrowserRemoteClose(): Promise<BrowserRemoteCloseResult> {
  const pair = await createPeerPair()
  const rightFramesPromise = collectFrames(pair.right.channel.frames)
  const burst = patternedFrame(0x41, MAX_FRAME_BYTES)
  await fillToHighWater(pair.left.channel, pair.left.raw, burst)
  const highWaterObserved = pair.left.raw.bufferedAmount >= DATA_CHANNEL_HIGH_WATER_BYTES
  let sendSettled = false
  const waiting = pair.left.channel.send(Uint8Array.of(0x42))
  waiting.then(() => {
    sendSettled = true
  }).catch(() => undefined)
  await Promise.resolve()
  const capacityWaitObserved = highWaterObserved && !sendSettled

  pair.right.peer.close()
  const sendError = await errorName(waiting)
  await Promise.all([pair.left.channel.done, pair.right.channel.done])
  const rightFrames = await rightFramesPromise
  await Promise.all([pair.left.channel.close(), pair.right.channel.close()])
  pair.left.peer.close()

  return {
    highWaterObserved,
    capacityWaitObserved,
    sendError,
    leftReason: errorNameOf(pair.left.channel.reason),
    rightReason: errorNameOf(pair.right.channel.reason),
    lateMarkerReceived: rightFrames.some((frame) => frame.marker === 0x42),
  }
}

export async function runBrowserDataChannelClose(): Promise<BrowserDataChannelCloseResult> {
  const pair = await createPeerPair()
  pair.right.raw.close()
  await Promise.all([pair.left.channel.done, pair.right.channel.done])
  await Promise.all([pair.left.channel.close(), pair.right.channel.close()])
  const result: BrowserDataChannelCloseResult = {
    leftReason: errorNameOf(pair.left.channel.reason),
    rightReason: errorNameOf(pair.right.channel.reason),
    leftState: pair.left.channel.state,
    rightState: pair.right.channel.state,
    leftRawState: pair.left.raw.readyState,
    rightRawState: pair.right.raw.readyState,
  }
  pair.left.peer.close()
  pair.right.peer.close()
  return result
}

export function runBrowserInvalidConfiguration(): BrowserInvalidConfigurationResult {
  const peer = new globalThis.RTCPeerConnection({ iceServers: [] })
  const raw = peer.createDataChannel(DATA_CHANNEL_LABEL, {
    ordered: true,
    protocol: `${DATA_CHANNEL_PROTOCOL}-invalid`,
    negotiated: false,
  })
  let error: unknown
  try {
    wrapWindShareDataChannel(peer, raw)
  } catch (caught) {
    error = caught
  }
  const result = {
    errorName: errorNameOf(error),
    errorMessage: error instanceof Error ? error.message : String(error),
    rawLabel: raw.label,
    rawProtocol: raw.protocol,
    rawOrdered: raw.ordered,
    rawReliable: raw.maxPacketLifeTime === null && raw.maxRetransmits === null,
    rawNegotiated: raw.negotiated,
  }
  raw.close()
  peer.close()
  return result
}

export async function runPionInterop(apiBase = '/d2-pion'): Promise<PionInteropResult> {
  const config = await fetchJSON<PionConfig>(`${apiBase}/config`)
  const peer = new globalThis.RTCPeerConnection({ iceServers: [] })
  const raw = createWindShareDataChannel(peer)
  const channel = wrapWindShareDataChannel(peer, raw)
  const serverFramesPromise = collectFrames(channel.frames)
  let lowWaterObserved = false
  raw.addEventListener('bufferedamountlow', () => {
    lowWaterObserved = true
  }, { once: true })

  await negotiateWithPion(peer, apiBase)
  await channel.opened
  await channel.send(patternedFrame(config.clientProbeMarker, config.maxFrameSize))
  const burst = patternedFrame(config.clientBurstMarker, config.maxFrameSize)
  const clientBurstMessages = await fillToHighWater(channel, raw, burst)
  const highWaterObserved = raw.bufferedAmount >= config.highWaterBytes

  const controller = new AbortController()
  let cancellationSettled = false
  const canceledSend = channel.send(
    Uint8Array.of(config.canceledSendMarker),
    controller.signal,
  )
  canceledSend.then(() => {
    cancellationSettled = true
  }).catch(() => undefined)
  await Promise.resolve()
  const cancellationWaitObserved = highWaterObserved && !cancellationSettled
  controller.abort(new DOMException('Pion interop cancellation', 'AbortError'))
  const cancellationError = await errorName(canceledSend)

  await channel.send(Uint8Array.of(config.clientFinishedMarker))
  await channel.done
  const serverFrames = await serverFramesPromise
  const server = await fetchJSON<Record<string, unknown>>(`${apiBase}/result`)
  const serverErrors = stringArray(server['errors'])
  await channel.close()
  const result: PionInteropResult = {
    browser: {
      label: raw.label,
      protocol: raw.protocol,
      ordered: raw.ordered,
      reliable: raw.maxPacketLifeTime === null && raw.maxRetransmits === null,
      negotiated: raw.negotiated,
      maximumMessageSize: peer.sctp?.maxMessageSize ?? 0,
      highWaterObserved,
      lowWaterObserved,
      cancellationWaitObserved,
      cancellationError,
      canceledMarkerReceived: serverErrors.some(
        (error) => error.includes(`marker=0x${config.canceledSendMarker.toString(16)}`),
      ),
      exactServerProbe: containsSummary(
        serverFrames,
        patternedFrame(config.serverProbeMarker, config.maxFrameSize),
      ),
      serverBurstMessages: serverFrames.filter(
        (frame) =>
          frame.marker === config.serverBurstMarker &&
          frame.size === config.maxFrameSize,
      ).length,
      serverFinished: serverFrames.some(
        (frame) => frame.marker === config.serverFinishedMarker && frame.size === 1,
      ),
      terminalLast: serverFrames.at(-1)?.marker === config.serverTerminalMarker &&
        serverFrames.at(-1)?.size === config.terminalFrameBytes,
      channelState: channel.state,
      channelReason: errorNameOf(channel.reason),
      clientBurstMessages,
      serverFrames,
    },
    server,
  }
  peer.close()
  return result
}

async function createPeerPair(): Promise<PeerPair> {
  const leftPeer = new globalThis.RTCPeerConnection({ iceServers: [] })
  const rightPeer = new globalThis.RTCPeerConnection({ iceServers: [] })
  const remoteRaw = deferred<RTCDataChannel>()
  rightPeer.addEventListener('datachannel', (event) => {
    remoteRaw.resolve((event as RTCDataChannelEvent).channel)
  }, { once: true })

  const leftRaw = createWindShareDataChannel(leftPeer)
  const leftChannel = wrapWindShareDataChannel(leftPeer, leftRaw)
  await negotiatePeers(leftPeer, rightPeer)
  const rightRaw = await remoteRaw.promise
  const rightChannel = wrapWindShareDataChannel(rightPeer, rightRaw)
  await Promise.all([leftChannel.opened, rightChannel.opened])
  return {
    left: { peer: leftPeer, raw: leftRaw, channel: leftChannel },
    right: { peer: rightPeer, raw: rightRaw, channel: rightChannel },
  }
}

async function negotiatePeers(
  offerer: RTCPeerConnection,
  answerer: RTCPeerConnection,
): Promise<void> {
  await offerer.setLocalDescription(await offerer.createOffer())
  await waitForIceGathering(offerer)
  await answerer.setRemoteDescription(requiredDescription(offerer))
  await answerer.setLocalDescription(await answerer.createAnswer())
  await waitForIceGathering(answerer)
  await offerer.setRemoteDescription(requiredDescription(answerer))
}

async function negotiateWithPion(peer: RTCPeerConnection, apiBase: string): Promise<void> {
  await peer.setLocalDescription(await peer.createOffer())
  await waitForIceGathering(peer)
  const answer = await fetchJSON<RTCSessionDescriptionInit>(`${apiBase}/offer`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(requiredDescription(peer)),
  })
  await peer.setRemoteDescription(answer)
}

async function waitForIceGathering(peer: RTCPeerConnection): Promise<void> {
  if (peer.iceGatheringState === 'complete') {
    return
  }
  await new Promise<void>((resolve) => {
    const changed = () => {
      if (peer.iceGatheringState !== 'complete') {
        return
      }
      peer.removeEventListener('icegatheringstatechange', changed)
      resolve()
    }
    peer.addEventListener('icegatheringstatechange', changed)
  })
}

async function fillToHighWater(
  channel: WebRTCFrameChannel,
  raw: RTCDataChannel,
  frame: Uint8Array,
): Promise<number> {
  let count = 0
  while (
    raw.bufferedAmount < DATA_CHANNEL_HIGH_WATER_BYTES &&
    count < MAXIMUM_BURSTS
  ) {
    await channel.send(frame)
    count += 1
  }
  return count
}

async function collectFrames(
  stream: ReadableStream<Uint8Array>,
): Promise<FrameSummary[]> {
  const summaries: FrameSummary[] = []
  const reader = stream.getReader()
  while (true) {
    const result = await reader.read()
    if (result.done) {
      return summaries
    }
    summaries.push(summarize(result.value))
  }
}

function patternedFrame(marker: number, size: number): Uint8Array {
  const frame = new Uint8Array(size)
  if (size === 0) {
    return frame
  }
  frame[0] = marker
  for (let index = 1; index < size; index += 1) {
    frame[index] = (index * 31 + 17) % 251
  }
  return frame
}

function summarize(frame: Uint8Array): FrameSummary {
  let checksum = 0
  for (const value of frame) {
    checksum = (checksum + value) >>> 0
  }
  return { marker: frame[0] ?? 0, size: frame.byteLength, checksum }
}

function containsSummary(summaries: readonly FrameSummary[], frame: Uint8Array): boolean {
  const expected = summarize(frame)
  return summaries.some(
    (candidate) =>
      candidate.marker === expected.marker &&
      candidate.size === expected.size &&
      candidate.checksum === expected.checksum,
  )
}

function summariesEndWith(
  summaries: readonly FrameSummary[],
  frame: Uint8Array,
): boolean {
  const expected = summarize(frame)
  const last = summaries.at(-1)
  return last?.marker === expected.marker &&
    last.size === expected.size &&
    last.checksum === expected.checksum
}

function requiredDescription(peer: RTCPeerConnection): RTCSessionDescription {
  if (peer.localDescription === null) {
    throw new Error('local description is unavailable after ICE gathering')
  }
  return peer.localDescription
}

async function fetchJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await globalThis.fetch(url, init)
  const text = await response.text()
  if (!response.ok) {
    throw new Error(`${init?.method ?? 'GET'} ${url} failed: ${text}`)
  }
  return JSON.parse(text) as T
}

function stringArray(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return []
  }
  return value.map((entry) => String(entry))
}

async function errorName(promise: Promise<unknown>): Promise<string> {
  try {
    await promise
    return 'none'
  } catch (error) {
    return errorNameOf(error)
  }
}

function errorNameOf(error: unknown): string {
  if (error instanceof Error) {
    return error.name
  }
  if (error === undefined) {
    return 'none'
  }
  return String(error)
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((accept) => {
    resolve = accept
  })
  return { promise, resolve }
}
