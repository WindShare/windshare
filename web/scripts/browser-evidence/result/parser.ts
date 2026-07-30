import {
  parseBrowserSelectedPair,
  parsePionSelectedPair,
} from '../attempt-evidence.ts'
import {
  parseLogicalAttempts,
  reducePeerAttemptOutcome,
  type LogicalAttempt,
} from '../attempt-collector.ts'
import { artifactIdForManifest } from '../artifact/manifest.ts'
import {
  comparePortablePaths,
  portablePathCollisionKey,
  requirePortableRelativePath,
} from '../filesystem/portable-path.ts'
import {
  classifyRtcCapability,
  parseCapabilityEvidence,
  type CapabilityEvidence,
} from '../capability.ts'
import {
  classifyExecutionOutcome,
  parseExecutionEvidence,
  validateRunnerProcessVerdict,
} from '../execution-evidence.ts'
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
} from '../contract/json.ts'
import {
  parseMainRouteEvidence,
} from '../route-evidence.ts'
import {
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
} from '../run-policy.ts'
import {
  parseTestIceTopology,
  parseTestIceTopologyResolution,
  readVerifiedTestIceTopologyLock,
  selectedPairAllowedByTopology,
  type TestIceTopology,
  type TestIceTopologyResolution,
  type VerifiedTestIceTopologyLock,
} from '../test-ice-topology.ts'
import {
  BROWSER_ENGINES,
  BROWSER_EVIDENCE_SCHEMA_VERSION,
  DELIVERY_OUTCOMES,
  EXECUTION_OUTCOMES,
  PEER_ATTEMPT_OUTCOMES,
  RESULT_STATUSES,
  RTC_CAPABILITIES,
  type DeliveryOutcome,
} from '../vocabulary.ts'
import {
  ARTIFACT_KINDS,
  DELIVERY_TERMINALS,
  MAIN_TRANSFER_BYTES,
  MAIN_TRANSFER_SHA256,
  NATIVE_INTEROP_FAILURE_CODES,
  NATIVE_INTEROP_OUTCOMES,
  PION_APPLICABILITY,
  PLAYWRIGHT_OUTCOMES,
  type ArtifactIndexEntry,
  type BrowserSampleResult,
  type DeliveryEvidence,
  type MainBrowserSampleResult,
  type NativeInteropEvidence,
  type NativeInteropOutcome,
  type NativeInteropSideEvidence,
  type PionApplicability,
  type PionBrowserSampleResult,
  type SampleResultCommon,
} from './contract.ts'
import {
  selectedPairsCorrelate,
  validateHotSwitchAttemptCorrelation,
} from './acceptance.ts'

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
