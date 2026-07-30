import {
  parseBrowserSelectedPair,
  parsePionSelectedPair,
  type BrowserSelectedPairEvidence,
  type PionSelectedPairEvidence,
} from './attempt-evidence.ts'
import {
  parseLogicalAttempts,
  reducePeerAttemptOutcome,
  type LogicalAttempt,
} from './attempt-collector.ts'
import { artifactIdForManifest } from './artifact/manifest.ts'
import {
  comparePortablePaths,
  portablePathCollisionKey,
  requirePortableRelativePath,
} from './filesystem/portable-path.ts'
import {
  classifyRtcCapability,
  parseCapabilityEvidence,
  type CapabilityEvidence,
} from './capability.ts'
import {
  classifyExecutionOutcome,
  parseExecutionEvidence,
  validateRunnerProcessVerdict,
  type ExecutionEvidence,
} from './execution-evidence.ts'
import {
  contractError,
  freezeRecord,
  optionalField,
  requireArray,
  requireCanonicalIdentity,
  requireCheckoutSha,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireSha256,
  requireString,
  type JsonRecord,
} from './contract/json.ts'
import {
  parseMainRouteEvidence,
  type MainRouteEvidence,
  type PeerAdmissionObservation,
} from './route-evidence.ts'
import {
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
  type BrowserRunPolicy,
} from './run-policy.ts'
import {
  PR_TEST_ICE_TOPOLOGY_ID,
  parseTestIceTopology,
  parseTestIceTopologyResolution,
  readVerifiedTestIceTopologyLock,
  selectedPairAllowedByTopology,
  type TestIceTopology,
  type TestIceTopologyResolution,
  type VerifiedTestIceTopologyLock,
} from './test-ice-topology.ts'
import {
  BROWSER_ENGINES,
  BROWSER_EVIDENCE_SCHEMA_VERSION,
  DELIVERY_OUTCOMES,
  EXECUTION_OUTCOMES,
  PEER_ATTEMPT_OUTCOMES,
  RESULT_STATUSES,
  RTC_CAPABILITIES,
  type BrowserEngine,
  type DeliveryOutcome,
  type ExecutionOutcome,
  type PeerAttemptOutcome,
  type ResultStatus,
  type RtcCapability,
} from './vocabulary.ts'

export const PLAYWRIGHT_OUTCOMES = Object.freeze(['not-started', 'passed', 'failed'] as const)
export const PION_APPLICABILITY = Object.freeze(['unknown', 'applicable', 'not-applicable'] as const)
export const NATIVE_INTEROP_OUTCOMES = Object.freeze(['not-started', 'succeeded', 'failed'] as const)
export const NATIVE_INTEROP_FAILURE_CODES = Object.freeze([
  'peer-construction',
  'negotiation',
  'datachannel',
  'interop-deadline',
  'selected-pair',
  'protocol',
  'unexpected',
] as const)
export const DELIVERY_TERMINALS = Object.freeze(['succeeded', 'failed'] as const)
export const MAIN_TRANSFER_BYTES = 16_777_216 as const
export const MAIN_TRANSFER_SHA256 =
  '25e349f1212bb99491944eb8e885665bb71edc5d5db49d1cd2ef1ffafac1dd5d' as const
export const ARTIFACT_KINDS = Object.freeze([
  'trace',
  'video',
  'screenshot',
  'error-context',
  'console-log',
  'runner-stdout',
  'runner-stderr',
  'process-log',
  'attempt-evidence',
  'native-interop-evidence',
  'result-diagnostic',
] as const)

export type PlaywrightOutcome = (typeof PLAYWRIGHT_OUTCOMES)[number]
export type PionApplicability = (typeof PION_APPLICABILITY)[number]
export type NativeInteropOutcome = (typeof NATIVE_INTEROP_OUTCOMES)[number]
export type NativeInteropFailureCode = (typeof NATIVE_INTEROP_FAILURE_CODES)[number]
export type ArtifactKind = (typeof ARTIFACT_KINDS)[number]

export interface ArtifactIndexEntry {
  readonly artifactId: string
  readonly kind: ArtifactKind
  readonly relativePath: string
  readonly mediaType: string
  readonly byteLength: number
  readonly sha256: string
}

export interface DeliveryEvidence {
  readonly expectedBytes: number
  readonly receivedBytes: number
  readonly expectedSha256: string
  readonly receivedSha256: string | null
  readonly terminal: (typeof DELIVERY_TERMINALS)[number]
}

export interface NativeInteropEvidence {
  readonly browser: NativeInteropSideEvidence<BrowserSelectedPairEvidence>
  readonly pion: NativeInteropSideEvidence<PionSelectedPairEvidence>
  readonly failureCode?: NativeInteropFailureCode
  readonly failureMessage?: string
}

export interface NativeInteropSideEvidence<TSelectedPair> {
  readonly attemptId: string
  readonly selectedPair: TSelectedPair | null
}

interface SampleResultCommon {
  readonly schemaVersion: typeof BROWSER_EVIDENCE_SCHEMA_VERSION
  readonly resultStatus: ResultStatus
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
  readonly topologyId: typeof PR_TEST_ICE_TOPOLOGY_ID
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
  readonly rtcCapability: RtcCapability
  readonly capabilityEvidence: CapabilityEvidence
  readonly executionOutcome: ExecutionOutcome
  readonly executionEvidence: ExecutionEvidence
  readonly playwrightOutcome: PlaywrightOutcome
  readonly artifacts: readonly ArtifactIndexEntry[]
  readonly integrityViolations: readonly string[]
}

export interface MainBrowserSampleResult extends SampleResultCommon {
  readonly suite: 'main'
  readonly peerAttemptOutcome: PeerAttemptOutcome
  readonly deliveryOutcome: DeliveryOutcome
  readonly attempts: readonly LogicalAttempt[]
  readonly deliveryEvidence: DeliveryEvidence | null
  readonly routeEvidence: MainRouteEvidence | null
}

export interface PionBrowserSampleResult extends SampleResultCommon {
  readonly suite: 'pion'
  readonly applicability: PionApplicability
  readonly nativeInteropOutcome: NativeInteropOutcome
  readonly nativeInteropEvidence: NativeInteropEvidence | null
}

export type BrowserSampleResult = MainBrowserSampleResult | PionBrowserSampleResult
export type PionAcceptanceDisposition = 'accepted' | 'requires-main-relay-fallback'

export { classifyExecutionOutcome } from './execution-evidence.ts'

export function validateMainAcceptance(
  result: MainBrowserSampleResult,
  topologyLock: VerifiedTestIceTopologyLock,
): void {
  const { profile: topology, resolution } = readVerifiedTestIceTopologyLock(topologyLock)
  if (
    result.resultStatus !== 'final-valid' || result.executionOutcome !== 'healthy' ||
    result.playwrightOutcome !== 'passed' || result.deliveryOutcome !== 'succeeded'
  ) {
    contractError('main acceptance requires valid, healthy, passed, successful delivery evidence')
  }
  if (result.rtcCapability === 'available') {
    if (
      result.peerAttemptOutcome !== 'admitted' || result.routeEvidence?.mode !== 'hot-switch' ||
      !hasDirectSelectedPairProof(result.attempts, topology, resolution)
    ) {
      contractError('available RTC acceptance requires admission, direct pair proof, and hot-switch fence proof')
    }
    return
  }
  if (result.rtcCapability === 'unavailable') {
    if (
      result.peerAttemptOutcome !== 'not-started' || result.attempts.length !== 0 ||
      result.routeEvidence?.mode !== 'relay-only'
    ) {
      contractError('unavailable RTC acceptance requires attempt-free exact relay fallback')
    }
    return
  }
  contractError('unknown or unusable RTC capability is never an accepted main result')
}

export function validatePionAcceptance(
  result: PionBrowserSampleResult,
): PionAcceptanceDisposition {
  if (
    result.resultStatus !== 'final-valid' || result.executionOutcome !== 'healthy' ||
    result.playwrightOutcome !== 'passed'
  ) {
    contractError('Pion acceptance requires valid, healthy, passed execution evidence')
  }
  if (result.rtcCapability === 'available') {
    if (result.applicability !== 'applicable' || result.nativeInteropOutcome !== 'succeeded') {
      contractError('available RTC Pion acceptance requires successful applicable native interop')
    }
    return 'accepted'
  }
  if (result.rtcCapability === 'unavailable') {
    if (result.applicability !== 'not-applicable' || result.nativeInteropOutcome !== 'not-started') {
      contractError('unavailable RTC Pion evidence must be explicitly not-applicable')
    }
    return 'requires-main-relay-fallback'
  }
  contractError('unknown or unusable RTC capability is never accepted by the Pion suite')
}

export function parseBrowserSampleResult(
  value: unknown,
  topologyLock: VerifiedTestIceTopologyLock,
): BrowserSampleResult {
  const {
    profile: topology,
    resolution,
    profileSha256: expectedProfileSha256,
    resolutionSha256: expectedResolutionSha256,
  } = readVerifiedTestIceTopologyLock(topologyLock)
  const record = requireRecord(value, 'browser sample result')
  const suite = requireEnum(record.suite, ['main', 'pion'] as const, 'browser sample suite')
  return suite === 'main'
    ? parseMainResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256)
    : parsePionResult(record, topology, resolution, expectedProfileSha256, expectedResolutionSha256)
}

function parseMainResult(
  record: JsonRecord,
  topology: TestIceTopology,
  resolution: TestIceTopologyResolution,
  expectedProfileSha256: string,
  expectedResolutionSha256: string,
): MainBrowserSampleResult {
  requireExactKeys(
    record,
    [
      ...commonResultFields(),
      'suite',
      'peerAttemptOutcome',
      'deliveryOutcome',
      'attempts',
      'deliveryEvidence',
      'routeEvidence',
    ],
    [],
    'main browser sample result',
  )
  const common = parseCommonResult(
    record,
    topology,
    resolution,
    expectedProfileSha256,
    expectedResolutionSha256,
  )
  const peerAttemptOutcome = requireEnum(
    record.peerAttemptOutcome,
    PEER_ATTEMPT_OUTCOMES,
    'peer attempt outcome',
  )
  const deliveryOutcome = requireEnum(
    record.deliveryOutcome,
    DELIVERY_OUTCOMES,
    'delivery outcome',
  )
  const attempts = common.resultStatus === 'provisional'
    ? parseProvisionalAttempts(record.attempts)
    : parseLogicalAttempts(record.attempts, common.resultStatus === 'final-invalid')
  const deliveryEvidence = parseDeliveryEvidence(record.deliveryEvidence, deliveryOutcome)
  const routeEvidence = parseMainRouteEvidence(record.routeEvidence)
  if (common.resultStatus !== 'provisional' && reducePeerAttemptOutcome(attempts) !== peerAttemptOutcome) {
    contractError('peer attempt outcome does not match the failed-dominant reducer')
  }
  if (common.resultStatus === 'provisional' && peerAttemptOutcome !== 'not-started') {
    contractError('provisional main result must not assert a peer terminal')
  }
  if (common.resultStatus === 'provisional' && deliveryOutcome !== 'not-started') {
    contractError('provisional main result must not assert a delivery terminal')
  }
  if (common.resultStatus === 'provisional' && routeEvidence !== null) {
    contractError('provisional main result cannot assert route evidence')
  }
  if (common.resultStatus !== 'provisional' && routeEvidence?.mode === 'hot-switch') {
    validateHotSwitchAttemptCorrelation(routeEvidence, attempts)
  }
  return freezeRecord({
    ...common,
    suite: requireLiteral(record.suite, 'main', 'main result suite'),
    peerAttemptOutcome,
    deliveryOutcome,
    attempts,
    deliveryEvidence,
    routeEvidence,
  })
}

function parsePionResult(
  record: JsonRecord,
  topology: TestIceTopology,
  resolution: TestIceTopologyResolution,
  expectedProfileSha256: string,
  expectedResolutionSha256: string,
): PionBrowserSampleResult {
  requireExactKeys(
    record,
    [
      ...commonResultFields(),
      'suite',
      'applicability',
      'nativeInteropOutcome',
      'nativeInteropEvidence',
    ],
    [],
    'Pion browser sample result',
  )
  const common = parseCommonResult(
    record,
    topology,
    resolution,
    expectedProfileSha256,
    expectedResolutionSha256,
  )
  const applicability = requireEnum(
    record.applicability,
    PION_APPLICABILITY,
    'Pion suite applicability',
  )
  const nativeInteropOutcome = requireEnum(
    record.nativeInteropOutcome,
    NATIVE_INTEROP_OUTCOMES,
    'native Pion interop outcome',
  )
  const nativeInteropEvidence = parseNativeInteropEvidence(
    record.nativeInteropEvidence,
    nativeInteropOutcome,
    topology,
    resolution,
  )
  validatePionCombination(common, applicability, nativeInteropOutcome, nativeInteropEvidence)
  return freezeRecord({
    ...common,
    suite: requireLiteral(record.suite, 'pion', 'Pion result suite'),
    applicability,
    nativeInteropOutcome,
    nativeInteropEvidence,
  })
}

function parseCommonResult(
  record: JsonRecord,
  topology: TestIceTopology,
  resolution: TestIceTopologyResolution,
  expectedProfileSha256: string,
  expectedResolutionSha256: string,
): SampleResultCommon {
  const parsedTopology = parseTestIceTopology(topology)
  parseTestIceTopologyResolution(resolution, parsedTopology, expectedProfileSha256)
  const resultStatus = requireEnum(record.resultStatus, RESULT_STATUSES, 'result status')
  const capabilityEvidence = parseCapabilityEvidence(record.capabilityEvidence)
  const rtcCapability = requireEnum(record.rtcCapability, RTC_CAPABILITIES, 'RTC capability')
  if (classifyRtcCapability(capabilityEvidence) !== rtcCapability) {
    contractError('RTC capability does not match capability evidence')
  }
  const executionEvidence = parseExecutionEvidence(record.executionEvidence)
  const executionOutcome = requireEnum(
    record.executionOutcome,
    EXECUTION_OUTCOMES,
    'execution outcome',
  )
  if (classifyExecutionOutcome(executionEvidence) !== executionOutcome) {
    contractError('execution outcome does not match execution evidence')
  }
  const integrityViolations = Object.freeze(requireArray(
    record.integrityViolations,
    'result integrity violations',
  ).map((item, index) => requireString(item, `integrity violation ${index}`, 1_024))
    .sort(compareStrings))
  if (new Set(integrityViolations).size !== integrityViolations.length) {
    contractError('result integrity violations contain duplicates')
  }
  if (resultStatus === 'final-invalid' ? integrityViolations.length === 0 : integrityViolations.length !== 0) {
    contractError('only a final-invalid result must carry integrity violations')
  }
  const playwrightOutcome = requireEnum(
    record.playwrightOutcome,
    PLAYWRIGHT_OUTCOMES,
    'Playwright outcome',
  )
  validateRunnerProcessVerdict(resultStatus, playwrightOutcome, executionEvidence)
  const artifacts = parseArtifacts(record.artifacts)
  if (resultStatus === 'provisional') {
    if (
      rtcCapability !== 'unknown' || executionOutcome !== 'unknown' ||
      playwrightOutcome !== 'not-started' || capabilityEvidence.apiPresence !== 'unknown' ||
      artifacts.length !== 0
    ) {
      contractError('provisional result must retain unknown/not-started outcomes')
    }
  }
  if (
    resultStatus === 'final-valid' &&
    (executionOutcome === 'unknown' || playwrightOutcome === 'not-started')
  ) {
    contractError('final-valid result requires terminal execution and Playwright evidence')
  }
  const topologyProfileSha256 = requireSha256(
    record.topologyProfileSha256,
    'result topology profile SHA-256',
  )
  if (
    topologyProfileSha256 !== requireSha256(expectedProfileSha256, 'expected topology profile SHA-256')
  ) {
    contractError('result topology profile SHA-256 does not match the selected profile')
  }
  const topologyResolutionSha256 = requireSha256(
    record.topologyResolutionSha256,
    'result topology resolution SHA-256',
  )
  if (
    topologyResolutionSha256 !== requireSha256(
      expectedResolutionSha256,
      'expected topology resolution SHA-256',
    )
  ) contractError('result topology resolution SHA-256 does not match the selected resolution')
  const runPolicy = parseBrowserRunPolicy(record.runPolicy, 'result run policy')
  const sampleIndex = requireSafeInteger(
    record.sampleIndex,
    1,
    runPolicy.sampleCount,
    'sample index',
  )
  validatePolicySampleIndex(sampleIndex, runPolicy)
  return freezeRecord({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      BROWSER_EVIDENCE_SCHEMA_VERSION,
      'browser result schema version',
    ),
    resultStatus,
    runId: requireRunId(record.runId),
    runPolicy,
    browser: requireEnum(record.browser, BROWSER_ENGINES, 'browser engine'),
    sampleIndex,
    checkoutSha: requireCheckoutSha(record.checkoutSha, 'checkout SHA'),
    topologyId: requireLiteral(record.topologyId, parsedTopology.topologyId, 'result topology ID'),
    topologyProfileSha256,
    topologyResolutionSha256,
    rtcCapability,
    capabilityEvidence,
    executionOutcome,
    executionEvidence,
    playwrightOutcome,
    artifacts,
    integrityViolations,
  })
}

function parseDeliveryEvidence(value: unknown, outcome: DeliveryOutcome): DeliveryEvidence | null {
  if (value === null) {
    if (outcome !== 'not-started') contractError('started delivery outcome lacks delivery evidence')
    return null
  }
  if (outcome === 'not-started') contractError('not-started delivery cannot carry delivery evidence')
  const evidence = requireRecord(value, 'delivery evidence')
  requireExactKeys(
    evidence,
    ['expectedBytes', 'receivedBytes', 'expectedSha256', 'receivedSha256', 'terminal'],
    [],
    'delivery evidence',
  )
  const receivedDigest = evidence.receivedSha256 === null
    ? null
    : requireSha256(evidence.receivedSha256, 'received delivery SHA-256')
  const result = freezeRecord({
    expectedBytes: requireSafeInteger(
      evidence.expectedBytes,
      MAIN_TRANSFER_BYTES,
      MAIN_TRANSFER_BYTES,
      'expected delivery bytes',
    ),
    receivedBytes: requireSafeInteger(
      evidence.receivedBytes,
      0,
      MAIN_TRANSFER_BYTES,
      'received delivery bytes',
    ),
    expectedSha256: requireLiteral(
      evidence.expectedSha256,
      MAIN_TRANSFER_SHA256,
      'expected delivery SHA-256',
    ),
    receivedSha256: receivedDigest,
    terminal: requireEnum(evidence.terminal, DELIVERY_TERMINALS, 'delivery terminal'),
  })
  if (result.terminal !== outcome) contractError('delivery outcome does not match its terminal evidence')
  if (
    outcome === 'succeeded' &&
    (
      result.receivedBytes !== MAIN_TRANSFER_BYTES ||
      result.receivedSha256 !== MAIN_TRANSFER_SHA256
    )
  ) {
    contractError('succeeded delivery does not prove exact bytes and SHA-256')
  }
  return result
}

function parseNativeInteropEvidence(
  value: unknown,
  outcome: NativeInteropOutcome,
  topology: TestIceTopology,
  resolution: TestIceTopologyResolution,
): NativeInteropEvidence | null {
  if (value === null) {
    if (outcome !== 'not-started') contractError('native interop terminal lacks evidence')
    return null
  }
  if (outcome === 'not-started') contractError('not-started native interop cannot carry evidence')
  const evidence = requireRecord(value, 'native interop evidence')
  requireExactKeys(
    evidence,
    ['browser', 'pion'],
    ['failureCode', 'failureMessage'],
    'native interop evidence',
  )
  const browser = parseNativeInteropSide(
    evidence.browser,
    'browser',
    parseBrowserSelectedPair,
  )
  const pion = parseNativeInteropSide(evidence.pion, 'Pion', parsePionSelectedPair)
  if (browser.attemptId !== pion.attemptId) {
    contractError('native browser and Pion evidence identify different attempts')
  }
  const failureCodeValue = optionalField(evidence, 'failureCode')
  const failureMessageValue = optionalField(evidence, 'failureMessage')
  if (outcome === 'failed' ? failureCodeValue === undefined || failureMessageValue === undefined :
      failureCodeValue !== undefined || failureMessageValue !== undefined) {
    contractError('only failed native interop must carry failure code and message')
  }
  if (
    outcome === 'succeeded' &&
    (browser.selectedPair === null || pion.selectedPair === null ||
      !selectedPairAllowedByTopology(browser.selectedPair, topology, resolution) ||
      !selectedPairAllowedByTopology(pion.selectedPair, topology, resolution) ||
      !selectedPairsCorrelate(browser.selectedPair, pion.selectedPair))
  ) {
    contractError('succeeded native interop lacks a correlated direct browser/Pion selected pair')
  }
  return freezeRecord({
    browser,
    pion,
    ...(failureCodeValue === undefined
      ? {}
      : {
          failureCode: requireEnum(
            failureCodeValue,
            NATIVE_INTEROP_FAILURE_CODES,
            'native interop failure code',
          ),
        }),
    ...(failureMessageValue === undefined
      ? {}
      : { failureMessage: requireString(failureMessageValue, 'native interop failure message', 512) }),
  })
}

function validatePionCombination(
  common: SampleResultCommon,
  applicability: PionApplicability,
  outcome: NativeInteropOutcome,
  evidence: NativeInteropEvidence | null,
): void {
  if (common.resultStatus === 'provisional') {
    if (applicability !== 'unknown' || outcome !== 'not-started' || evidence !== null) {
      contractError('provisional Pion result must retain unknown/not-started applicability')
    }
    return
  }
  const expectedApplicability = applicabilityForApiPresence(common.capabilityEvidence.apiPresence)
  if (applicability !== expectedApplicability) {
    contractError('Pion applicability must be derived from authoritative RTC API presence')
  }
  if (applicability !== 'applicable') {
    if (outcome !== 'not-started' || evidence !== null) {
      contractError('unknown or absent RTC API cannot carry native Pion attempt evidence')
    }
    return
  }
  if (common.resultStatus === 'final-valid' && (outcome === 'not-started' || evidence === null)) {
    contractError('final API-present Pion result requires a native interop terminal')
  }
}

function applicabilityForApiPresence(
  presence: CapabilityEvidence['apiPresence'],
): PionApplicability {
  if (presence === 'unknown') return 'unknown'
  return presence === 'absent' ? 'not-applicable' : 'applicable'
}

/** Runtime admission remains an observed fact when pair proof is absent. This
 * predicate gives the later verdict one explicit authority for rejecting that
 * otherwise admitted sample without rewriting its peer outcome. */
export function hasDirectSelectedPairProof(
  attempts: readonly LogicalAttempt[],
  topology: TestIceTopology,
  resolution: TestIceTopologyResolution,
): boolean {
  const admitted = attempts.filter((attempt) => attempt.outcome === 'admitted')
  return admitted.length > 0 && admitted.every((attempt) => {
    const browser = attempt.events.find(({ evidence }) =>
      evidence.side === 'browser' && evidence.stage === 'admitted')?.evidence
    const sender = attempt.events.find(({ evidence }) =>
      evidence.side === 'sender' && evidence.stage === 'admitted')?.evidence
    if (
      browser?.side !== 'browser' || browser.stage !== 'admitted' ||
      sender?.side !== 'sender' || sender.stage !== 'admitted' ||
      browser.selectedPair === null || sender.selectedPair === null
    ) {
      return false
    }
    return selectedPairAllowedByTopology(browser.selectedPair, topology, resolution) &&
      selectedPairAllowedByTopology(sender.selectedPair, topology, resolution) &&
      selectedPairsCorrelate(browser.selectedPair, sender.selectedPair)
  })
}

function validateHotSwitchAttemptCorrelation(
  route: MainRouteEvidence,
  attempts: readonly LogicalAttempt[],
): void {
  const admission = route.observations.find(
    (observation): observation is PeerAdmissionObservation => observation.kind === 'peer-admitted',
  )
  if (admission === undefined) contractError('hot-switch route evidence lacks peer admission')
  const matches = attempts.filter((attempt) =>
    attempt.sessionId === admission.sessionId && attempt.peerPathId === admission.peerPathId &&
    attempt.attemptId === admission.attemptId)
  if (matches.length !== 1 || matches[0]?.outcome !== 'admitted') {
    contractError('hot-switch route admission does not identify one admitted logical attempt')
  }
  const browserAdmission = matches[0].events.find(({ evidence }) =>
    evidence.side === 'browser' && evidence.stage === 'admitted')?.evidence
  if (
    browserAdmission?.side !== 'browser' || browserAdmission.stage !== 'admitted' ||
    browserAdmission.lane.laneId !== admission.lane.laneId ||
    browserAdmission.lane.laneEpoch !== admission.lane.laneEpoch
  ) {
    contractError('hot-switch route admission lane differs from attempt admission')
  }
}

function parseNativeInteropSide<TSelectedPair>(
  value: unknown,
  side: string,
  parseSelectedPair: (pair: unknown) => TSelectedPair,
): NativeInteropSideEvidence<TSelectedPair> {
  const evidence = requireRecord(value, `${side} native interop evidence`)
  requireExactKeys(
    evidence,
    ['attemptId', 'selectedPair'],
    [],
    `${side} native interop evidence`,
  )
  return freezeRecord({
    attemptId: requireAttemptIdentity(evidence.attemptId),
    selectedPair: evidence.selectedPair === null ? null : parseSelectedPair(evidence.selectedPair),
  })
}

function selectedPairsCorrelate(
  browser: BrowserSelectedPairEvidence,
  pion: PionSelectedPairEvidence,
): boolean {
  return browserLocalEndpointMatchesPionRemote(browser.local, pion.remote) &&
    browserRemoteEndpointMatchesPionLocal(browser.remote, pion.local)
}

function browserLocalEndpointMatchesPionRemote(
  browser: BrowserSelectedPairEvidence['local'],
  pion: PionSelectedPairEvidence['local'],
): boolean {
  if (browser.protocol !== pion.protocol) return false
  if (browser.port !== undefined && browser.port !== pion.port) return false
  if (browser.address === undefined) return browser.candidateType === 'host'
  if (isIpLiteral(browser.address)) return browser.address === pion.address
  return browser.candidateType === 'host' && isMdnsHostname(browser.address)
}

function browserRemoteEndpointMatchesPionLocal(
  browser: BrowserSelectedPairEvidence['remote'],
  pion: PionSelectedPairEvidence['local'],
): boolean {
  return browser.address !== undefined && isIpLiteral(browser.address) &&
    browser.address === pion.address && browser.port === pion.port &&
    browser.protocol === pion.protocol
}

function isIpLiteral(address: string): boolean {
  return address.includes(':') || /^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/u.test(address)
}

function isMdnsHostname(address: string): boolean {
  return /^(?=.{1,253}\.?$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+local\.?$/u
    .test(address)
}

function parseArtifacts(value: unknown): readonly ArtifactIndexEntry[] {
  const identities = new Set<string>()
  const paths = new Set<string>()
  const portablePaths = new Set<string>()
  const artifacts = requireArray(value, 'artifact index').map((item, index) => {
    const artifact = requireRecord(item, `artifact ${index}`)
    requireExactKeys(
      artifact,
      ['artifactId', 'kind', 'relativePath', 'mediaType', 'byteLength', 'sha256'],
      [],
      `artifact ${index}`,
    )
    const relativePath = requireRelativeArtifactPath(artifact.relativePath, index)
    const parsed = freezeRecord({
      artifactId: requireArtifactId(artifact.artifactId, index),
      kind: requireEnum(artifact.kind, ARTIFACT_KINDS, `artifact ${index} kind`),
      relativePath,
      mediaType: requireString(artifact.mediaType, `artifact ${index} media type`, 128),
      byteLength: requireSafeInteger(
        artifact.byteLength,
        0,
        Number.MAX_SAFE_INTEGER,
        `artifact ${index} byte length`,
      ),
      sha256: requireSha256(artifact.sha256, `artifact ${index} SHA-256`),
    })
    if (parsed.artifactId !== artifactIdForManifest(parsed)) {
      contractError(`artifact ${index} ID does not bind its exact manifest`)
    }
    const artifactId = parsed.artifactId
    const portablePath = portablePathCollisionKey(relativePath)
    if (identities.has(artifactId)) contractError(`artifact ID ${artifactId} appears more than once`)
    if (paths.has(relativePath)) contractError(`artifact path ${relativePath} appears more than once`)
    if (portablePaths.has(portablePath)) {
      contractError(`artifact path ${relativePath} collides on a portable filesystem`)
    }
    identities.add(artifactId)
    paths.add(relativePath)
    portablePaths.add(portablePath)
    return parsed
  }).sort((left, right) => comparePortablePaths(left.relativePath, right.relativePath) ||
    compareStrings(left.artifactId, right.artifactId))
  return Object.freeze(artifacts)
}

function parseProvisionalAttempts(value: unknown): readonly LogicalAttempt[] {
  const attempts = requireArray(value, 'provisional attempts')
  if (attempts.length !== 0) {
    contractError('provisional result cannot assert peer attempt evidence')
  }
  return Object.freeze([])
}

function commonResultFields(): readonly string[] {
  return [
    'schemaVersion',
    'resultStatus',
    'runId',
    'runPolicy',
    'browser',
    'sampleIndex',
    'checkoutSha',
    'topologyId',
    'topologyProfileSha256',
    'topologyResolutionSha256',
    'rtcCapability',
    'capabilityEvidence',
    'executionOutcome',
    'executionEvidence',
    'playwrightOutcome',
    'artifacts',
    'integrityViolations',
  ]
}

function requireRunId(value: unknown): string {
  const runId = requireString(value, 'browser run ID', 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(runId)) {
    contractError('browser run ID contains non-portable characters')
  }
  return runId
}

function requireAttemptIdentity(value: unknown): string {
  return requireCanonicalIdentity(value, 'native attempt ID')
}

function requireArtifactId(value: unknown, index: number): string {
  const artifactId = requireString(value, `artifact ${index} ID`, 128)
  if (!/^[A-Za-z0-9._-]+$/u.test(artifactId)) {
    contractError(`artifact ${index} ID contains non-portable characters`)
  }
  return artifactId
}

function requireRelativeArtifactPath(value: unknown, index: number): string {
  try {
    return requirePortableRelativePath(value, `artifact ${index} relative path`)
  } catch (cause) {
    contractError(cause instanceof Error ? cause.message : String(cause))
  }
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
