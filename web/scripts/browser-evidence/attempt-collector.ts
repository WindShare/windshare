import {
  parseAttemptEvidence,
  type AttemptEvidence,
  type CandidateCounts,
  type LaneIdentity,
} from './attempt-evidence.ts'
import {
  contractError,
  freezeRecord,
  requireArray,
  requireCanonicalIdentity,
  requireEnum,
  requireExactKeys,
  requireRecord,
  requireSafeInteger,
} from './contract/json.ts'
import {
  BROWSER_ATTEMPT_STAGES,
  PEER_ATTEMPT_OUTCOMES,
  SENDER_ATTEMPT_STAGES,
  type AttemptSide,
  type AttemptStage,
  type PeerAttemptOutcome,
} from './vocabulary.ts'

const BROWSER_SUCCESS_CHAIN = BROWSER_ATTEMPT_STAGES.filter((stage) => stage !== 'failed')
const SENDER_SUCCESS_CHAIN = SENDER_ATTEMPT_STAGES.filter((stage) => stage !== 'failed')

export interface ReceivedAttemptEvidence {
  readonly receiveSequence: number
  readonly evidence: AttemptEvidence
}

export interface LogicalAttempt {
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
  readonly outcome: Exclude<PeerAttemptOutcome, 'not-started'>
  readonly events: readonly ReceivedAttemptEvidence[]
}

export interface AttemptCollectionFinalization {
  readonly attempts: readonly LogicalAttempt[]
  readonly integrityViolations: readonly string[]
}

interface SideStreamState {
  readonly side: AttemptSide
  nextSuccessIndex: number
  lastSequence: number
  lastElapsedMs: number
  terminal: 'admitted' | 'failed' | undefined
  candidateCounts: CandidateCounts | undefined
  lane: LaneIdentity | undefined
  localGeneration: string | undefined
}

interface AttemptState {
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
  readonly streams: Map<AttemptSide, SideStreamState>
  readonly events: ReceivedAttemptEvidence[]
  lane: LaneIdentity | undefined
}

interface SessionLaneAuthorityState {
  readonly laneIdOwners: Map<number, string>
  readonly laneEpochOwners: Map<number, string>
}

/**
 * Validation deliberately happens before reduction. A missing terminal is lost
 * evidence, not a peer failure, and coercing it into a fixed outcome would make
 * the browser gate claim a runtime fact it never observed.
 */
export class AttemptCollector {
  readonly #attempts = new Map<string, AttemptState>()
  readonly #laneAuthorityBySession = new Map<string, SessionLaneAuthorityState>()
  #receiveSequence = 0
  #finalized = false
  #rejectedEvidence = false

  ingest(value: unknown): ReceivedAttemptEvidence {
    if (this.#finalized) contractError('attempt collector is already finalized')
    if (this.#rejectedEvidence) contractError('attempt collector previously rejected evidence')
    try {
      const evidence = parseAttemptEvidence(value)
      const key = attemptKey(evidence)
      const attempt = cloneAttemptState(this.#attempts.get(key), evidence)
      const laneAuthority = cloneSessionLaneAuthority(
        this.#laneAuthorityBySession.get(evidence.sessionId),
      )
      const stream = ensureSideStream(attempt, evidence.side)
      validateStreamEvent(stream, attempt, evidence)
      reserveObservedLane(laneAuthority, attempt)
      const receiveSequence = this.#receiveSequence + 1
      const received = freezeRecord({ receiveSequence, evidence })
      attempt.events.push(received)
      // Attempt progress and the session-wide allocation authority form one
      // transaction: otherwise rejected evidence could consume a lane identity
      // that no replayable producer event ever established.
      this.#attempts.set(key, attempt)
      this.#laneAuthorityBySession.set(evidence.sessionId, laneAuthority)
      this.#receiveSequence = receiveSequence
      return received
    } catch (cause) {
      // Discarding a rejected event and continuing would let a caller hide a
      // corrupt producer stream. One rejection permanently invalidates this
      // sample collector; the runner preserves the raw event as diagnostics.
      this.#rejectedEvidence = true
      throw cause
    }
  }

  finalize(): readonly LogicalAttempt[] {
    if (this.#finalized) contractError('attempt collector can only be finalized once')
    this.#finalized = true
    if (this.#rejectedEvidence) contractError('attempt collector cannot finalize after rejected evidence')
    const attempts = [...this.#attempts.values()]
      .sort((left, right) => compareAttemptKeys(attemptKey(left), attemptKey(right)))
      .map((attempt) => finalizeAttempt(attempt))
    return Object.freeze(attempts)
  }

  finalizePreservingCompleted(): AttemptCollectionFinalization {
    if (this.#finalized) contractError('attempt collector can only be finalized once')
    this.#finalized = true
    const attempts: LogicalAttempt[] = []
    const violations: string[] = []
    for (const attempt of [...this.#attempts.values()]
      .sort((left, right) => compareAttemptKeys(attemptKey(left), attemptKey(right)))) {
      try {
        attempts.push(finalizeAttempt(attempt))
      } catch (cause) {
        violations.push(errorMessage(cause))
      }
    }
    if (this.#rejectedEvidence && violations.length === 0) {
      violations.push('attempt collector rejected evidence after its last valid state')
    }
    return freezeRecord({
      attempts: Object.freeze(attempts),
      integrityViolations: Object.freeze([...new Set(violations)].sort(compareAttemptKeys)),
    })
  }

}

function cloneAttemptState(
  current: AttemptState | undefined,
  evidence: AttemptEvidence,
): AttemptState {
  if (current === undefined) {
    return {
      sessionId: evidence.sessionId,
      peerPathId: evidence.peerPathId,
      attemptId: evidence.attemptId,
      streams: new Map(),
      events: [],
      lane: undefined,
    }
  }
  return {
    sessionId: current.sessionId,
    peerPathId: current.peerPathId,
    attemptId: current.attemptId,
    streams: new Map([...current.streams].map(([side, stream]) => [side, { ...stream }])),
    events: [...current.events],
    lane: current.lane,
  }
}

function cloneSessionLaneAuthority(
  current: SessionLaneAuthorityState | undefined,
): SessionLaneAuthorityState {
  return {
    laneIdOwners: new Map(current?.laneIdOwners),
    laneEpochOwners: new Map(current?.laneEpochOwners),
  }
}

function reserveObservedLane(
  authority: SessionLaneAuthorityState,
  attempt: AttemptState,
): void {
  const lane = attempt.lane
  if (lane === undefined) return

  const owner = attemptKey(attempt)
  const laneIdOwner = authority.laneIdOwners.get(lane.laneId)
  const laneEpochOwner = authority.laneEpochOwners.get(lane.laneEpoch)
  if (laneIdOwner !== undefined && laneIdOwner !== owner) {
    contractError(
      `lane ID ${lane.laneId} is reused within ProtocolSession ${attempt.sessionId}`,
    )
  }
  if (laneEpochOwner !== undefined && laneEpochOwner !== owner) {
    contractError(
      `lane epoch ${lane.laneEpoch} is reused within ProtocolSession ${attempt.sessionId}`,
    )
  }

  authority.laneIdOwners.set(lane.laneId, owner)
  authority.laneEpochOwners.set(lane.laneEpoch, owner)
}

function ensureSideStream(attempt: AttemptState, side: AttemptSide): SideStreamState {
  let stream = attempt.streams.get(side)
  if (stream === undefined) {
    stream = {
      side,
      nextSuccessIndex: 0,
      lastSequence: 0,
      lastElapsedMs: 0,
      terminal: undefined,
      candidateCounts: undefined,
      lane: undefined,
      localGeneration: undefined,
    }
    attempt.streams.set(side, stream)
  }
  return stream
}

export function reducePeerAttemptOutcome(attempts: readonly LogicalAttempt[]): PeerAttemptOutcome {
  if (attempts.length === 0) return 'not-started'
  const identities = new Set<string>()
  let admitted = false
  let failed = false
  for (const attempt of attempts) {
    const key = attemptKey(attempt)
    if (identities.has(key)) contractError(`logical attempt ${key} appears more than once`)
    identities.add(key)
    if (attempt.outcome === 'failed') failed = true
    else admitted = true
  }
  if (failed) return 'failed'
  if (!admitted) contractError('non-empty logical attempt set has no terminal outcome')
  return 'admitted'
}

export function parseLogicalAttempts(
  value: unknown,
  allowReceiveSequenceGaps = false,
): readonly LogicalAttempt[] {
  const records = requireArray(value, 'logical attempts')
  const normalized = records.map((item, index) => parseLogicalAttemptRecord(item, index))
  const events = normalized
    .flatMap((attempt) => attempt.events)
    .sort((left, right) => left.receiveSequence - right.receiveSequence)
  const collector = new AttemptCollector()
  let previousReceiveSequence = 0
  for (let index = 0; index < events.length; index += 1) {
    const received = events[index]
    if (
      received === undefined ||
      (allowReceiveSequenceGaps
        ? received.receiveSequence <= previousReceiveSequence
        : received.receiveSequence !== index + 1)
    ) {
      contractError('collector receive sequence must be contiguous from one')
    }
    previousReceiveSequence = received.receiveSequence
    const replayed = collector.ingest(received.evidence)
    if (!allowReceiveSequenceGaps && replayed.receiveSequence !== received.receiveSequence) {
      contractError('collector receive sequence does not reproduce')
    }
  }
  const replayed = collector.finalize()
  const rankByReceiveSequence = new Map(events.map((event, index) => [event.receiveSequence, index + 1]))
  const normalizedForReplay = normalized.map((attempt) => freezeRecord({
    ...attempt,
    events: Object.freeze(attempt.events.map((event) => freezeRecord({
      ...event,
      receiveSequence: rankByReceiveSequence.get(event.receiveSequence),
    }))),
  }))
  if (JSON.stringify(replayed) !== JSON.stringify(normalizedForReplay)) {
    contractError('serialized logical attempts do not match their producer evidence')
  }
  return allowReceiveSequenceGaps ? Object.freeze(normalized) : replayed
}

function validateStreamEvent(
  stream: SideStreamState,
  attempt: AttemptState,
  evidence: AttemptEvidence,
): void {
  if (stream.terminal !== undefined) {
    const kind = evidence.stage === 'admitted' || evidence.stage === 'failed'
      ? 'duplicate terminal'
      : 'post-terminal event'
    contractError(`${kind} for ${attemptKey(attempt)}/${stream.side}`)
  }
  if (evidence.sideSequence !== stream.lastSequence + 1) {
    // Contiguity turns output truncation into a deterministic contract failure
    // instead of silently accepting a partial lifecycle as complete evidence.
    contractError(`side sequence is not contiguous for ${attemptKey(attempt)}/${stream.side}`)
  }
  if (stream.lastSequence === 0 && evidence.stage !== 'started') {
    contractError(`side stream does not begin with started for ${attemptKey(attempt)}/${stream.side}`)
  }
  if (evidence.attemptElapsedMs < stream.lastElapsedMs) {
    contractError(`attempt elapsed time regressed for ${attemptKey(attempt)}/${stream.side}`)
  }
  const expectedStage = successChain(stream.side)[stream.nextSuccessIndex]
  if (evidence.stage === 'failed') {
    if (evidence.failedAtStage !== expectedStage) {
      contractError(`failed-at stage does not name the next milestone for ${attemptKey(attempt)}/${stream.side}`)
    }
    stream.terminal = 'failed'
  } else {
    if (evidence.stage !== expectedStage) {
      contractError(`attempt stage is out of order for ${attemptKey(attempt)}/${stream.side}`)
    }
    stream.nextSuccessIndex += 1
    if (evidence.stage === 'admitted') stream.terminal = 'admitted'
  }
  validateCandidateCounts(stream, evidence, attempt)
  validateSelectedPair(stream, evidence, attempt)
  validateLane(stream, attempt, evidence)
  validateLocalGeneration(stream, evidence, attempt)
  stream.lastSequence = evidence.sideSequence
  stream.lastElapsedMs = evidence.attemptElapsedMs
}

function validateCandidateCounts(
  stream: SideStreamState,
  evidence: AttemptEvidence,
  attempt: AttemptState,
): void {
  const counts = Object.hasOwn(evidence, 'candidateCounts')
    ? (evidence as AttemptEvidence & { readonly candidateCounts: CandidateCounts }).candidateCounts
    : undefined
  if (stream.candidateCounts !== undefined && counts === undefined) {
    contractError(`candidate counts disappeared for ${attemptKey(attempt)}/${stream.side}`)
  }
  if (
    counts !== undefined && stream.candidateCounts !== undefined &&
    (counts.localEmitted < stream.candidateCounts.localEmitted ||
      counts.remoteAccepted < stream.candidateCounts.remoteAccepted)
  ) {
    contractError(`cumulative candidate counts regressed for ${attemptKey(attempt)}/${stream.side}`)
  }
  if (counts !== undefined) stream.candidateCounts = counts
}

function validateLane(
  stream: SideStreamState,
  attempt: AttemptState,
  evidence: AttemptEvidence,
): void {
  const lane = Object.hasOwn(evidence, 'lane')
    ? (evidence as AttemptEvidence & { readonly lane: LaneIdentity }).lane
    : undefined
  if (stream.lane !== undefined && lane === undefined) {
    contractError(`known lane identity disappeared for ${attemptKey(attempt)}/${stream.side}`)
  }
  if (lane === undefined) return
  if (evidence.stage === 'failed' && stream.lane === undefined) {
    contractError(`failed evidence invents a lane before its milestone for ${attemptKey(attempt)}/${stream.side}`)
  }
  if (stream.lane !== undefined && !sameLane(stream.lane, lane)) {
    contractError(`lane identity changed within ${attemptKey(attempt)}/${stream.side}`)
  }
  if (attempt.lane !== undefined && !sameLane(attempt.lane, lane)) {
    contractError(`browser and sender lane identities differ for ${attemptKey(attempt)}`)
  }
  stream.lane = lane
  attempt.lane = lane
}

function validateSelectedPair(
  stream: SideStreamState,
  evidence: AttemptEvidence,
  attempt: AttemptState,
): void {
  if (!Object.hasOwn(evidence, 'selectedPair')) return
  if (evidence.stage === 'failed' && stream.lane === undefined) {
    contractError(`failed evidence invents selected-pair proof before lane admission for ${attemptKey(attempt)}/${stream.side}`)
  }
}

function validateLocalGeneration(
  stream: SideStreamState,
  evidence: AttemptEvidence,
  attempt: AttemptState,
): void {
  if (evidence.side !== 'sender') return
  const generation = evidence.localGeneration
  if (stream.localGeneration !== undefined && generation === undefined) {
    contractError(`known local generation disappeared for ${attemptKey(attempt)}/sender`)
  }
  if (generation !== undefined && stream.localGeneration !== undefined && generation !== stream.localGeneration) {
    contractError(`local generation changed for ${attemptKey(attempt)}/sender`)
  }
  if (generation !== undefined) stream.localGeneration = generation
}

function finalizeAttempt(attempt: AttemptState): LogicalAttempt {
  const browser = attempt.streams.get('browser')
  if (browser === undefined) {
    contractError(`logical attempt ${attemptKey(attempt)} has no browser authority stream`)
  }
  for (const stream of attempt.streams.values()) {
    if (stream.terminal === undefined) {
      contractError(`side stream ${attemptKey(attempt)}/${stream.side} has no terminal`)
    }
  }
  const failed = [...attempt.streams.values()].some((stream) => stream.terminal === 'failed')
  const answerReceivedIndex = BROWSER_SUCCESS_CHAIN.indexOf('answer-received')
  const authenticatedBrowserFailure = attempt.events.find(({ evidence }) =>
    evidence.side === 'browser' && evidence.stage === 'failed' &&
    Object.hasOwn(evidence, 'authenticatedSenderOperationFailure'))?.evidence
  const authenticatedSenderFailure = authenticatedBrowserFailure !== undefined
  const sender = attempt.streams.get('sender')
  if (
    failed && (browser.nextSuccessIndex > answerReceivedIndex || authenticatedSenderFailure) &&
    sender === undefined
  ) {
    contractError(`failed attempt ${attemptKey(attempt)} observed sender participation but has no sender stream`)
  }
  if (authenticatedSenderFailure && (sender === undefined || !stageCompleted(sender, 'offer-received'))) {
    contractError(`authenticated sender failure ${attemptKey(attempt)} lacks sender offer reception`)
  }
  validateAuthenticatedSenderFailure(attempt, sender, authenticatedBrowserFailure)
  validateCrossSideReachability(attempt, browser)
  if (!failed) {
    if (browser.terminal !== 'admitted' || sender?.terminal !== 'admitted') {
      contractError(`admitted attempt ${attemptKey(attempt)} lacks both admitted side streams`)
    }
    if (attempt.lane === undefined) {
      contractError(`admitted attempt ${attemptKey(attempt)} has no authoritative lane identity`)
    }
  }
  return freezeRecord({
    sessionId: attempt.sessionId,
    peerPathId: attempt.peerPathId,
    attemptId: attempt.attemptId,
    outcome: failed ? 'failed' as const : 'admitted' as const,
    events: Object.freeze([...attempt.events]),
  })
}

function validateAuthenticatedSenderFailure(
  attempt: AttemptState,
  sender: SideStreamState | undefined,
  browserEvidence: AttemptEvidence | undefined,
): void {
  if (browserEvidence === undefined) return
  if (browserEvidence.side !== 'browser' || browserEvidence.stage !== 'failed') {
    contractError(`authenticated sender failure ${attemptKey(attempt)} has invalid browser authority`)
  }
  const operation = browserEvidence.authenticatedSenderOperationFailure
  const senderEvidence = attempt.events.find(({ evidence }) =>
    evidence.side === 'sender' && evidence.stage === 'failed')?.evidence
  if (
    operation === undefined || sender?.terminal !== 'failed' ||
    senderEvidence?.side !== 'sender' || senderEvidence.stage !== 'failed'
  ) {
    contractError(`authenticated sender failure ${attemptKey(attempt)} requires a failed sender terminal`)
  }
  if (
    senderEvidence.typedErrorCode !== browserEvidence.typedErrorCode ||
    senderEvidence.failureScope !== browserEvidence.failureScope ||
    senderEvidence.failureMessage !== operation.message
  ) {
    contractError(`authenticated sender failure ${attemptKey(attempt)} differs across producer streams`)
  }
}

function validateCrossSideReachability(attempt: AttemptState, browser: SideStreamState): void {
  const sender = attempt.streams.get('sender')
  if (sender === undefined) return
  if (!stageCompleted(browser, 'offer-sent')) {
    contractError(`sender stream ${attemptKey(attempt)} exists before browser offer dispatch`)
  }
  const dependencies: readonly [AttemptStage, AttemptStage][] = [
    ['answer-received', 'answer-sent'],
    ['datachannel-open', 'datachannel-open'],
    ['lane-granted', 'lane-admission-started'],
  ]
  for (const [browserStage, senderStage] of dependencies) {
    if (stageCompleted(browser, browserStage) && !stageCompleted(sender, senderStage)) {
      contractError(
        `browser ${browserStage} in ${attemptKey(attempt)} lacks sender ${senderStage}`,
      )
    }
  }
  if (stageCompleted(browser, 'lane-attached') && sender.terminal !== 'admitted') {
    contractError(`browser lane attachment in ${attemptKey(attempt)} lacks sender admission`)
  }
  if (stageCompleted(sender, 'datachannel-open') && !stageCompleted(browser, 'answer-received')) {
    contractError(`sender datachannel in ${attemptKey(attempt)} precedes browser answer receipt`)
  }
  if (
    (stageCompleted(sender, 'lane-admission-started') || sender.terminal === 'admitted') &&
    !stageCompleted(browser, 'datachannel-open')
  ) {
    contractError(`sender lane admission in ${attemptKey(attempt)} lacks browser datachannel`)
  }
}

function stageCompleted(stream: SideStreamState, stage: AttemptStage): boolean {
  const chain = successChain(stream.side)
  const stageIndex = chain.indexOf(stage)
  return stageIndex >= 0 && stream.nextSuccessIndex > stageIndex
}

function parseLogicalAttemptRecord(value: unknown, index: number): LogicalAttempt {
  const record = requireRecord(value, `logical attempt ${index}`)
  requireExactKeys(
    record,
    ['sessionId', 'peerPathId', 'attemptId', 'outcome', 'events'],
    [],
    `logical attempt ${index}`,
  )
  const sessionId = requireCanonicalIdentity(record.sessionId, `logical attempt ${index} session ID`)
  const peerPathId = requireCanonicalIdentity(record.peerPathId, `logical attempt ${index} path ID`)
  const attemptId = requireCanonicalIdentity(record.attemptId, `logical attempt ${index} attempt ID`)
  const events = requireArray(record.events, `logical attempt ${index} events`).map((event, eventIndex) => {
    const wrapper = requireRecord(event, `logical attempt ${index} event ${eventIndex}`)
    requireExactKeys(
      wrapper,
      ['receiveSequence', 'evidence'],
      [],
      `logical attempt ${index} event ${eventIndex}`,
    )
    const evidence = parseAttemptEvidence(wrapper.evidence)
    if (
      evidence.sessionId !== sessionId || evidence.peerPathId !== peerPathId ||
      evidence.attemptId !== attemptId
    ) {
      contractError(`logical attempt ${index} contains evidence for another identity`)
    }
    return freezeRecord({
      receiveSequence: requireSafeInteger(
        wrapper.receiveSequence,
        1,
        Number.MAX_SAFE_INTEGER,
        `logical attempt ${index} receive sequence`,
      ),
      evidence,
    })
  })
  return freezeRecord({
    sessionId,
    peerPathId,
    attemptId,
    outcome: requireEnum(
      record.outcome,
      PEER_ATTEMPT_OUTCOMES.filter((outcome) => outcome !== 'not-started'),
      `logical attempt ${index} outcome`,
    ),
    events: Object.freeze(events),
  })
}

function successChain(side: AttemptSide): readonly AttemptStage[] {
  return side === 'browser' ? BROWSER_SUCCESS_CHAIN : SENDER_SUCCESS_CHAIN
}

function attemptKey(identity: {
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
}): string {
  return `${identity.sessionId}/${identity.peerPathId}/${identity.attemptId}`
}

function sameLane(left: LaneIdentity, right: LaneIdentity): boolean {
  return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch
}

function compareAttemptKeys(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause)
}
