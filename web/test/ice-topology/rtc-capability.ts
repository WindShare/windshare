export const RTC_CAPABILITY_DIAGNOSTIC_SCHEMA_VERSION = 1 as const
export const RTC_CAPABILITY_PROBE_DEADLINE_MS = 5_000 as const
export const RTC_CAPABILITY_PROBE_DATA_CHANNEL_LABEL = 'windshare-capability-probe' as const

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
  'probe-deadline',
  'unexpected',
] as const)
const CAPABILITY_FAILURE_MESSAGE_MAXIMUM_LENGTH = 512

export type RtcApiPresence = (typeof RTC_API_PRESENCE)[number]
export type RtcCapabilityProbeOutcome = (typeof RTC_CAPABILITY_PROBE_OUTCOMES)[number]
export type RtcCapabilityProbeFailureCode =
  (typeof RTC_CAPABILITY_PROBE_FAILURE_CODES)[number]
export type RtcCapabilityStatus = 'unknown' | 'unavailable' | 'available' | 'unusable'

export interface RtcCapabilityDiagnostic {
  readonly schemaVersion: typeof RTC_CAPABILITY_DIAGNOSTIC_SCHEMA_VERSION
  readonly apiPresence: RtcApiPresence
  readonly probeOutcome: RtcCapabilityProbeOutcome
  readonly probeDeadlineMs: typeof RTC_CAPABILITY_PROBE_DEADLINE_MS
  readonly failureCode?: RtcCapabilityProbeFailureCode
  readonly failureMessage?: string
}

interface RtcProbeEnvironment {
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
 * API presence and offer usability are deliberately separate facts. Keeping the
 * probe local prevents a network or topology failure from being mislabeled as a
 * missing browser API.
 */
export async function probeRtcCapability(
  environment: RtcProbeEnvironment = globalThis as unknown as RtcProbeEnvironment,
): Promise<RtcCapabilityDiagnostic> {
  if (typeof environment.RTCPeerConnection !== 'function') {
    return rtcCapabilityDiagnostic('absent', 'not-started')
  }

  let peer: RTCPeerConnection | undefined
  let channel: RTCDataChannel | undefined
  let activeStage: RtcCapabilityProbeFailureCode = 'peer-construction'
  try {
    peer = new environment.RTCPeerConnection({ iceServers: [] })
    activeStage = 'datachannel-construction'
    channel = peer.createDataChannel(RTC_CAPABILITY_PROBE_DATA_CHANNEL_LABEL)
    await withinProbeDeadline(environment, async () => {
      activeStage = 'offer-creation'
      const offer = await peer?.createOffer()
      if (offer?.type !== 'offer' || typeof offer.sdp !== 'string' || offer.sdp.trim() === '') {
        throw new RtcCapabilityProbeError(
          'offer-creation',
          'capability probe created no usable SDP offer',
        )
      }
      activeStage = 'local-description'
      await peer?.setLocalDescription(offer)
      if (peer?.localDescription?.type !== 'offer' || peer.localDescription.sdp.trim() === '') {
        throw new RtcCapabilityProbeError(
          'local-description',
          'capability probe did not retain a usable local offer',
        )
      }
    })
    return rtcCapabilityDiagnostic('present', 'succeeded')
  } catch (cause) {
    const failure = cause instanceof RtcCapabilityProbeError
      ? cause
      : new RtcCapabilityProbeError(activeStage, diagnosticMessage(cause), { cause })
    return rtcCapabilityDiagnostic('present', 'failed', failure)
  } finally {
    channel?.close()
    peer?.close()
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
  // Closing a timed-out PeerConnection can reject its outstanding promise later.
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
