import {
  BLOCK_HEADER_BYTES,
  MAX_BLOCK_PAYLOAD_BYTES,
  splitBlockCiphertext,
} from '../../src/session'
import {
  DATA_CHANNEL_HIGH_WATER_BYTES,
  DATA_CHANNEL_LOW_WATER_BYTES,
  type WebRTCFrameChannel,
  createWindShareDataChannel,
  wrapWindShareDataChannel,
} from '../../src/transport/webrtc'

const KIB = 1024
const MIB = 1024 * KIB
const DEFAULT_FIXTURE_BYTES = 64 * MIB
const PEER_SETUP_TIMEOUT_MS = 30_000
const PEER_CLEANUP_TIMEOUT_MS = 10_000
const TRANSFER_TIMEOUT_MS = 2 * 60_000

interface PerformanceMemory {
  readonly usedJSHeapSize: number
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

export interface RTCStatsLike {
  readonly id: string
  readonly type: string
  readonly [key: string]: unknown
}

export interface ICECandidateEvidence {
  readonly id: string
  readonly address: string
  readonly port: number
  readonly protocol: string
  readonly candidateType: string
  readonly networkType: string | null
  readonly relayProtocol: string | null
}

export interface SelectedCandidatePairEvidence {
  readonly id: string
  readonly state: string
  readonly nominated: boolean
  readonly bytesSent: number
  readonly bytesReceived: number
  readonly currentRoundTripTimeSeconds: number | null
  readonly local: ICECandidateEvidence
  readonly remote: ICECandidateEvidence
}

export interface BrowserPerformanceResult {
  readonly chunkBytes: number
  readonly fixtureBytes: number
  readonly chunks: number
  readonly framesPerChunk: number
  readonly frames: number
  readonly wireBytes: number
  readonly receivedWireBytes: number
  readonly elapsedMs: number
  readonly throughputMiBps: number
  readonly peakBufferedBytes: number
  readonly highWaterObserved: boolean
  readonly lowWaterEvents: number
  readonly lowWaterBytes: number
  readonly highWaterBytes: number
  readonly maximumMessageBytes: number
  readonly heapBeforeBytes: number
  readonly heapAfterBytes: number
  readonly userAgent: string
  readonly selectedCandidatePair: SelectedCandidatePairEvidence
}

export async function runBrowserPerformance(
  chunkBytes: number,
  fixtureBytes = DEFAULT_FIXTURE_BYTES,
): Promise<BrowserPerformanceResult> {
  validateGeometry(chunkBytes, fixtureBytes)
  const block = patternedBlock(chunkBytes)
  const chunkFrames = splitBlockCiphertext(0n, block)
  const framesPerChunk = chunkFrames.length
  const chunks = fixtureBytes / chunkBytes
  const totalFrames = chunks * framesPerChunk
  const wireBytesPerChunk = chunkFrames.reduce(
    (total, frame) => total + frame.byteLength,
    0,
  )
  const expectedWireBytes = chunks * wireBytesPerChunk
  const expectedFrameCount = Math.ceil(chunkBytes / MAX_BLOCK_PAYLOAD_BYTES)
  if (framesPerChunk !== expectedFrameCount) {
    throw new Error(`frames per chunk = ${framesPerChunk}, want ${expectedFrameCount}`)
  }
  if (wireBytesPerChunk !== chunkBytes + framesPerChunk * BLOCK_HEADER_BYTES) {
    throw new Error('BLOCK wire-byte accounting does not match production framing')
  }

  const pair = await createPeerPair()
  const selectedCandidatePair = await readSelectedCandidatePair(pair.left.peer)
  let lowWaterEvents = 0
  let peakBufferedBytes = 0
  let highWaterObserved = false
  pair.left.raw.addEventListener('bufferedamountlow', () => {
    lowWaterEvents += 1
  })
  const receiver = collectExactFrames(
    pair.right.channel,
    chunkFrames,
    totalFrames,
  )
  receiver.catch(() => undefined)
  const transfer = new AbortController()
  const transferTimeout = setTimeout(() => {
    transfer.abort(new DOMException('benchmark transfer timed out', 'TimeoutError'))
  }, TRANSFER_TIMEOUT_MS)
  const heapBeforeBytes = usedJSHeapSize()
  const started = performance.now()
  try {
    for (let chunk = 0; chunk < chunks; chunk += 1) {
      for (const frame of chunkFrames) {
        await pair.left.channel.send(frame, transfer.signal)
        const buffered = pair.left.raw.bufferedAmount
        peakBufferedBytes = Math.max(peakBufferedBytes, buffered)
        highWaterObserved ||= buffered >= DATA_CHANNEL_HIGH_WATER_BYTES
      }
    }
    const receivedWireBytes = await receiver
    const elapsedMs = performance.now() - started
    const heapAfterBytes = usedJSHeapSize()
    if (receivedWireBytes !== expectedWireBytes) {
      throw new Error(
        `received ${receivedWireBytes} wire bytes, want ${expectedWireBytes}`,
      )
    }
    return {
      chunkBytes,
      fixtureBytes,
      chunks,
      framesPerChunk,
      frames: totalFrames,
      wireBytes: expectedWireBytes,
      receivedWireBytes,
      elapsedMs,
      throughputMiBps: fixtureBytes / MIB / (elapsedMs / 1_000),
      peakBufferedBytes,
      highWaterObserved,
      lowWaterEvents,
      lowWaterBytes: DATA_CHANNEL_LOW_WATER_BYTES,
      highWaterBytes: DATA_CHANNEL_HIGH_WATER_BYTES,
      maximumMessageBytes: pair.left.peer.sctp?.maxMessageSize ?? 0,
      heapBeforeBytes,
      heapAfterBytes,
      userAgent: navigator.userAgent,
      selectedCandidatePair,
    }
  } finally {
    clearTimeout(transferTimeout)
    await closePair(pair)
  }
}

async function readSelectedCandidatePair(
  peer: RTCPeerConnection,
): Promise<SelectedCandidatePairEvidence> {
  const report = await peer.getStats()
  const stats: RTCStatsLike[] = []
  report.forEach((stat) => stats.push(stat as RTCStatsLike))
  return selectedCandidatePairFromStats(stats)
}

// The transport's selectedCandidatePairId is the topology oracle. A nominated
// candidate pair alone is not enough because more than one succeeded pair can be
// present while ICE switches paths.
export function selectedCandidatePairFromStats(
  stats: readonly RTCStatsLike[],
): SelectedCandidatePairEvidence {
  const byID = new Map(stats.map((stat) => [stat.id, stat]))
  const selectedIDs = new Set(
    stats
      .filter((stat) => stat.type === 'transport')
      .map((stat) => stat.selectedCandidatePairId)
      .filter((id): id is string => typeof id === 'string' && id.length > 0),
  )
  if (selectedIDs.size !== 1) {
    throw new Error(
      `WebRTC stats expose ${selectedIDs.size} selected candidate pairs; want exactly one`,
    )
  }
  const selectedID = [...selectedIDs][0]
  const pair = selectedID === undefined ? undefined : byID.get(selectedID)
  if (pair === undefined || pair.type !== 'candidate-pair') {
    throw new Error(`selected ICE candidate pair ${selectedID ?? '<missing>'} is unavailable`)
  }
  if (pair.state !== 'succeeded' || pair.nominated !== true) {
    throw new Error(
      `selected ICE candidate pair ${pair.id} is not a nominated succeeded pair`,
    )
  }
  const localID = requiredString(pair, 'localCandidateId')
  const remoteID = requiredString(pair, 'remoteCandidateId')
  return {
    id: pair.id,
    state: pair.state,
    nominated: true,
    bytesSent: optionalNumber(pair, 'bytesSent') ?? 0,
    bytesReceived: optionalNumber(pair, 'bytesReceived') ?? 0,
    currentRoundTripTimeSeconds: optionalNumber(pair, 'currentRoundTripTime'),
    local: candidateEvidence(byID.get(localID), localID),
    remote: candidateEvidence(byID.get(remoteID), remoteID),
  }
}

function candidateEvidence(
  stat: RTCStatsLike | undefined,
  id: string,
): ICECandidateEvidence {
  if (stat === undefined ||
    (stat.type !== 'local-candidate' && stat.type !== 'remote-candidate')) {
    throw new Error(`selected ICE candidate ${id} is unavailable`)
  }
  const address = typeof stat.address === 'string'
    ? stat.address
    : requiredString(stat, 'ip')
  return {
    id: stat.id,
    address,
    port: requiredNumber(stat, 'port'),
    protocol: requiredString(stat, 'protocol'),
    candidateType: requiredString(stat, 'candidateType'),
    networkType: optionalString(stat, 'networkType'),
    relayProtocol: optionalString(stat, 'relayProtocol'),
  }
}

function requiredString(stat: RTCStatsLike, key: string): string {
  const value = stat[key]
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`WebRTC stat ${stat.id} has no ${key}`)
  }
  return value
}

function optionalString(stat: RTCStatsLike, key: string): string | null {
  const value = stat[key]
  return typeof value === 'string' && value.length > 0 ? value : null
}

function requiredNumber(stat: RTCStatsLike, key: string): number {
  const value = optionalNumber(stat, key)
  if (value === null) {
    throw new Error(`WebRTC stat ${stat.id} has no ${key}`)
  }
  return value
}

function optionalNumber(stat: RTCStatsLike, key: string): number | null {
  const value = stat[key]
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

function validateGeometry(chunkBytes: number, fixtureBytes: number): void {
  if (
    !Number.isSafeInteger(chunkBytes) ||
    chunkBytes < KIB ||
    chunkBytes > 4 * MIB
  ) {
    throw new RangeError('chunk bytes must be an integer in [1 KiB, 4 MiB]')
  }
  if (
    !Number.isSafeInteger(fixtureBytes) ||
    fixtureBytes <= 0 ||
    fixtureBytes % chunkBytes !== 0
  ) {
    throw new RangeError('fixture bytes must be a positive multiple of chunk bytes')
  }
}

function patternedBlock(chunkBytes: number): Uint8Array {
  const block = new Uint8Array(chunkBytes)
  for (let index = 0; index < block.byteLength; index += 1) {
    block[index] = (index * 29 + chunkBytes) % 251
  }
  return block
}

async function collectExactFrames(
  channel: WebRTCFrameChannel,
  expected: readonly Uint8Array[],
  totalFrames: number,
): Promise<number> {
  const reader = channel.frames.getReader()
  let receivedBytes = 0
  try {
    for (let index = 0; index < totalFrames; index += 1) {
      const result = await reader.read()
      if (result.done) {
        throw new Error(`receive stream closed after ${index} frames`)
      }
      const want = expected[index % expected.length]
      if (want === undefined || !equalBytes(result.value, want)) {
        throw new Error(`frame ${index} differs from the encoded BLOCK fixture`)
      }
      receivedBytes += result.value.byteLength
    }
    return receivedBytes
  } finally {
    reader.releaseLock()
  }
}

function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) {
    return false
  }
  for (let index = 0; index < left.byteLength; index += 1) {
    if (left[index] !== right[index]) {
      return false
    }
  }
  return true
}

async function createPeerPair(): Promise<PeerPair> {
  const leftPeer = new RTCPeerConnection({ iceServers: [] })
  const rightPeer = new RTCPeerConnection({ iceServers: [] })
  try {
    return await withTimeout((async () => {
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
    })(), PEER_SETUP_TIMEOUT_MS, 'benchmark peer setup')
  } catch (error) {
    leftPeer.close()
    rightPeer.close()
    throw error
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

async function waitForIceGathering(peer: RTCPeerConnection): Promise<void> {
  if (peer.iceGatheringState === 'complete') {
    return
  }
  await new Promise<void>((resolve) => {
    const changed = () => {
      if (peer.iceGatheringState === 'complete') {
        peer.removeEventListener('icegatheringstatechange', changed)
        resolve()
      }
    }
    peer.addEventListener('icegatheringstatechange', changed)
  })
}

function requiredDescription(peer: RTCPeerConnection): RTCSessionDescription {
  const description = peer.localDescription
  if (description === null) {
    throw new Error('local description is unavailable after ICE gathering')
  }
  return description
}

async function closePair(pair: PeerPair): Promise<void> {
  try {
    await withTimeout(
      Promise.all([
        pair.left.channel.close(),
        pair.right.channel.close(),
      ]),
      PEER_CLEANUP_TIMEOUT_MS,
      'benchmark peer cleanup',
    )
  } finally {
    pair.left.peer.close()
    pair.right.peer.close()
  }
}

function usedJSHeapSize(): number {
  const memory = (performance as Performance & {
    readonly memory?: PerformanceMemory
  }).memory
  return memory?.usedJSHeapSize ?? 0
}

async function withTimeout<T>(
  operation: Promise<T>,
  timeoutMs: number,
  label: string,
): Promise<T> {
  let timeout: ReturnType<typeof setTimeout> | undefined
  const deadline = new Promise<never>((_resolve, reject) => {
    timeout = setTimeout(() => {
      reject(new Error(`${label} timed out after ${timeoutMs} ms`))
    }, timeoutMs)
  })
  try {
    return await Promise.race([operation, deadline])
  } finally {
    clearTimeout(timeout)
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  resolve(value: T): void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((fulfill) => {
    resolve = fulfill
  })
  return { promise, resolve }
}
