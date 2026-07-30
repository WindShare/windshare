export const BROWSER_EVIDENCE_SCHEMA_VERSION = 1 as const
export const BROWSER_EVIDENCE_SAMPLE_COUNT = 3 as const

export const BROWSER_ENGINES = Object.freeze(['chromium', 'firefox', 'webkit'] as const)
export const BROWSER_SUITES = Object.freeze(['main', 'pion'] as const)
export const RESULT_STATUSES = Object.freeze([
  'provisional',
  'final-valid',
  'final-invalid',
] as const)

export const RTC_CAPABILITIES = Object.freeze([
  'unknown',
  'unavailable',
  'unusable',
  'available',
] as const)
export const PEER_ATTEMPT_OUTCOMES = Object.freeze([
  'not-started',
  'admitted',
  'failed',
] as const)
export const DELIVERY_OUTCOMES = Object.freeze([
  'not-started',
  'succeeded',
  'failed',
] as const)
export const EXECUTION_OUTCOMES = Object.freeze([
  'healthy',
  'crashed',
  'infrastructure-failed',
  'unknown',
] as const)

export const ATTEMPT_SIDES = Object.freeze(['browser', 'sender'] as const)
export const BROWSER_ATTEMPT_STAGES = Object.freeze([
  'started',
  'offer-created',
  'offer-sent',
  'answer-received',
  'datachannel-open',
  'lane-granted',
  'lane-attached',
  'admitted',
  'failed',
] as const)
export const SENDER_ATTEMPT_STAGES = Object.freeze([
  'started',
  'offer-received',
  'answer-created',
  'answer-sent',
  'datachannel-open',
  'lane-admission-started',
  'admitted',
  'failed',
] as const)
export const ATTEMPT_TERMINAL_STAGES = Object.freeze(['admitted', 'failed'] as const)

export const ATTEMPT_FAILURE_SCOPES = Object.freeze(['attempt', 'session'] as const)
export const TYPED_PEER_ERROR_CODES = Object.freeze([
  'peer-negotiation',
  'peer-timeout',
  'peer-candidates',
  'peer-admission',
  'signaling-contract',
  'attempt-cancelled',
  'runtime-stopped',
  'unexpected',
] as const)

export const PEER_OPERATION_CODES = Object.freeze({
  negotiation: 0x5001,
  timeout: 0x5002,
  candidates: 0x5003,
  admission: 0x5004,
} as const)

export const PEER_OPERATION_TYPED_ERRORS = Object.freeze({
  [PEER_OPERATION_CODES.negotiation]: 'peer-negotiation',
  [PEER_OPERATION_CODES.timeout]: 'peer-timeout',
  [PEER_OPERATION_CODES.candidates]: 'peer-candidates',
  [PEER_OPERATION_CODES.admission]: 'peer-admission',
} as const satisfies Readonly<Record<number, TypedPeerErrorCode>>)

export const PEER_OPERATION_ERROR_REGISTRY = Object.freeze([
  Object.freeze({ code: PEER_OPERATION_CODES.negotiation, typedErrorCode: 'peer-negotiation' }),
  Object.freeze({ code: PEER_OPERATION_CODES.timeout, typedErrorCode: 'peer-timeout' }),
  Object.freeze({ code: PEER_OPERATION_CODES.candidates, typedErrorCode: 'peer-candidates' }),
  Object.freeze({ code: PEER_OPERATION_CODES.admission, typedErrorCode: 'peer-admission' }),
] as const)

export const ICE_CANDIDATE_TYPES = Object.freeze(['host', 'prflx', 'srflx', 'relay'] as const)
export const ICE_PROTOCOLS = Object.freeze(['udp', 'tcp'] as const)
export const IP_ADDRESS_FAMILIES = Object.freeze(['ipv4', 'ipv6'] as const)

export const BROWSER_EVIDENCE_VOCABULARY = Object.freeze({
  schemaVersion: BROWSER_EVIDENCE_SCHEMA_VERSION,
  browserEngines: BROWSER_ENGINES,
  suites: BROWSER_SUITES,
  resultStatuses: RESULT_STATUSES,
  rtcCapabilities: RTC_CAPABILITIES,
  peerAttemptOutcomes: PEER_ATTEMPT_OUTCOMES,
  deliveryOutcomes: DELIVERY_OUTCOMES,
  executionOutcomes: EXECUTION_OUTCOMES,
  attemptSides: ATTEMPT_SIDES,
  browserStages: BROWSER_ATTEMPT_STAGES,
  senderStages: SENDER_ATTEMPT_STAGES,
  terminalStages: ATTEMPT_TERMINAL_STAGES,
  failureScopes: ATTEMPT_FAILURE_SCOPES,
  typedPeerErrorCodes: TYPED_PEER_ERROR_CODES,
  peerOperationCodeMapping: PEER_OPERATION_ERROR_REGISTRY,
  iceCandidateTypes: ICE_CANDIDATE_TYPES,
  iceProtocols: ICE_PROTOCOLS,
  ipAddressFamilies: IP_ADDRESS_FAMILIES,
})

export type BrowserEngine = (typeof BROWSER_ENGINES)[number]
export type BrowserSuite = (typeof BROWSER_SUITES)[number]
export type ResultStatus = (typeof RESULT_STATUSES)[number]
export type RtcCapability = (typeof RTC_CAPABILITIES)[number]
export type PeerAttemptOutcome = (typeof PEER_ATTEMPT_OUTCOMES)[number]
export type DeliveryOutcome = (typeof DELIVERY_OUTCOMES)[number]
export type ExecutionOutcome = (typeof EXECUTION_OUTCOMES)[number]
export type AttemptSide = (typeof ATTEMPT_SIDES)[number]
export type BrowserAttemptStage = (typeof BROWSER_ATTEMPT_STAGES)[number]
export type SenderAttemptStage = (typeof SENDER_ATTEMPT_STAGES)[number]
export type AttemptStage = BrowserAttemptStage | SenderAttemptStage
export type AttemptTerminalStage = (typeof ATTEMPT_TERMINAL_STAGES)[number]
export type AttemptFailureScope = (typeof ATTEMPT_FAILURE_SCOPES)[number]
export type TypedPeerErrorCode = (typeof TYPED_PEER_ERROR_CODES)[number]
export type IceCandidateType = (typeof ICE_CANDIDATE_TYPES)[number]
export type IceProtocol = (typeof ICE_PROTOCOLS)[number]
export type IpAddressFamily = (typeof IP_ADDRESS_FAMILIES)[number]

export function typedErrorForPeerOperationCode(code: number): TypedPeerErrorCode | undefined {
  if (!Object.hasOwn(PEER_OPERATION_TYPED_ERRORS, code)) return undefined
  return PEER_OPERATION_TYPED_ERRORS[code as keyof typeof PEER_OPERATION_TYPED_ERRORS]
}
