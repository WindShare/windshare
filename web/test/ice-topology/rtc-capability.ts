import {
  CAPABILITY_PROBE_DATA_CHANNEL_LABEL,
  CAPABILITY_PROBE_DEADLINE_MS,
  parseCapabilityEvidence,
  type CapabilityEvidence,
  type CapabilityProbeFailureCode,
} from '../../scripts/browser-evidence/capability'
import { BROWSER_EVIDENCE_SCHEMA_VERSION } from '../../scripts/browser-evidence/vocabulary'

const CAPABILITY_FAILURE_MESSAGE_MAXIMUM_LENGTH = 512

interface RtcProbeEnvironment {
  readonly RTCPeerConnection?: typeof RTCPeerConnection
  readonly setTimeout: typeof globalThis.setTimeout
  readonly clearTimeout: typeof globalThis.clearTimeout
}

class CapabilityProbeError extends Error {
  readonly code: CapabilityProbeFailureCode

  constructor(
    code: CapabilityProbeFailureCode,
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'CapabilityProbeError'
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
): Promise<CapabilityEvidence> {
  if (typeof environment.RTCPeerConnection !== 'function') {
    return parseCapabilityEvidence({
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      apiPresence: 'absent',
      probeOutcome: 'not-started',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    })
  }

  let peer: RTCPeerConnection | undefined
  let channel: RTCDataChannel | undefined
  let activeStage: CapabilityProbeFailureCode = 'peer-construction'
  try {
    peer = new environment.RTCPeerConnection({ iceServers: [] })
    activeStage = 'datachannel-construction'
    channel = peer.createDataChannel(CAPABILITY_PROBE_DATA_CHANNEL_LABEL)
    await withinProbeDeadline(environment, async () => {
      activeStage = 'offer-creation'
      const offer = await peer?.createOffer()
      if (offer?.type !== 'offer' || typeof offer.sdp !== 'string' || offer.sdp.trim() === '') {
        throw new CapabilityProbeError('offer-creation', 'capability probe created no usable SDP offer')
      }
      activeStage = 'local-description'
      await peer?.setLocalDescription(offer)
      if (peer?.localDescription?.type !== 'offer' || peer.localDescription.sdp.trim() === '') {
        throw new CapabilityProbeError(
          'local-description',
          'capability probe did not retain a usable local offer',
        )
      }
    })
    return parseCapabilityEvidence({
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      apiPresence: 'present',
      probeOutcome: 'succeeded',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
    })
  } catch (cause) {
    const failure = cause instanceof CapabilityProbeError
      ? cause
      : new CapabilityProbeError(activeStage, diagnosticMessage(cause), { cause })
    return parseCapabilityEvidence({
      schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
      apiPresence: 'present',
      probeOutcome: 'failed',
      probeDeadlineMs: CAPABILITY_PROBE_DEADLINE_MS,
      failureCode: failure.code,
      failureMessage: diagnosticMessage(failure),
    })
  } finally {
    channel?.close()
    peer?.close()
  }
}

async function withinProbeDeadline(
  environment: Pick<RtcProbeEnvironment, 'setTimeout' | 'clearTimeout'>,
  operation: () => Promise<void>,
): Promise<void> {
  const work = operation()
  // A timed-out WebRTC promise can reject after the PeerConnection is closed.
  // Observing that loser keeps the classifier from creating an unhandled rejection.
  work.catch(() => undefined)
  let timer: ReturnType<typeof globalThis.setTimeout> | undefined
  const deadline = new Promise<never>((_resolve, reject) => {
    timer = environment.setTimeout(() => {
      reject(new CapabilityProbeError('probe-deadline', 'capability probe exceeded its named deadline'))
    }, CAPABILITY_PROBE_DEADLINE_MS)
  })
  try {
    await Promise.race([work, deadline])
  } finally {
    if (timer !== undefined) environment.clearTimeout(timer)
  }
}

function diagnosticMessage(cause: unknown): string {
  const message = cause instanceof Error ? cause.message : String(cause)
  return (message.trim() || 'native RTC capability probe failed unexpectedly')
    .slice(0, CAPABILITY_FAILURE_MESSAGE_MAXIMUM_LENGTH)
}
