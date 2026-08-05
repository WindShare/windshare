export const RTC_CAPABILITY_DIAGNOSTIC_SCHEMA_VERSION = 1 as const
export const RTC_CAPABILITY_PROBE_DEADLINE_MS = 5_000 as const
export const RTC_CAPABILITY_PROBE_DATA_CHANNEL_LABEL = 'windshare-capability-probe' as const

const RTC_CAPABILITY_PROBE_MESSAGE = 'windshare-capability-probe-message'
const RTC_CAPABILITY_PROBE_ACKNOWLEDGEMENT = 'windshare-capability-probe-acknowledgement'

const RTC_API_PRESENCE = Object.freeze(['unknown', 'absent', 'present'] as const)
const RTC_CAPABILITY_PROBE_OUTCOMES = Object.freeze([
  'not-started',
  'succeeded',
  'failed',
] as const)
const RTC_CAPABILITY_PROBE_FAILURE_CODES = Object.freeze([
  'peer-construction',
  'datachannel-construction',
  'offer-creation',
  'local-description',
  'ice-gathering',
  'remote-description',
  'answer-creation',
  'datachannel-open',
  'datachannel-roundtrip',
  'probe-deadline',
  'unexpected',
] as const)
const CAPABILITY_FAILURE_MESSAGE_MAXIMUM_LENGTH = 512

export type RtcApiPresence = (typeof RTC_API_PRESENCE)[number]
export type RtcCapabilityProbeOutcome = (typeof RTC_CAPABILITY_PROBE_OUTCOMES)[number]
export type RtcCapabilityProbeFailureCode = (typeof RTC_CAPABILITY_PROBE_FAILURE_CODES)[number]
export type RtcCapabilityStatus = 'unknown' | 'unavailable' | 'available' | 'unusable'

export interface RtcCapabilityDiagnostic {
  readonly schemaVersion: typeof RTC_CAPABILITY_DIAGNOSTIC_SCHEMA_VERSION
  readonly apiPresence: RtcApiPresence
  readonly probeOutcome: RtcCapabilityProbeOutcome
  readonly probeDeadlineMs: typeof RTC_CAPABILITY_PROBE_DEADLINE_MS
  readonly failureCode?: RtcCapabilityProbeFailureCode
  readonly failureMessage?: string
}

export interface RtcProbeEnvironment {
  readonly RTCPeerConnection?: typeof RTCPeerConnection
  readonly setTimeout: typeof globalThis.setTimeout
  readonly clearTimeout: typeof globalThis.clearTimeout
}

class RtcCapabilityProbeError extends Error {
  readonly code: RtcCapabilityProbeFailureCode

  constructor(
    code: RtcCapabilityProbeFailureCode,
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'RtcCapabilityProbeError'
    this.code = code
  }
}

/**
 * API presence and lane usability are deliberately separate facts. A local
 * offer only proves that a browser can serialize SDP; the product lane also
 * needs ICE candidates and an authenticated DataChannel transport. Keeping the
 * loopback probe local makes that distinction deterministic without depending
 * on a remote network or a relay service.
 */
export async function probeRtcCapability(
  environment: RtcProbeEnvironment = globalThis as unknown as RtcProbeEnvironment,
): Promise<RtcCapabilityDiagnostic> {
  if (typeof environment.RTCPeerConnection !== 'function') {
    return rtcCapabilityDiagnostic('absent', 'not-started')
  }

  let offerer: RTCPeerConnection | undefined
  let answerer: RTCPeerConnection | undefined
  let offererChannel: RTCDataChannel | undefined
  let answererChannel: RTCDataChannel | undefined
  let activeStage: RtcCapabilityProbeFailureCode = 'peer-construction'
  const remoteChannel = deferred<RTCDataChannel>()
  const onRemoteChannel = (event: Event): void => {
    const channel = (event as RTCDataChannelEvent).channel
    if (channel === undefined) {
      remoteChannel.reject(new RtcCapabilityProbeError(
        'datachannel-open',
        'capability probe received no remote DataChannel',
      ))
      return
    }
    answererChannel = channel
    remoteChannel.resolve(channel)
  }

  try {
    offerer = new environment.RTCPeerConnection({ iceServers: [] })
    answerer = new environment.RTCPeerConnection({ iceServers: [] })
    const localOfferer = offerer
    const localAnswerer = answerer
    localAnswerer.addEventListener('datachannel', onRemoteChannel)

    activeStage = 'datachannel-construction'
    offererChannel = localOfferer.createDataChannel(RTC_CAPABILITY_PROBE_DATA_CHANNEL_LABEL)

    await withinProbeDeadline(environment, async () => {
      activeStage = 'offer-creation'
      const offer = await localOfferer.createOffer()
      if (offer?.type !== 'offer' || !hasSdp(offer.sdp)) {
        throw new RtcCapabilityProbeError(
          'offer-creation',
          'capability probe created no usable SDP offer',
        )
      }

      // Register ICE observers before setting the description. Some engines
      // emit candidates synchronously while setLocalDescription is pending.
      activeStage = 'ice-gathering'
      const offerGathering = waitForIceGathering(localOfferer)
      activeStage = 'local-description'
      await localOfferer.setLocalDescription(offer)
      await offerGathering

      activeStage = 'remote-description'
      await localAnswerer.setRemoteDescription(requiredDescription(localOfferer))

      activeStage = 'answer-creation'
      const answer = await localAnswerer.createAnswer()
      if (answer?.type !== 'answer' || !hasSdp(answer.sdp)) {
        throw new RtcCapabilityProbeError(
          'answer-creation',
          'capability probe created no usable SDP answer',
        )
      }

      activeStage = 'ice-gathering'
      const answerGathering = waitForIceGathering(localAnswerer)
      activeStage = 'local-description'
      await localAnswerer.setLocalDescription(answer)
      await answerGathering

      activeStage = 'remote-description'
      await localOfferer.setRemoteDescription(requiredDescription(localAnswerer))

      activeStage = 'datachannel-open'
      answererChannel = await remoteChannel.promise
      await Promise.all([
        waitForDataChannelOpen(offererChannel),
        waitForDataChannelOpen(answererChannel),
      ])

      activeStage = 'datachannel-roundtrip'
      await exchangeProbeMessage(offererChannel, answererChannel)
    })
    return rtcCapabilityDiagnostic('present', 'succeeded')
  } catch (cause) {
    const failure = cause instanceof RtcCapabilityProbeError
      ? cause
      : new RtcCapabilityProbeError(activeStage, diagnosticMessage(cause), { cause })
    return rtcCapabilityDiagnostic('present', 'failed', failure)
  } finally {
    answerer?.removeEventListener('datachannel', onRemoteChannel)
    closeDataChannel(answererChannel)
    closeDataChannel(offererChannel)
    answerer?.close()
    offerer?.close()
  }
}

export function parseRtcCapabilityDiagnostic(value: unknown): RtcCapabilityDiagnostic {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new TypeError('RTC capability diagnostic must be an object')
  }
  const record = value as Record<string, unknown>
  const optionalKeys = record.probeOutcome === 'failed'
    ? ['failureCode', 'failureMessage']
    : []
  requireExactKeys(record, [
    'schemaVersion',
    'apiPresence',
    'probeOutcome',
    'probeDeadlineMs',
    ...optionalKeys,
  ])
  if (record.schemaVersion !== RTC_CAPABILITY_DIAGNOSTIC_SCHEMA_VERSION) {
    throw new TypeError('RTC capability diagnostic has an unsupported schema version')
  }
  if (record.probeDeadlineMs !== RTC_CAPABILITY_PROBE_DEADLINE_MS) {
    throw new TypeError('RTC capability diagnostic has an unexpected probe deadline')
  }
  const diagnostic: RtcCapabilityDiagnostic = {
    schemaVersion: RTC_CAPABILITY_DIAGNOSTIC_SCHEMA_VERSION,
    apiPresence: requireEnum(record.apiPresence, RTC_API_PRESENCE, 'RTC API presence'),
    probeOutcome: requireEnum(
      record.probeOutcome,
      RTC_CAPABILITY_PROBE_OUTCOMES,
      'RTC capability probe outcome',
    ),
    probeDeadlineMs: RTC_CAPABILITY_PROBE_DEADLINE_MS,
    ...(record.failureCode === undefined
      ? {}
      : {
          failureCode: requireEnum(
            record.failureCode,
            RTC_CAPABILITY_PROBE_FAILURE_CODES,
            'RTC capability probe failure code',
          ),
        }),
    ...(record.failureMessage === undefined
      ? {}
      : { failureMessage: requireDiagnosticText(record.failureMessage) }),
  }
  validateCapabilityDiagnostic(diagnostic)
  return Object.freeze(diagnostic)
}

export function classifyRtcCapability(
  diagnostic: RtcCapabilityDiagnostic,
): RtcCapabilityStatus {
  validateCapabilityDiagnostic(diagnostic)
  if (diagnostic.apiPresence === 'unknown') return 'unknown'
  if (diagnostic.apiPresence === 'absent') return 'unavailable'
  if (diagnostic.probeOutcome === 'not-started') return 'unknown'
  return diagnostic.probeOutcome === 'succeeded' ? 'available' : 'unusable'
}

function rtcCapabilityDiagnostic(
  apiPresence: RtcApiPresence,
  probeOutcome: RtcCapabilityProbeOutcome,
  failure?: RtcCapabilityProbeError,
): RtcCapabilityDiagnostic {
  return parseRtcCapabilityDiagnostic({
    schemaVersion: RTC_CAPABILITY_DIAGNOSTIC_SCHEMA_VERSION,
    apiPresence,
    probeOutcome,
    probeDeadlineMs: RTC_CAPABILITY_PROBE_DEADLINE_MS,
    ...(failure === undefined
      ? {}
      : { failureCode: failure.code, failureMessage: diagnosticMessage(failure) }),
  })
}

function validateCapabilityDiagnostic(diagnostic: RtcCapabilityDiagnostic): void {
  const failed = diagnostic.probeOutcome === 'failed'
  if (failed !== (diagnostic.failureCode !== undefined)) {
    throw new TypeError('only a failed RTC capability probe may carry a failure code')
  }
  if (failed !== (diagnostic.failureMessage !== undefined)) {
    throw new TypeError('only a failed RTC capability probe may carry a failure message')
  }
  if (diagnostic.apiPresence !== 'present' && diagnostic.probeOutcome !== 'not-started') {
    throw new TypeError('an RTC capability probe cannot run before API presence is proved')
  }
}

async function withinProbeDeadline(
  environment: Pick<RtcProbeEnvironment, 'setTimeout' | 'clearTimeout'>,
  operation: () => Promise<void>,
): Promise<void> {
  const work = operation()
  // Closing timed-out PeerConnections can reject their outstanding promises
  // later; consume that rejection while cleanup closes both probe peers.
  work.catch(() => undefined)
  let timer: ReturnType<typeof globalThis.setTimeout> | undefined
  const deadline = new Promise<never>((_resolve, reject) => {
    timer = environment.setTimeout(() => {
      reject(new RtcCapabilityProbeError(
        'probe-deadline',
        'capability probe exceeded its named deadline',
      ))
    }, RTC_CAPABILITY_PROBE_DEADLINE_MS)
  })
  try {
    await Promise.race([work, deadline])
  } finally {
    if (timer !== undefined) environment.clearTimeout(timer)
  }
}

async function waitForIceGathering(peer: RTCPeerConnection): Promise<void> {
  if (peer.iceGatheringState === 'complete') {
    requireGatheredCandidate(peer)
    return
  }
  let changed: (() => void) | undefined
  const gathered = new Promise<void>((resolve, reject) => {
    changed = () => {
      if (peer.iceGatheringState !== 'complete') return
      try {
        requireGatheredCandidate(peer)
        resolve()
      } catch (error) {
        reject(error)
      }
    }
    peer.addEventListener('icegatheringstatechange', changed)
  })
  try {
    await gathered
  } finally {
    if (changed !== undefined) peer.removeEventListener('icegatheringstatechange', changed)
  }
}

function requireGatheredCandidate(peer: RTCPeerConnection): void {
  const sdp = peer.localDescription?.sdp
  if (!hasSdp(sdp) || !/(?:^|\r?\n)a=candidate:/u.test(sdp)) {
    throw new RtcCapabilityProbeError(
      'ice-gathering',
      'capability probe gathered no usable ICE candidate',
    )
  }
}

async function waitForDataChannelOpen(channel: RTCDataChannel | undefined): Promise<void> {
  if (channel === undefined) {
    throw new RtcCapabilityProbeError(
      'datachannel-open',
      'capability probe did not receive a DataChannel',
    )
  }
  if (channel.readyState === 'open') return
  let opened: (() => void) | undefined
  let failed: ((event: Event) => void) | undefined
  const ready = new Promise<void>((resolve, reject) => {
    opened = () => resolve()
    failed = () => reject(new RtcCapabilityProbeError(
      'datachannel-open',
      'capability probe DataChannel entered an error state',
    ))
    channel.addEventListener('open', opened)
    channel.addEventListener('error', failed)
    if (channel.readyState === 'open') resolve()
  })
  try {
    await ready
  } finally {
    if (opened !== undefined) channel.removeEventListener('open', opened)
    if (failed !== undefined) channel.removeEventListener('error', failed)
  }
}

async function exchangeProbeMessage(
  offerer: RTCDataChannel | undefined,
  answerer: RTCDataChannel | undefined,
): Promise<void> {
  if (offerer === undefined || answerer === undefined) {
    throw new RtcCapabilityProbeError(
      'datachannel-roundtrip',
      'capability probe cannot exchange a message without two DataChannels',
    )
  }
  let onAnswer: ((event: MessageEvent<unknown>) => void) | undefined
  let onOffer: ((event: MessageEvent<unknown>) => void) | undefined
  try {
    const acknowledgement = new Promise<void>((resolve, reject) => {
      onAnswer = (event: MessageEvent<unknown>) => {
        if (event.data === RTC_CAPABILITY_PROBE_ACKNOWLEDGEMENT) resolve()
      }
      onOffer = (event: MessageEvent<unknown>) => {
        if (event.data !== RTC_CAPABILITY_PROBE_MESSAGE) return
        try {
          answerer.send(RTC_CAPABILITY_PROBE_ACKNOWLEDGEMENT)
        } catch (cause) {
          reject(new RtcCapabilityProbeError(
            'datachannel-roundtrip',
            diagnosticMessage(cause),
            { cause },
          ))
        }
      }
      offerer.addEventListener('message', onAnswer)
      answerer.addEventListener('message', onOffer)
      try {
        offerer.send(RTC_CAPABILITY_PROBE_MESSAGE)
      } catch (cause) {
        reject(new RtcCapabilityProbeError(
          'datachannel-roundtrip',
          diagnosticMessage(cause),
          { cause },
        ))
      }
    })
    await acknowledgement
  } finally {
    if (onAnswer !== undefined) offerer.removeEventListener('message', onAnswer)
    if (onOffer !== undefined) answerer.removeEventListener('message', onOffer)
  }
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
  readonly reject: (reason: unknown) => void
} {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function requiredDescription(peer: RTCPeerConnection): RTCSessionDescriptionInit {
  const description = peer.localDescription
  if (description === null || !hasSdp(description.sdp)) {
    throw new RtcCapabilityProbeError(
      'local-description',
      'capability probe did not retain a usable local description',
    )
  }
  return { type: description.type, sdp: description.sdp }
}

function hasSdp(value: string | null | undefined): value is string {
  return typeof value === 'string' && value.trim() !== ''
}

function closeDataChannel(channel: RTCDataChannel | undefined): void {
  try {
    channel?.close()
  } catch {
    // Probe cleanup is best effort; the capability result already owns the
    // diagnostic and a browser-specific close error must not replace it.
  }
}

function requireExactKeys(record: Record<string, unknown>, expected: readonly string[]): void {
  const keys = Object.keys(record)
  if (keys.length !== expected.length || expected.some((key) => !Object.hasOwn(record, key))) {
    throw new TypeError('RTC capability diagnostic has an invalid field set')
  }
}

function requireEnum<const T extends readonly string[]>(
  value: unknown,
  allowed: T,
  label: string,
): T[number] {
  if (typeof value !== 'string' || !allowed.includes(value as T[number])) {
    throw new TypeError(`${label} is invalid`)
  }
  return value as T[number]
}

function requireDiagnosticText(value: unknown): string {
  if (typeof value !== 'string' || value.trim() === '' || value.length > CAPABILITY_FAILURE_MESSAGE_MAXIMUM_LENGTH) {
    throw new TypeError('RTC capability failure message is invalid')
  }
  return value
}

function diagnosticMessage(cause: unknown): string {
  const message = cause instanceof Error ? cause.message : String(cause)
  return (message.trim() || 'native RTC capability probe failed unexpectedly')
    .slice(0, CAPABILITY_FAILURE_MESSAGE_MAXIMUM_LENGTH)
}
