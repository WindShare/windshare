import {
  contractError,
  freezeRecord,
  optionalField,
  requireCanonicalIdentity,
  requireDecimalUint64,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireString,
  type JsonRecord,
} from './contract/json.ts'
import {
  ATTEMPT_FAILURE_SCOPES,
  ATTEMPT_SIDES,
  BROWSER_ATTEMPT_STAGES,
  BROWSER_EVIDENCE_SCHEMA_VERSION,
  ICE_CANDIDATE_TYPES,
  ICE_PROTOCOLS,
  IP_ADDRESS_FAMILIES,
  SENDER_ATTEMPT_STAGES,
  TYPED_PEER_ERROR_CODES,
  typedErrorForPeerOperationCode,
  type AttemptFailureScope,
  type AttemptSide,
  type AttemptStage,
  type BrowserAttemptStage,
  type IceCandidateType,
  type IceProtocol,
  type IpAddressFamily,
  type SenderAttemptStage,
  type TypedPeerErrorCode,
} from './vocabulary.ts'

const COMMON_FIELDS = Object.freeze([
  'schemaVersion',
  'sessionId',
  'peerPathId',
  'attemptId',
  'side',
  'sideSequence',
  'attemptElapsedMs',
  'stage',
] as const)
const MAXIMUM_COUNTER = 0xffff_ffff
const MAXIMUM_DIAGNOSTIC_TEXT_BYTES = 512

export interface CandidateCounts {
  readonly localEmitted: number
  readonly remoteAccepted: number
}

export interface LaneIdentity {
  readonly laneId: number
  readonly laneEpoch: number
}

export interface BrowserIceCandidateEvidence {
  readonly candidateId: string
  readonly candidateType: IceCandidateType
  readonly protocol: IceProtocol
  readonly address?: string
  readonly port?: number
}

export interface PionIceCandidateEvidence {
  readonly candidateId?: string
  readonly candidateType: IceCandidateType
  readonly protocol: IceProtocol
  readonly address: string
  readonly port: number
  readonly addressFamily: IpAddressFamily
}

export interface BrowserSelectedPairEvidence {
  readonly candidatePairId: string
  readonly local: BrowserIceCandidateEvidence
  readonly remote: BrowserIceCandidateEvidence
}

export interface PionSelectedPairEvidence {
  readonly candidatePairId?: string
  readonly local: PionIceCandidateEvidence
  readonly remote: PionIceCandidateEvidence
}

export interface AuthenticatedSenderOperationFailureEvidence {
  readonly scope: 'peer'
  readonly code: number
  readonly message: string
}

interface AttemptEnvelope {
  readonly schemaVersion: typeof BROWSER_EVIDENCE_SCHEMA_VERSION
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
  readonly sideSequence: number
  readonly attemptElapsedMs: number
}

interface BrowserEnvelope extends AttemptEnvelope {
  readonly side: 'browser'
}

interface SenderEnvelope extends AttemptEnvelope {
  readonly side: 'sender'
  // Decimal text keeps Go uint64 diagnostics lossless in JavaScript and JSON.
  readonly localGeneration?: string
}

interface CandidateMilestone {
  readonly candidateCounts: CandidateCounts
}

interface LaneMilestone extends CandidateMilestone {
  readonly lane: LaneIdentity
}

interface FailureMilestone {
  readonly stage: 'failed'
  readonly failedAtStage: Exclude<AttemptStage, 'started' | 'failed'>
  readonly failureScope: AttemptFailureScope
  readonly typedErrorCode: TypedPeerErrorCode
  readonly failureMessage: string
  readonly candidateCounts?: CandidateCounts
  readonly lane?: LaneIdentity
  readonly authenticatedSenderOperationFailure?: AuthenticatedSenderOperationFailureEvidence
}

type BrowserCandidateStage = Extract<
  BrowserAttemptStage,
  'offer-created' | 'offer-sent' | 'answer-received' | 'datachannel-open'
>
type SenderCandidateStage = Extract<
  SenderAttemptStage,
  'answer-created' | 'answer-sent' | 'datachannel-open'
>

export type BrowserAttemptEvidence =
  | (BrowserEnvelope & { readonly stage: 'started' })
  | (BrowserEnvelope & CandidateMilestone & { readonly stage: BrowserCandidateStage })
  | (BrowserEnvelope & LaneMilestone & { readonly stage: 'lane-granted' | 'lane-attached' })
  | (BrowserEnvelope & LaneMilestone & {
      readonly stage: 'admitted'
      readonly selectedPair: BrowserSelectedPairEvidence | null
    })
  | (BrowserEnvelope & FailureMilestone & {
      readonly selectedPair?: BrowserSelectedPairEvidence | null
    })

export type SenderAttemptEvidence =
  | (SenderEnvelope & { readonly stage: 'started' | 'offer-received' })
  | (SenderEnvelope & CandidateMilestone & { readonly stage: SenderCandidateStage })
  | (SenderEnvelope & LaneMilestone & { readonly stage: 'lane-admission-started' })
  | (SenderEnvelope & LaneMilestone & {
      readonly stage: 'admitted'
      readonly selectedPair: PionSelectedPairEvidence | null
    })
  | (SenderEnvelope & FailureMilestone & {
      readonly selectedPair?: PionSelectedPairEvidence | null
    })

export type AttemptEvidence = BrowserAttemptEvidence | SenderAttemptEvidence

export function parseAttemptEvidence(value: unknown): AttemptEvidence {
  const record = requireRecord(value, 'attempt evidence')
  const side = requireEnum(record.side, ATTEMPT_SIDES, 'attempt side')
  const stage = parseStage(record.stage, side)
  requireAttemptKeys(record, side, stage)
  const envelope = {
    schemaVersion: requireLiteral(
      record.schemaVersion,
      BROWSER_EVIDENCE_SCHEMA_VERSION,
      'attempt evidence schema version',
    ),
    sessionId: requireCanonicalIdentity(record.sessionId, 'protocol session ID'),
    peerPathId: requireCanonicalIdentity(record.peerPathId, 'peer path ID'),
    attemptId: requireCanonicalIdentity(record.attemptId, 'peer attempt ID'),
    side,
    sideSequence: requireSafeInteger(
      record.sideSequence,
      1,
      Number.MAX_SAFE_INTEGER,
      'attempt side sequence',
    ),
    attemptElapsedMs: requireSafeInteger(
      record.attemptElapsedMs,
      0,
      Number.MAX_SAFE_INTEGER,
      'attempt elapsed milliseconds',
    ),
    stage,
    ...(side === 'sender' && optionalField(record, 'localGeneration') !== undefined
      ? {
          localGeneration: requireDecimalUint64(
            record.localGeneration,
            'sender local generation',
          ),
        }
      : {}),
  }
  const payload = parseStagePayload(record, side, stage)
  return freezeRecord({ ...envelope, ...payload }) as AttemptEvidence
}

export function parseBrowserSelectedPair(value: unknown): BrowserSelectedPairEvidence {
  const pair = requireRecord(value, 'browser selected pair')
  requireExactKeys(pair, ['candidatePairId', 'local', 'remote'], [], 'browser selected pair')
  return freezeRecord({
    candidatePairId: requireString(pair.candidatePairId, 'browser candidate pair ID', 256),
    local: parseBrowserCandidate(pair.local, 'browser local selected candidate'),
    remote: parseBrowserCandidate(pair.remote, 'browser remote selected candidate'),
  })
}

export function parsePionSelectedPair(value: unknown): PionSelectedPairEvidence {
  const pair = requireRecord(value, 'Pion selected pair')
  requireExactKeys(pair, ['local', 'remote'], ['candidatePairId'], 'Pion selected pair')
  const pairId = optionalField(pair, 'candidatePairId')
  const local = parsePionCandidate(pair.local, 'Pion local selected candidate')
  const remote = parsePionCandidate(pair.remote, 'Pion remote selected candidate')
  if (
    local.addressFamily === remote.addressFamily && local.address === remote.address &&
    local.port === remote.port && local.protocol === remote.protocol
  ) {
    contractError('Pion selected pair must identify distinct local and remote transport endpoints')
  }
  return freezeRecord({
    ...(pairId === undefined
      ? {}
      : { candidatePairId: requireString(pairId, 'Pion candidate pair ID', 256) }),
    local,
    remote,
  })
}

function parseStage(value: unknown, side: AttemptSide): AttemptStage {
  return side === 'browser'
    ? requireEnum(value, BROWSER_ATTEMPT_STAGES, 'browser attempt stage')
    : requireEnum(value, SENDER_ATTEMPT_STAGES, 'sender attempt stage')
}

function requireAttemptKeys(record: JsonRecord, side: AttemptSide, stage: AttemptStage): void {
  const required: string[] = [...COMMON_FIELDS]
  const optional: string[] = side === 'sender' ? ['localGeneration'] : []
  if (stage === 'failed') {
    required.push('failedAtStage', 'failureScope', 'typedErrorCode', 'failureMessage')
    optional.push('candidateCounts', 'lane', 'selectedPair', 'authenticatedSenderOperationFailure')
  } else if (stage === 'admitted') {
    required.push('candidateCounts', 'lane', 'selectedPair')
  } else if (candidateCountsRequired(side, stage)) {
    required.push('candidateCounts')
    if (laneRequired(side, stage)) required.push('lane')
  }
  requireExactKeys(record, required, optional, `${side} ${stage} attempt evidence`)
}

function candidateCountsRequired(side: AttemptSide, stage: AttemptStage): boolean {
  if (side === 'browser') return stage !== 'started'
  return stage !== 'started' && stage !== 'offer-received'
}

function laneRequired(side: AttemptSide, stage: AttemptStage): boolean {
  return side === 'browser'
    ? stage === 'lane-granted' || stage === 'lane-attached'
    : stage === 'lane-admission-started'
}

function parseStagePayload(
  record: JsonRecord,
  side: AttemptSide,
  stage: AttemptStage,
): Record<string, unknown> {
  if (stage === 'started' || (side === 'sender' && stage === 'offer-received')) return {}
  if (stage === 'failed') return parseFailurePayload(record, side)
  const candidateCounts = parseCandidateCounts(record.candidateCounts)
  if (stage === 'admitted') {
    return {
      candidateCounts,
      lane: parseLaneIdentity(record.lane),
      selectedPair: parseNullableSelectedPair(record.selectedPair, side),
    }
  }
  if (laneRequired(side, stage)) {
    return { candidateCounts, lane: parseLaneIdentity(record.lane) }
  }
  return { candidateCounts }
}

function parseFailurePayload(record: JsonRecord, side: AttemptSide): Record<string, unknown> {
  const failedAtStage = parseFailureStage(record.failedAtStage, side)
  const typedErrorCode = requireEnum(
    record.typedErrorCode,
    TYPED_PEER_ERROR_CODES,
    'typed peer error code',
  )
  const candidateCountsValue = optionalField(record, 'candidateCounts')
  const laneValue = optionalField(record, 'lane')
  const selectedPairValue = optionalField(record, 'selectedPair')
  const authenticatedOperationValue = optionalField(record, 'authenticatedSenderOperationFailure')
  const authenticatedSenderOperationFailure = authenticatedOperationValue === undefined
    ? undefined
    : parseAuthenticatedSenderOperationFailure(authenticatedOperationValue, typedErrorCode)
  const failureScope = requireEnum(
    record.failureScope,
    ATTEMPT_FAILURE_SCOPES,
    'attempt failure scope',
  )
  const failureMessage = requireString(
    record.failureMessage,
    'attempt failure message',
    MAXIMUM_DIAGNOSTIC_TEXT_BYTES,
  )
  validateFailureFieldCausality({
    side,
    failedAtStage,
    failureScope,
    typedErrorCode,
    failureMessage,
    candidateCountsValue,
    laneValue,
    selectedPairValue,
    authenticatedSenderOperationFailure,
  })
  return buildFailurePayload({
    side,
    failedAtStage,
    failureScope,
    typedErrorCode,
    failureMessage,
    candidateCountsValue,
    laneValue,
    selectedPairValue,
    authenticatedSenderOperationFailure,
  })
}

interface FailurePayloadParts {
  readonly side: AttemptSide
  readonly failedAtStage: FailureMilestone['failedAtStage']
  readonly failureScope: AttemptFailureScope
  readonly typedErrorCode: TypedPeerErrorCode
  readonly failureMessage: string
  readonly candidateCountsValue: unknown | undefined
  readonly laneValue: unknown | undefined
  readonly selectedPairValue: unknown | undefined
  readonly authenticatedSenderOperationFailure: AuthenticatedSenderOperationFailureEvidence | undefined
}

function validateFailureFieldCausality(parts: FailurePayloadParts): void {
  const {
    side, failedAtStage, failureScope, failureMessage, candidateCountsValue,
    laneValue, selectedPairValue, authenticatedSenderOperationFailure,
  } = parts
  if (candidateCountsValue !== undefined && !failureCanCarryCandidateCounts(side, failedAtStage)) {
    contractError(`${side} failure cannot carry candidate counts before their first completed milestone`)
  }
  if (laneValue !== undefined && !failureCanCarryKnownLane(side, failedAtStage)) {
    contractError(`${side} failure cannot carry a lane before the lane milestone is known`)
  }
  if (selectedPairValue !== undefined && failedAtStage !== 'admitted') {
    contractError(`${side} failure can carry selected-pair evidence only while admission fails`)
  }
  if (authenticatedSenderOperationFailure !== undefined) {
    if (
      side !== 'browser' ||
      failedAtStage === 'offer-created' || failedAtStage === 'offer-sent'
    ) {
      contractError('authenticated sender operation failure requires a browser stream after offer dispatch')
    }
    if (failureScope !== 'attempt') {
      contractError('authenticated sender peer operation failure must remain attempt-scoped')
    }
    if (failureMessage !== authenticatedSenderOperationFailure.message) {
      contractError('authenticated sender operation message must be preserved losslessly')
    }
  }
}

function buildFailurePayload(parts: FailurePayloadParts): Record<string, unknown> {
  const result: Record<string, unknown> = {
    failedAtStage: parts.failedAtStage,
    failureScope: parts.failureScope,
    typedErrorCode: parts.typedErrorCode,
    failureMessage: parts.failureMessage,
  }
  if (parts.candidateCountsValue !== undefined) {
    result.candidateCounts = parseCandidateCounts(parts.candidateCountsValue)
  }
  if (parts.laneValue !== undefined) result.lane = parseLaneIdentity(parts.laneValue)
  if (parts.selectedPairValue !== undefined) {
    result.selectedPair = parseNullableSelectedPair(parts.selectedPairValue, parts.side)
  }
  if (parts.authenticatedSenderOperationFailure !== undefined) {
    result.authenticatedSenderOperationFailure = parts.authenticatedSenderOperationFailure
  }
  return result
}

function parseFailureStage(value: unknown, side: AttemptSide): FailureMilestone['failedAtStage'] {
  const stages = side === 'browser' ? BROWSER_ATTEMPT_STAGES : SENDER_ATTEMPT_STAGES
  const stage = requireEnum(value, stages, `${side} failed-at stage`)
  if (stage === 'started' || stage === 'failed') {
    contractError(`${side} failed-at stage must name the milestone that could not complete`)
  }
  return stage
}

function parseCandidateCounts(value: unknown): CandidateCounts {
  const counts = requireRecord(value, 'candidate counts')
  requireExactKeys(counts, ['localEmitted', 'remoteAccepted'], [], 'candidate counts')
  return freezeRecord({
    localEmitted: requireSafeInteger(
      counts.localEmitted,
      0,
      MAXIMUM_COUNTER,
      'local emitted candidate count',
    ),
    remoteAccepted: requireSafeInteger(
      counts.remoteAccepted,
      0,
      MAXIMUM_COUNTER,
      'remote accepted candidate count',
    ),
  })
}

function parseLaneIdentity(value: unknown): LaneIdentity {
  const lane = requireRecord(value, 'lane identity')
  requireExactKeys(lane, ['laneId', 'laneEpoch'], [], 'lane identity')
  return freezeRecord({
    laneId: requireSafeInteger(lane.laneId, 1, MAXIMUM_COUNTER, 'lane ID'),
    laneEpoch: requireSafeInteger(lane.laneEpoch, 1, MAXIMUM_COUNTER, 'lane epoch'),
  })
}

function parseNullableSelectedPair(
  value: unknown,
  side: AttemptSide,
): BrowserSelectedPairEvidence | PionSelectedPairEvidence | null {
  if (value === null) return null
  return side === 'browser' ? parseBrowserSelectedPair(value) : parsePionSelectedPair(value)
}

function parseBrowserCandidate(value: unknown, label: string): BrowserIceCandidateEvidence {
  const candidate = requireRecord(value, label)
  requireExactKeys(
    candidate,
    ['candidateId', 'candidateType', 'protocol'],
    ['address', 'port'],
    label,
  )
  const address = optionalField(candidate, 'address')
  const port = optionalField(candidate, 'port')
  return freezeRecord({
    candidateId: requireString(candidate.candidateId, `${label} ID`, 256),
    candidateType: requireEnum(candidate.candidateType, ICE_CANDIDATE_TYPES, `${label} type`),
    protocol: requireEnum(candidate.protocol, ICE_PROTOCOLS, `${label} protocol`),
    ...(address === undefined ? {} : { address: requireString(address, `${label} address`, 255) }),
    ...(port === undefined
      ? {}
      : { port: requireSafeInteger(port, 1, 65_535, `${label} port`) }),
  })
}

function parsePionCandidate(value: unknown, label: string): PionIceCandidateEvidence {
  const candidate = requireRecord(value, label)
  requireExactKeys(
    candidate,
    ['candidateType', 'protocol', 'address', 'port', 'addressFamily'],
    ['candidateId'],
    label,
  )
  const addressFamily = requireEnum(
    candidate.addressFamily,
    IP_ADDRESS_FAMILIES,
    `${label} address family`,
  )
  const address = requireString(candidate.address, `${label} address`, 255)
  if (!isOperationalUnicastAddress(address, addressFamily)) {
    contractError(`${label} address must be operational non-loopback ${addressFamily} unicast`)
  }
  const candidateId = optionalField(candidate, 'candidateId')
  return freezeRecord({
    ...(candidateId === undefined
      ? {}
      : { candidateId: requireString(candidateId, `${label} ID`, 256) }),
    candidateType: requireEnum(candidate.candidateType, ICE_CANDIDATE_TYPES, `${label} type`),
    protocol: requireEnum(candidate.protocol, ICE_PROTOCOLS, `${label} protocol`),
    address,
    port: requireSafeInteger(candidate.port, 1, 65_535, `${label} port`),
    addressFamily,
  })
}

function parseAuthenticatedSenderOperationFailure(
  value: unknown,
  typedErrorCode: TypedPeerErrorCode,
): AuthenticatedSenderOperationFailureEvidence {
  const failure = requireRecord(value, 'authenticated sender operation failure')
  requireExactKeys(failure, ['scope', 'code', 'message'], [], 'authenticated sender operation failure')
  const code = requireSafeInteger(failure.code, 0, 65_535, 'authenticated peer operation code')
  const mapped = typedErrorForPeerOperationCode(code)
  if (mapped === undefined || mapped !== typedErrorCode) {
    contractError('authenticated peer operation code does not match the typed peer error code')
  }
  return freezeRecord({
    scope: requireLiteral(failure.scope, 'peer', 'authenticated operation scope'),
    code,
    message: requireString(
      failure.message,
      'authenticated peer operation message',
      MAXIMUM_DIAGNOSTIC_TEXT_BYTES,
    ),
  })
}

function failureCanCarryCandidateCounts(
  side: AttemptSide,
  failedAtStage: FailureMilestone['failedAtStage'],
): boolean {
  if (side === 'browser') return failedAtStage !== 'offer-created'
  return failedAtStage !== 'offer-received' && failedAtStage !== 'answer-created'
}

function failureCanCarryKnownLane(
  side: AttemptSide,
  failedAtStage: FailureMilestone['failedAtStage'],
): boolean {
  return side === 'browser'
    ? failedAtStage === 'lane-attached' || failedAtStage === 'admitted'
    : failedAtStage === 'admitted'
}

function isOperationalUnicastAddress(address: string, family: IpAddressFamily): boolean {
  if (family === 'ipv4') {
    const parts = address.split('.')
    if (parts.length !== 4 || !parts.every((part) => {
      if (!/^(0|[1-9]\d{0,2})$/u.test(part)) return false
      return Number(part) <= 255
    })) return false
    const octets = parts.map(Number)
    const first = octets[0]
    const second = octets[1]
    return first !== undefined && second !== undefined && first !== 0 && first !== 127 &&
      first < 224 && !(first === 169 && second === 254)
  }
  if (!address.includes(':') || !/^[0-9a-fA-F:.]+$/u.test(address)) return false
  const normalized = address.toLowerCase()
  const firstGroup = normalized.split(':')[0]
  if (
    normalized === '::' || normalized === '::1' || normalized.startsWith('ff') ||
    (firstGroup !== undefined && /^fe[89ab]/u.test(firstGroup))
  ) return false
  try {
    return new URL(`http://[${address}]/`).hostname.length > 2
  } catch {
    return false
  }
}
