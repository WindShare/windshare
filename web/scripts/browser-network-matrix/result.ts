import {
  NETWORK_MATRIX_BROWSERS,
  NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
  NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
  NETWORK_MATRIX_ORCHESTRATION_OUTCOMES,
  NETWORK_MATRIX_RATIONALE_CODES,
  NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
  NETWORK_MATRIX_SAMPLE_ORDINALS,
  NETWORK_MATRIX_SAMPLE_OUTCOMES,
  NETWORK_RUN_RESULT_SCHEMA,
  NETWORK_SAMPLE_RESULT_SCHEMA,
  type NetworkMatrixCandidatePolicyOutcome,
  type NetworkMatrixExecutionMode,
  type NetworkMatrixPrerequisiteOutcome,
  type NetworkMatrixProfileId,
  type NetworkMatrixProfileRunOutcome,
  type NetworkMatrixRationaleCode,
  type NetworkMatrixRunOutcome,
  type NetworkMatrixSampleOrdinal,
  type NetworkMatrixSampleOutcome,
} from './vocabulary.ts'
import {
  networkMatrixIdentities,
  networkMatrixIdentityKey,
  sha256,
  type LoadedNetworkMatrixRegistry,
  type NetworkMatrixIdentity,
  type NetworkMatrixManifest,
  type NetworkMatrixProfileReference,
} from './manifest.ts'
import {
  canonicalNetworkRuntimeAttestationJson,
  authenticatePinnedExternalFixtureAttestation,
  parseNetworkRuntimeAttestation,
  type NetworkRuntimeAttestation,
  type NetworkRuntimeAttestationContext,
} from './attestation.ts'
import { evaluateNetworkCandidatePolicy } from './candidate.ts'
import {
  parseNetworkMatrixAttemptEvidence,
  networkMatrixPionSelectedPairFromTerminalReceipt,
  type NetworkMatrixAttemptEvidence,
} from './attempt-evidence.ts'
import {
  networkMatrixAttemptAuthoritySha256,
  sameNetworkMatrixAttemptAuthority,
} from './sample-authority.ts'
import { authenticateSignedExternalFixtureTerminalReceipt } from './linux-topology/external-fixture-terminal-receipt.ts'
import type { NetworkTopologyProfile } from './profile.ts'
import {
  networkMatrixError,
  parseNetworkMatrixJsonText,
  requireArray,
  requireCanonicalEncoding,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireRunId,
  requireSafeInteger,
  requireSha256,
} from './contract-support.ts'

export interface NetworkSampleFailure {
  readonly failureCode: (typeof NETWORK_MATRIX_SAMPLE_FAILURE_CODES)[number]
}

export interface NetworkSampleResult {
  readonly schemaVersion: typeof NETWORK_SAMPLE_RESULT_SCHEMA
  readonly runId: string
  readonly manifestSha256: string
  readonly identity: NetworkMatrixIdentity
  readonly profileSha256: string
  readonly attestationSha256: string
  readonly sampleOutcome: NetworkMatrixSampleOutcome
  readonly processInstanceId: string | null
  readonly attemptEvidence: NetworkMatrixAttemptEvidence | null
  readonly candidatePolicyOutcome: NetworkMatrixCandidatePolicyOutcome
  readonly rationaleCodes: readonly NetworkMatrixRationaleCode[]
  readonly failure: NetworkSampleFailure | null
}

export interface NetworkOrchestrationFailure {
  readonly failureCode: (typeof NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES)[number]
}

export interface NetworkProfileRunResult {
  readonly profileId: NetworkMatrixProfileId
  readonly prerequisiteOutcome: NetworkMatrixPrerequisiteOutcome
  readonly expectedSamples: typeof NETWORK_MATRIX_IDENTITIES_PER_PROFILE
  readonly observedSamples: number
  readonly sampleInfrastructureFailures: number
  readonly profileOutcome: NetworkMatrixProfileRunOutcome
}

export interface NetworkRunResult {
  readonly schemaVersion: typeof NETWORK_RUN_RESULT_SCHEMA
  readonly runId: string
  readonly manifestSha256: string
  readonly executionMode: NetworkMatrixExecutionMode
  readonly orchestrationOutcome: (typeof NETWORK_MATRIX_ORCHESTRATION_OUTCOMES)[number]
  readonly orchestrationFailure: NetworkOrchestrationFailure | null
  readonly expectedIdentities: readonly NetworkMatrixIdentity[]
  readonly runtimeAttestations: readonly NetworkRuntimeAttestation[]
  readonly samples: readonly NetworkSampleResult[]
  readonly profileResults: readonly NetworkProfileRunResult[]
  readonly runOutcome: NetworkMatrixRunOutcome
}

interface NetworkSampleContext extends NetworkRuntimeAttestationContext {
  readonly profiles: readonly NetworkTopologyProfile[]
  readonly attestations: readonly NetworkRuntimeAttestation[]
}

export function parseNetworkSampleResult(
  value: unknown,
  context: NetworkSampleContext,
): NetworkSampleResult {
  const record = requireRecord(value, 'browser network matrix sample result')
  requireExactKeys(record, [
    'schemaVersion',
    'runId',
    'manifestSha256',
    'identity',
    'profileSha256',
    'attestationSha256',
    'sampleOutcome',
    'processInstanceId',
    'attemptEvidence',
    'candidatePolicyOutcome',
    'rationaleCodes',
    'failure',
  ], 'browser network matrix sample result')
  const identity = parseNetworkMatrixIdentity(record.identity, context.manifest)
  const profileReference = context.manifest.profiles.find(
    (profile) => profile.profileId === identity.profileId,
  )
  const profile = context.profiles.find((candidate) => candidate.profileId === identity.profileId)
  const attestation = context.attestations.find(
    (candidate) => candidate.profileId === identity.profileId,
  )
  if (profileReference === undefined || profile === undefined || attestation === undefined) {
    networkMatrixError('sample identity lacks a loaded profile and runtime attestation')
  }
  if (attestation.prerequisiteOutcome !== 'satisfied') {
    networkMatrixError('a sample cannot exist for an unsatisfied runtime prerequisite')
  }
  const sampleOutcome = requireEnum(
    record.sampleOutcome,
    NETWORK_MATRIX_SAMPLE_OUTCOMES,
    'network sample outcome',
  )
  let processInstanceId: string | null
  let attemptEvidence: NetworkMatrixAttemptEvidence | null
  let candidatePolicyOutcome: NetworkMatrixCandidatePolicyOutcome
  let rationaleCodes: readonly NetworkMatrixRationaleCode[]
  let failure: NetworkSampleFailure | null
  if (sampleOutcome === 'observed') {
    processInstanceId = requireRunId(
      record.processInstanceId,
      'network matrix browser process instance ID',
    )
    attemptEvidence = parseNetworkMatrixAttemptEvidence(record.attemptEvidence, identity.profileId)
    authenticateSampleAttempt(
      attemptEvidence,
      profileReference,
      profile,
      attestation,
      context,
      identity,
      processInstanceId,
    )
    const evaluation = evaluateNetworkCandidatePolicy(attemptEvidence.browserSelectedPair, profile)
    candidatePolicyOutcome = requireLiteral(
      record.candidatePolicyOutcome,
      evaluation.candidatePolicyOutcome,
      'network candidate policy outcome',
    )
    rationaleCodes = requireExactRationales(record.rationaleCodes, evaluation.rationaleCodes)
    failure = requireNull(record.failure, 'observed network sample failure')
  } else {
    processInstanceId = requireNull(
      record.processInstanceId,
      'infrastructure-failed browser process instance ID',
    )
    attemptEvidence = requireNull(
      record.attemptEvidence,
      'infrastructure-failed attempt evidence',
    )
    candidatePolicyOutcome = requireLiteral(
      record.candidatePolicyOutcome,
      'not-evaluated',
      'infrastructure-failed candidate policy outcome',
    )
    rationaleCodes = requireExactRationales(record.rationaleCodes, [])
    failure = parseSampleFailure(record.failure)
  }
  const attestationContext = attestationContextFor(context)
  return Object.freeze({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      NETWORK_SAMPLE_RESULT_SCHEMA,
      'network sample result schema',
    ),
    runId: requireLiteral(
      requireRunId(record.runId, 'network sample run ID'),
      context.runId,
      'network sample run ID',
    ),
    manifestSha256: requireLiteral(
      requireSha256(record.manifestSha256, 'network sample manifest digest'),
      context.manifestSha256,
      'network sample manifest digest',
    ),
    identity,
    profileSha256: requireLiteral(
      requireSha256(record.profileSha256, 'network sample profile digest'),
      profileReference.profileSha256,
      'network sample profile digest',
    ),
    attestationSha256: requireLiteral(
      requireSha256(record.attestationSha256, 'network sample attestation digest'),
      sha256(canonicalNetworkRuntimeAttestationJson(attestation, attestationContext)),
      'network sample attestation digest',
    ),
    sampleOutcome,
    processInstanceId,
    attemptEvidence,
    candidatePolicyOutcome,
    rationaleCodes,
    failure,
  })
}

function authenticateSampleAttempt(
  evidence: NetworkMatrixAttemptEvidence,
  profileReference: NetworkMatrixProfileReference,
  profile: NetworkTopologyProfile,
  runtimeAttestation: NetworkRuntimeAttestation,
  context: NetworkSampleContext,
  identity: NetworkMatrixIdentity,
  processInstanceId: string,
): void {
  const sample = evidence.externalFixture
  const pinned = authenticatePinnedExternalFixtureAttestation(
    sample.signedAttestation,
    sample.attestationPublicKeySpki,
    profileReference,
    profile.authority,
    context.manifest,
  )
  const authenticated = pinned.authenticated
  const signedAttestation = authenticated.attestation
  const fixture = signedAttestation.fixture
  const runtimeTrust = runtimeAttestation.proof?.externalFixtureTrust
  if (runtimeTrust === undefined) {
    networkMatrixError('observed sample lacks a satisfied external fixture trust proof')
  }
  const issuedAt = Date.parse(signedAttestation.issuedAt)
  const expiresAt = Date.parse(signedAttestation.expiresAt)
  if (
    expiresAt - issuedAt !== signedAttestation.leaseMillis ||
    fixture.networkSemantics.kind === 'coturn-relay' &&
      fixture.networkSemantics.turnCredentialExpiresAt !== signedAttestation.expiresAt
  ) networkMatrixError('sample-local external fixture attestation lifetime is inconsistent')
  if (
    pinned.attestationPublicKeySpki !== runtimeTrust.attestationPublicKeySpki ||
    signedAttestation.runId !== context.runId || sample.runId !== context.runId ||
    fixture.profileId !== profile.profileId ||
    sample.attestationSha256 !== authenticated.attestationSha256 ||
    sample.authorityInstanceId !== fixture.authorityInstanceId ||
    sample.remoteServiceInstanceId !== fixture.remoteServiceInstanceId ||
    sample.networkBindingSha256 !== authenticated.networkBindingSha256 ||
    sample.remotePeerBindingSha256 !== authenticated.remotePeerBindingSha256 ||
    sample.controllerPublicIp !== fixture.controllerPublicIp ||
    sample.attestationExpiresAt !== signedAttestation.expiresAt ||
    sample.remotePeerPublicIp !== fixture.remotePeerPublicIp ||
    sample.remotePeerUdpPortMin !== fixture.remotePeerUdpPortMin ||
    sample.remotePeerUdpPortMax !== fixture.remotePeerUdpPortMax
  ) networkMatrixError('sample attempt differs from its signed external fixture declaration')
  const terminal = authenticateSignedExternalFixtureTerminalReceipt(
    evidence.terminalReceipt,
    pinned.attestationPublicKeyPem,
  )
  const attemptIssuedAt = Date.parse(terminal.attemptLeaseIssuedAt)
  const attemptExpiresAt = Date.parse(terminal.attemptLeaseExpiresAt)
  const terminalAt = Date.parse(terminal.terminalAt)
  const terminalPair = networkMatrixPionSelectedPairFromTerminalReceipt(terminal.selectedPair)
  const expectedBlocked = profile.profileId === 'scheduled-restricted-udp'
  const attemptAuthority = terminal.attemptAuthority
  const sampleAuthority = attemptAuthority.requestAuthority.controlAuthority.sampleAuthority
  const fixtureBinding = attemptAuthority.requestAuthority.fixtureBinding
  if (
    !sameNetworkMatrixAttemptAuthority(attemptAuthority, evidence.attemptAuthority) ||
    sampleAuthority.runId !== context.runId || sampleAuthority.profileId !== profile.profileId ||
    sampleAuthority.browser !== identity.browser ||
    sampleAuthority.sampleOrdinal !== identity.sampleOrdinal ||
    sampleAuthority.processInstanceId !== processInstanceId ||
    fixtureBinding.attestationSha256 !== authenticated.attestationSha256 ||
    fixtureBinding.authorityInstanceId !== fixture.authorityInstanceId ||
    fixtureBinding.remoteServiceInstanceId !== fixture.remoteServiceInstanceId ||
    fixtureBinding.networkBindingSha256 !== authenticated.networkBindingSha256 ||
    fixtureBinding.remotePeerBindingSha256 !== authenticated.remotePeerBindingSha256 ||
    terminal.challengeBindingSha256 !== networkMatrixAttemptAuthoritySha256(attemptAuthority) ||
    attemptExpiresAt - attemptIssuedAt !== terminal.attemptLeaseMillis ||
    attemptIssuedAt < issuedAt || attemptExpiresAt > expiresAt ||
    terminalAt < attemptIssuedAt || terminalAt >= attemptExpiresAt || terminalAt >= expiresAt ||
    JSON.stringify(terminalPair) !== JSON.stringify(evidence.pionSelectedPair) ||
    expectedBlocked && (terminal.state !== 'failed' || terminal.failureCode !== 'ice-failed') ||
    !expectedBlocked && (terminal.state !== 'established' || terminal.failureCode !== null) ||
    evidence.challenge !== null && (
      evidence.challenge.challenge !== attemptAuthority.challenge ||
      evidence.challenge.bindingSha256 !== terminal.challengeBindingSha256
    )
  ) networkMatrixError('signed terminal receipt differs from the exact sample attempt')
}

export function parseNetworkRunResult(
  value: unknown,
  registry: LoadedNetworkMatrixRegistry,
): NetworkRunResult {
  const record = requireRecord(value, 'browser network matrix run result')
  requireExactKeys(record, [
    'schemaVersion',
    'runId',
    'manifestSha256',
    'executionMode',
    'orchestrationOutcome',
    'orchestrationFailure',
    'expectedIdentities',
    'runtimeAttestations',
    'samples',
    'profileResults',
    'runOutcome',
  ], 'browser network matrix run result')
  const runId = requireRunId(record.runId, 'browser network matrix run ID')
  const manifestSha256 = requireLiteral(
    requireSha256(record.manifestSha256, 'browser network matrix run manifest digest'),
    registry.manifestSha256,
    'browser network matrix run manifest digest',
  )
  const executionMode = requireEnum(
    record.executionMode,
    ['scheduled', 'manual'] as const,
    'browser network matrix execution mode',
  )
  const expectedIdentities = parseExpectedIdentities(
    record.expectedIdentities,
    registry.manifest,
    executionMode,
  )
  const attestationContext = Object.freeze({
    manifest: registry.manifest,
    manifestSha256,
    runId,
  })
  const runtimeAttestations = parseRuntimeAttestations(
    record.runtimeAttestations,
    executionMode,
    attestationContext,
  )
  const sampleContext = Object.freeze({
    ...attestationContext,
    profiles: registry.profiles,
    attestations: runtimeAttestations,
  })
  const samples = parseSamples(record.samples, expectedIdentities, sampleContext)
  const orchestrationOutcome = requireEnum(
    record.orchestrationOutcome,
    NETWORK_MATRIX_ORCHESTRATION_OUTCOMES,
    'network matrix orchestration outcome',
  )
  const orchestrationFailure = orchestrationOutcome === 'healthy'
    ? requireNull(record.orchestrationFailure, 'healthy orchestration failure')
    : parseOrchestrationFailure(record.orchestrationFailure)
  const derivedOutcome = deriveNetworkRunOutcome(
    orchestrationOutcome,
    expectedIdentities,
    runtimeAttestations,
    samples,
  )
  const derivedProfileResults = deriveNetworkProfileResults(
    expectedIdentities,
    runtimeAttestations,
    samples,
  )
  const profileResults = parseNetworkProfileResults(
    record.profileResults,
    derivedProfileResults,
  )
  return Object.freeze({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      NETWORK_RUN_RESULT_SCHEMA,
      'network matrix run-result schema',
    ),
    runId,
    manifestSha256,
    executionMode,
    orchestrationOutcome,
    orchestrationFailure,
    expectedIdentities,
    runtimeAttestations,
    samples,
    profileResults,
    runOutcome: requireLiteral(record.runOutcome, derivedOutcome, 'network matrix run outcome'),
  })
}

export function parseNetworkSampleResultJson(
  encoded: string,
  context: NetworkSampleContext,
): NetworkSampleResult {
  return requireCanonicalEncoding(
    encoded,
    parseNetworkSampleResult(
      parseNetworkMatrixJsonText(encoded, 'browser network matrix sample result'),
      context,
    ),
    'browser network matrix sample result',
  )
}

export function parseNetworkRunResultJson(
  encoded: string,
  registry: LoadedNetworkMatrixRegistry,
): NetworkRunResult {
  return requireCanonicalEncoding(
    encoded,
    parseNetworkRunResult(
      parseNetworkMatrixJsonText(encoded, 'browser network matrix run result'),
      registry,
    ),
    'browser network matrix run result',
  )
}

export function canonicalNetworkRunResultJson(
  result: NetworkRunResult,
  registry: LoadedNetworkMatrixRegistry,
): string {
  return `${JSON.stringify(parseNetworkRunResult(result, registry))}\n`
}

function parseNetworkMatrixIdentity(
  value: unknown,
  manifest: NetworkMatrixManifest,
): NetworkMatrixIdentity {
  const identity = requireRecord(value, 'browser network matrix sample identity')
  requireExactKeys(
    identity,
    ['profileId', 'browser', 'sampleOrdinal'],
    'browser network matrix sample identity',
  )
  const profile = manifest.profiles.find((candidate) => candidate.profileId === identity.profileId)
  if (profile === undefined) networkMatrixError('network matrix sample profile is outside the manifest')
  const browser = requireEnum(identity.browser, NETWORK_MATRIX_BROWSERS, 'network matrix sample browser')
  const sampleOrdinal = requireSafeInteger(
    identity.sampleOrdinal,
    NETWORK_MATRIX_SAMPLE_ORDINALS[0],
    NETWORK_MATRIX_SAMPLE_ORDINALS.at(-1) ?? 5,
    'network matrix sample ordinal',
  )
  if (!NETWORK_MATRIX_SAMPLE_ORDINALS.includes(sampleOrdinal as NetworkMatrixSampleOrdinal)) {
    networkMatrixError('network matrix sample ordinal is outside the frozen registry')
  }
  return Object.freeze({
    profileId: profile.profileId,
    browser,
    sampleOrdinal: sampleOrdinal as NetworkMatrixSampleOrdinal,
  })
}

function parseExpectedIdentities(
  value: unknown,
  manifest: NetworkMatrixManifest,
  mode: NetworkMatrixExecutionMode,
): readonly NetworkMatrixIdentity[] {
  const expected = networkMatrixIdentities(manifest, mode)
  const identities = requireArray(value, 'network matrix expected identities').map((identity) =>
    parseNetworkMatrixIdentity(identity, manifest))
  if (
    identities.length !== expected.length ||
    identities.some((identity, index) =>
      networkMatrixIdentityKey(identity) !== networkMatrixIdentityKey(expected[index] as NetworkMatrixIdentity))
  ) networkMatrixError('network matrix expected identities differ from the exact mode expansion')
  return Object.freeze(identities)
}

function parseRuntimeAttestations(
  value: unknown,
  mode: NetworkMatrixExecutionMode,
  context: NetworkRuntimeAttestationContext,
): readonly NetworkRuntimeAttestation[] {
  const expectedProfiles = context.manifest.profiles.filter(
    (profile) => profile.executionMode === mode,
  )
  const attestations = requireArray(value, 'network runtime attestations').map((attestation) =>
    parseNetworkRuntimeAttestation(attestation, context))
  if (
    attestations.length !== expectedProfiles.length ||
    attestations.some((attestation, index) => attestation.profileId !== expectedProfiles[index]?.profileId)
  ) networkMatrixError('network runtime attestations differ from the exact mode profile registry')
  return Object.freeze(attestations)
}

function parseSamples(
  value: unknown,
  expectedIdentities: readonly NetworkMatrixIdentity[],
  context: NetworkSampleContext,
): readonly NetworkSampleResult[] {
  const expectedPositions = new Map(expectedIdentities.map((identity, index) => [
    networkMatrixIdentityKey(identity),
    index,
  ]))
  const processInstanceIds = new Set<string>()
  const attemptIds = new Set<string>()
  const challengeBindings = new Set<string>()
  let previous = -1
  const samples = requireArray(value, 'network matrix samples').map((sample) => {
    const parsed = parseNetworkSampleResult(sample, context)
    const position = expectedPositions.get(networkMatrixIdentityKey(parsed.identity))
    if (position === undefined) networkMatrixError('network matrix sample is outside the mode identity set')
    if (position <= previous) networkMatrixError('network matrix samples repeat or violate canonical identity order')
    requireUniqueObservedAuthorities(
      parsed,
      processInstanceIds,
      attemptIds,
      challengeBindings,
    )
    previous = position
    return parsed
  })
  return Object.freeze(samples)
}

function requireUniqueObservedAuthorities(
  sample: NetworkSampleResult,
  processInstanceIds: Set<string>,
  attemptIds: Set<string>,
  challengeBindings: Set<string>,
): void {
  if (sample.sampleOutcome !== 'observed') return
  if (sample.processInstanceId === null || sample.attemptEvidence === null) {
    networkMatrixError('observed sample lacks its process or attempt authority')
  }
  requireUniqueAuthority(
    processInstanceIds,
    sample.processInstanceId,
    'browser process instance ID',
  )
  requireUniqueAuthority(
    attemptIds,
    sample.attemptEvidence.attemptAuthority.attemptId,
    'attempt ID',
  )
  if (sample.attemptEvidence.challenge !== null) {
    requireUniqueAuthority(
      challengeBindings,
      sample.attemptEvidence.challenge.bindingSha256,
      'challenge binding digest',
    )
  }
}

function requireUniqueAuthority(observed: Set<string>, value: string, label: string): void {
  if (observed.has(value)) networkMatrixError(`network matrix repeats ${label} across observed samples`)
  observed.add(value)
}

function deriveNetworkRunOutcome(
  orchestrationOutcome: (typeof NETWORK_MATRIX_ORCHESTRATION_OUTCOMES)[number],
  expectedIdentities: readonly NetworkMatrixIdentity[],
  attestations: readonly NetworkRuntimeAttestation[],
  samples: readonly NetworkSampleResult[],
): NetworkMatrixRunOutcome {
  const satisfiedProfiles = new Set(attestations
    .filter(({ prerequisiteOutcome }) => prerequisiteOutcome === 'satisfied')
    .map(({ profileId }) => profileId))
  if (samples.some((sample) => !satisfiedProfiles.has(sample.identity.profileId))) {
    networkMatrixError('network matrix contains a sample for an unsatisfied profile')
  }
  if (orchestrationOutcome === 'failed') return 'infrastructure-failed'
  const requiredObserved = expectedIdentities.filter((identity) => satisfiedProfiles.has(identity.profileId))
  if (
    samples.length !== requiredObserved.length ||
    samples.some((sample, index) =>
      networkMatrixIdentityKey(sample.identity) !==
      networkMatrixIdentityKey(requiredObserved[index] as NetworkMatrixIdentity))
  ) {
    networkMatrixError('healthy orchestration omitted or reordered a satisfied profile sample')
  }
  if (samples.some(({ sampleOutcome }) => sampleOutcome === 'infrastructure-failed')) {
    return 'infrastructure-failed'
  }
  if (satisfiedProfiles.size === 0) return 'not-executed'
  if (satisfiedProfiles.size === attestations.length) return 'completed'
  return 'partial'
}

function deriveNetworkProfileResults(
  expectedIdentities: readonly NetworkMatrixIdentity[],
  attestations: readonly NetworkRuntimeAttestation[],
  samples: readonly NetworkSampleResult[],
): readonly NetworkProfileRunResult[] {
  return Object.freeze(attestations.map((attestation) => {
    const expectedSamples = expectedIdentities.filter(
      ({ profileId }) => profileId === attestation.profileId,
    ).length
    if (expectedSamples !== NETWORK_MATRIX_IDENTITIES_PER_PROFILE) {
      networkMatrixError('network profile result does not own exactly 15 expected identities')
    }
    const profileSamples = samples.filter(
      ({ identity }) => identity.profileId === attestation.profileId,
    )
    const sampleInfrastructureFailures = profileSamples.filter(
      ({ sampleOutcome }) => sampleOutcome === 'infrastructure-failed',
    ).length
    return Object.freeze({
      profileId: attestation.profileId,
      prerequisiteOutcome: attestation.prerequisiteOutcome,
      expectedSamples: NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
      observedSamples: profileSamples.length,
      sampleInfrastructureFailures,
      profileOutcome: deriveNetworkProfileOutcome(
        attestation.prerequisiteOutcome,
        profileSamples.length,
        sampleInfrastructureFailures,
      ),
    })
  }))
}

function deriveNetworkProfileOutcome(
  prerequisiteOutcome: NetworkMatrixPrerequisiteOutcome,
  observedSamples: number,
  sampleInfrastructureFailures: number,
): NetworkMatrixProfileRunOutcome {
  if (prerequisiteOutcome !== 'satisfied') return 'not-executed'
  if (
    observedSamples !== NETWORK_MATRIX_IDENTITIES_PER_PROFILE ||
    sampleInfrastructureFailures > 0
  ) {
    return 'infrastructure-failed'
  }
  return 'completed'
}

function parseNetworkProfileResults(
  value: unknown,
  expected: readonly NetworkProfileRunResult[],
): readonly NetworkProfileRunResult[] {
  const results = requireArray(value, 'network matrix profile results')
  if (results.length !== expected.length) {
    networkMatrixError('network matrix profile results differ from the execution-mode profile registry')
  }
  return Object.freeze(results.map((value, index) => {
    const result = requireRecord(value, `network matrix profile result ${index}`)
    requireExactKeys(result, [
      'profileId',
      'prerequisiteOutcome',
      'expectedSamples',
      'observedSamples',
      'sampleInfrastructureFailures',
      'profileOutcome',
    ], `network matrix profile result ${index}`)
    const derived = expected[index]
    if (derived === undefined) networkMatrixError('network profile result exceeds the execution-mode registry')
    return Object.freeze({
      profileId: requireLiteral(
        result.profileId,
        derived.profileId,
        `network profile result ${index} profile ID`,
      ),
      prerequisiteOutcome: requireLiteral(
        result.prerequisiteOutcome,
        derived.prerequisiteOutcome,
        `network profile result ${index} prerequisite outcome`,
      ),
      expectedSamples: requireLiteral(
        result.expectedSamples,
        derived.expectedSamples,
        `network profile result ${index} expected samples`,
      ),
      observedSamples: requireLiteral(
        requireSafeInteger(
          result.observedSamples,
          0,
          NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
          `network profile result ${index} observed samples`,
        ),
        derived.observedSamples,
        `network profile result ${index} observed samples`,
      ),
      sampleInfrastructureFailures: requireLiteral(
        requireSafeInteger(
          result.sampleInfrastructureFailures,
          0,
          NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
          `network profile result ${index} sample infrastructure failures`,
        ),
        derived.sampleInfrastructureFailures,
        `network profile result ${index} sample infrastructure failures`,
      ),
      profileOutcome: requireLiteral(
        result.profileOutcome,
        derived.profileOutcome,
        `network profile result ${index} outcome`,
      ),
    })
  }))
}

function parseSampleFailure(value: unknown): NetworkSampleFailure {
  const failure = requireRecord(value, 'network sample infrastructure failure')
  requireExactKeys(failure, ['failureCode'], 'network sample infrastructure failure')
  return Object.freeze({
    failureCode: requireEnum(
      failure.failureCode,
      NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
      'network sample infrastructure failure code',
    ),
  })
}

function parseOrchestrationFailure(value: unknown): NetworkOrchestrationFailure {
  const failure = requireRecord(value, 'network matrix orchestration failure')
  requireExactKeys(failure, ['failureCode'], 'network matrix orchestration failure')
  return Object.freeze({
    failureCode: requireEnum(
      failure.failureCode,
      NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
      'network matrix orchestration failure code',
    ),
  })
}

function requireExactRationales(
  value: unknown,
  expected: readonly NetworkMatrixRationaleCode[],
): readonly NetworkMatrixRationaleCode[] {
  const rationales = requireArray(value, 'network candidate policy rationale codes').map((code) =>
    requireEnum(code, NETWORK_MATRIX_RATIONALE_CODES, 'network candidate policy rationale code'))
  if (
    rationales.length !== expected.length ||
    rationales.some((rationale, index) => rationale !== expected[index])
  ) networkMatrixError('network candidate policy rationale codes differ from the derived evaluation')
  return Object.freeze(rationales)
}

function attestationContextFor(context: NetworkSampleContext): NetworkRuntimeAttestationContext {
  return Object.freeze({
    manifest: context.manifest,
    manifestSha256: context.manifestSha256,
    runId: context.runId,
  })
}

function requireNull(value: unknown, label: string): null {
  if (value !== null) networkMatrixError(`${label} must be null`)
  return null
}
