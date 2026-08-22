import {
  chainDirectZipEpochDigestV1,
  concatDirectZipBytes,
  deriveDirectZipOwnershipHeaderReadBytes,
  digestDirectZipArchiveBytes,
  directZipEpochGenesisRoot,
  encodeDirectZipBootstrapPrefixV1,
  encodeDirectZipCentralDirectoryRecordV2,
  planDirectZipEntryV2,
  snapshotDirectZipOwnershipMarkerV1,
  type DirectZipOwnershipMarkerV1,
} from '../../../../src/output/direct-zip/format'
import {
  DIRECT_ZIP_WRITER_CHECKPOINT_VERSION,
  DirectZipEpochWriterV1,
  type DirectZipBoundedCompletionReadV1,
  type DirectZipCandidateObservationV1,
  type DirectZipCloseAttemptV1,
  type DirectZipCompletionProofV1,
  type DirectZipEpochCandidateV1,
  type DirectZipEpochProofV1,
  type DirectZipMemberAdmissionV1,
  type DirectZipOpenEpochResultV1,
  type DirectZipPositionedEpochWritable,
  type DirectZipPredecessorVerificationV1,
  type DirectZipSourceAuthorityV1,
  type DirectZipTargetVerificationPort,
  type DirectZipTruncateResultV1,
  type DirectZipWriterCheckpointV1,
  type DirectZipWriterCutSink,
  type DirectZipWriterIdentityPort,
  type DirectZipWriterObserver,
  type DirectZipWriterPageSink,
  type DirectZipWriterPageStateV1,
  type DirectZipAutomaticEpochBudgetV1,
} from '../../../../src/output/direct-zip/writer'

const ROOT_COMPONENT = 'root'
const ROOT_LAYOUT_EVIDENCE = new TextEncoder().encode('root-layout')
const ROOT_DISCOVERY_EVIDENCE = new TextEncoder().encode('root-discovery')

export type CloseFault = 'before-publish' | 'after-publish'

export class StagedDirectZipTarget implements DirectZipTargetVerificationPort {
  readonly artifactCount = 1
  visible: Uint8Array
  predecessorVerification: DirectZipPredecessorVerificationV1 = { kind: 'accepted-fast' }
  closeFaults: CloseFault[] = []
  observationOverrides: DirectZipCandidateObservationV1[] = []
  failNextWriteAtOrAfter: bigint | undefined
  failPromotionRead = false
  corruptClosingTail = false
  openEpochCount = 0
  closeAttemptCount = 0
  abortCount = 0
  truncateCount = 0
  boundedCompletionReadCount = 0
  readonly rangeReads: Array<Readonly<{ start: bigint; end: bigint }>> = []

  constructor(initial: Uint8Array) {
    this.visible = Uint8Array.from(initial)
  }

  verifyPredecessor(): Promise<DirectZipPredecessorVerificationV1> {
    return Promise.resolve(this.predecessorVerification)
  }

  openEpoch(): Promise<DirectZipOpenEpochResultV1> {
    this.openEpochCount += 1
    const staged = Uint8Array.from(this.visible)
    return Promise.resolve({ kind: 'opened', writable: this.#writable(staged) })
  }

  observeCandidate(
    candidate: DirectZipEpochCandidateV1,
    closeAttempt?: DirectZipCloseAttemptV1,
  ): Promise<DirectZipCandidateObservationV1> {
    const override = this.observationOverrides.shift()
    if (override !== undefined) return Promise.resolve(override)
    const visibleLength = BigInt(this.visible.byteLength)
    const base = {
      permission: 'granted' as const,
      presence: 'present' as const,
      ownership: 'matching' as const,
      candidateIntegrity: 'not-read' as const,
      predecessorIntegrity: 'not-read' as const,
      observationDigest: observationDigest(this.visible),
    }
    if (visibleLength === candidate.predecessorLength) {
      return Promise.resolve({
        ...base,
        length: 'predecessor',
        observationMatch: 'predecessor',
      })
    }
    if (visibleLength === candidate.stagedEnd) {
      return Promise.resolve({
        ...base,
        length: 'candidate',
        observationMatch: 'candidate',
        candidateIntegrity: closeAttempt?.kind === 'closed'
          ? 'writer-bounded-proof'
          : 'not-read',
      })
    }
    if (visibleLength > candidate.predecessorLength) {
      return Promise.resolve({
        ...base,
        length: 'unknown-tail',
        observationMatch: 'neither',
      })
    }
    return Promise.resolve({ ...base, length: 'other', observationMatch: 'neither' })
  }

  digestRange(start: bigint, end: bigint): Promise<Uint8Array> {
    this.rangeReads.push(Object.freeze({ start, end }))
    if (this.failPromotionRead) return Promise.resolve(fixedBytes(0xee))
    return Promise.resolve(digestDirectZipArchiveBytes(
      this.visible.subarray(Number(start), Number(end)),
    ))
  }

  truncateToPredecessor(
    checkpoint: DirectZipWriterCheckpointV1,
  ): Promise<DirectZipTruncateResultV1> {
    this.truncateCount += 1
    this.visible = this.visible.slice(0, Number(checkpoint.committedLength))
    return Promise.resolve({
      kind: 'truncated',
      observationDigest: observationDigest(this.visible),
    })
  }

  readBoundedCompletionProof(input: Readonly<{
    exactArchiveBytes: bigint
    rootCentralRecordOffset: bigint
    rootCentralRecordBytes: bigint
    closingTailBytes: number
  }>): Promise<DirectZipBoundedCompletionReadV1> {
    this.boundedCompletionReadCount += 1
    const fixed = this.visible.subarray(0, 30)
    const localBytes = deriveDirectZipOwnershipHeaderReadBytes(fixed)
    const tail = this.visible.slice(this.visible.byteLength - input.closingTailBytes)
    if (this.corruptClosingTail) {
      tail[tail.byteLength - 1] = tail[tail.byteLength - 1]! ^ 0xff
    }
    return Promise.resolve({
      localOwnershipHeader: this.visible.slice(0, localBytes),
      rootCentralRecord: this.visible.slice(
        Number(input.rootCentralRecordOffset),
        Number(input.rootCentralRecordOffset + input.rootCentralRecordBytes),
      ),
      closingTail: tail,
      observationDigest: observationDigest(this.visible),
    })
  }

  appendExternal(bytes: Uint8Array): void {
    this.visible = concatDirectZipBytes([this.visible, bytes])
  }

  #writable(initial: Uint8Array): DirectZipPositionedEpochWritable {
    let staged = initial
    let settled = false
    return {
      write: async (position, bytes) => {
        if (settled) throw new Error('test target epoch is settled')
        if (this.failNextWriteAtOrAfter !== undefined && position >= this.failNextWriteAtOrAfter) {
          this.failNextWriteAtOrAfter = undefined
          throw new Error('injected positioned write failure')
        }
        const end = Number(position) + bytes.byteLength
        if (end > staged.byteLength) {
          const grown = new Uint8Array(end)
          grown.set(staged)
          staged = grown
        }
        staged.set(bytes, Number(position))
      },
      closeOnce: async () => {
        if (settled) throw new Error('test target close was retried')
        settled = true
        this.closeAttemptCount += 1
        const fault = this.closeFaults.shift()
        if (fault === 'before-publish') {
          return { kind: 'threw', error: new Error('injected close-before-publish failure') }
        }
        this.visible = Uint8Array.from(staged)
        if (fault === 'after-publish') {
          return { kind: 'threw', error: new Error('injected close-after-publish failure') }
        }
        return { kind: 'closed' }
      },
      abort: async () => {
        if (!settled) settled = true
        this.abortCount += 1
      },
    }
  }
}

export class MemoryDirectZipPages implements DirectZipWriterPageSink {
  readonly #layouts: DirectZipMemberAdmissionV1[] = []
  readonly #central: Array<Readonly<{ ordinal: bigint; bytes: Uint8Array }>> = []
  readonly #epochProofs: DirectZipEpochProofV1[] = []

  constructor(rootCentralRecord: Uint8Array, bootstrapProof: DirectZipEpochProofV1) {
    const rootPlan = planDirectZipEntryV2({
      ordinal: 0n,
      localHeaderOffset: 0n,
      entry: { kind: 'directory', path: [ROOT_COMPONENT] },
      ownershipMarker: markerFixture(),
    })
    this.#layouts.push({
      plan: rootPlan,
      layoutEvidence: ROOT_LAYOUT_EVIDENCE,
      discoveryEvidence: ROOT_DISCOVERY_EVIDENCE,
    })
    this.#central.push({ ordinal: 0n, bytes: Uint8Array.from(rootCentralRecord) })
    this.#epochProofs.push(bootstrapProof)
  }

  stageLayout(admission: DirectZipMemberAdmissionV1): Promise<void> {
    this.#layouts.push(snapshotAdmission(admission))
    return Promise.resolve()
  }

  stageCentral(input: Readonly<{ ordinal: bigint; bytes: Uint8Array }>): Promise<void> {
    this.#central.push(Object.freeze({ ordinal: input.ordinal, bytes: Uint8Array.from(input.bytes) }))
    return Promise.resolve()
  }

  snapshot(): Promise<DirectZipWriterPageStateV1> {
    return Promise.resolve(this.currentState())
  }

  currentState(): DirectZipWriterPageStateV1 {
    const layoutChunks = this.#layouts.flatMap((entry) => [
      entry.layoutEvidence,
      entry.discoveryEvidence,
    ])
    const centralChunks = this.#central.map((entry) => entry.bytes)
    return Object.freeze({
      layoutRoot: digestDirectZipArchiveBytes(concatDirectZipBytes(layoutChunks)),
      layoutRecordCount: BigInt(this.#layouts.length),
      centralRoot: digestDirectZipArchiveBytes(concatDirectZipBytes(centralChunks)),
      centralRecordCount: BigInt(this.#central.length),
      centralBytes: BigInt(centralChunks.reduce((total, bytes) => total + bytes.byteLength, 0)),
    })
  }

  restore(state: DirectZipWriterPageStateV1): Promise<void> {
    this.#layouts.splice(Number(state.layoutRecordCount))
    this.#central.splice(Number(state.centralRecordCount))
    return Promise.resolve()
  }

  async *replayCentral(): AsyncIterable<Readonly<{ ordinal: bigint; bytes: Uint8Array }>> {
    for (const record of this.#central) {
      yield Object.freeze({ ordinal: record.ordinal, bytes: Uint8Array.from(record.bytes) })
    }
  }

  async *committedEpochProofs(): AsyncIterable<DirectZipEpochProofV1> {
    for (const proof of this.#epochProofs) yield proof
  }

  commitCandidateProof(candidate: DirectZipEpochCandidateV1): void {
    const predecessorRoot = this.#epochProofs.at(-1)?.epochRoot ?? directZipEpochGenesisRoot()
    this.#epochProofs.push(Object.freeze({
      start: candidate.rangeStart,
      end: candidate.stagedEnd,
      contentDigest: Uint8Array.from(candidate.contentDigest),
      predecessorRoot: Uint8Array.from(predecessorRoot),
      epochRoot: Uint8Array.from(candidate.expectedEpochRoot),
    }))
  }
}

export class MemoryDirectZipCuts implements DirectZipWriterCutSink {
  readonly staged: DirectZipEpochCandidateV1[] = []
  readonly promoted: Array<Readonly<{
    candidate: DirectZipEpochCandidateV1
    checkpoint: DirectZipWriterCheckpointV1
    completion?: DirectZipCompletionProofV1
  }>> = []
  readonly retired: DirectZipEpochCandidateV1[] = []
  readonly closing: DirectZipWriterCheckpointV1[] = []
  failPromotionCount = 0
  readonly #pages: MemoryDirectZipPages

  constructor(pages: MemoryDirectZipPages) {
    this.#pages = pages
  }

  stageCandidate(candidate: DirectZipEpochCandidateV1): Promise<void> {
    this.staged.push(candidate)
    return Promise.resolve()
  }

  promoteCandidate(input: Readonly<{
    candidate: DirectZipEpochCandidateV1
    checkpoint: DirectZipWriterCheckpointV1
    completion?: DirectZipCompletionProofV1
  }>): Promise<void> {
    if (this.failPromotionCount > 0) {
      this.failPromotionCount -= 1
      return Promise.reject(new Error('injected journal promotion failure'))
    }
    this.promoted.push(input)
    this.#pages.commitCandidateProof(input.candidate)
    return Promise.resolve()
  }

  retireCandidate(input: Readonly<{
    candidate: DirectZipEpochCandidateV1
    checkpoint: DirectZipWriterCheckpointV1
  }>): Promise<void> {
    this.retired.push(input.candidate)
    return Promise.resolve()
  }

  enterClosing(input: Readonly<{ checkpoint: DirectZipWriterCheckpointV1 }>): Promise<void> {
    this.closing.push(input.checkpoint)
    return Promise.resolve()
  }
}

export interface DirectZipWriterHarness {
  readonly marker: DirectZipOwnershipMarkerV1
  readonly target: StagedDirectZipTarget
  readonly pages: MemoryDirectZipPages
  readonly cuts: MemoryDirectZipCuts
  readonly checkpoint: DirectZipWriterCheckpointV1
  writer(
    checkpoint?: DirectZipWriterCheckpointV1,
    budget?: DirectZipAutomaticEpochBudgetV1,
    observe?: DirectZipWriterObserver,
  ): DirectZipEpochWriterV1
}

export function createWriterHarness(): DirectZipWriterHarness {
  const marker = markerFixture()
  const prefix = encodeDirectZipBootstrapPrefixV1(ROOT_COMPONENT, marker)
  const rootPlan = planDirectZipEntryV2({
    ordinal: 0n,
    localHeaderOffset: 0n,
    entry: { kind: 'directory', path: [ROOT_COMPONENT] },
    ownershipMarker: marker,
  })
  const rootCentral = encodeDirectZipCentralDirectoryRecordV2(rootPlan, 0)
  const contentDigest = digestDirectZipArchiveBytes(prefix)
  const epochRoot = chainDirectZipEpochDigestV1({
    predecessorRoot: directZipEpochGenesisRoot(),
    start: 0n,
    end: BigInt(prefix.byteLength),
    contentDigest,
  })
  const pages = new MemoryDirectZipPages(rootCentral, Object.freeze({
    start: 0n,
    end: BigInt(prefix.byteLength),
    contentDigest,
    predecessorRoot: directZipEpochGenesisRoot(),
    epochRoot,
  }))
  const pageState = pages.currentState()
  const checkpoint: DirectZipWriterCheckpointV1 = Object.freeze({
    version: DIRECT_ZIP_WRITER_CHECKPOINT_VERSION,
    operationId: 'operation-1',
    intentDigest: fixedBytes(0x11),
    generation: 1n,
    phase: 'between-members',
    nextEntryOrdinal: 1n,
    archiveOffset: BigInt(prefix.byteLength),
    committedLength: BigInt(prefix.byteLength),
    safeResumeBytes: 0n,
    targetObservationDigest: observationDigest(prefix),
    epochRoot,
    pages: pageState,
  })
  const target = new StagedDirectZipTarget(prefix)
  const cuts = new MemoryDirectZipCuts(pages)
  let epochId = 0
  let candidateId = 0
  const identities: DirectZipWriterIdentityPort = {
    nextEpochId: () => `epoch-${++epochId}`,
    nextCandidateId: () => `candidate-${++candidateId}`,
  }
  return {
    marker,
    target,
    pages,
    cuts,
    checkpoint,
    writer: (restored = checkpoint, budget, observe) => new DirectZipEpochWriterV1({
      context: { ownershipMarker: marker, rootComponent: ROOT_COMPONENT },
      checkpoint: restored,
      pages,
      cuts,
      target,
      identities,
      ...(budget === undefined ? {} : { automaticBudget: budget }),
      ...(observe === undefined ? {} : { observe }),
    }),
  }
}

export function fileSource(
  revision = 'revision-1',
  exactSize = 6n,
): DirectZipSourceAuthorityV1 {
  return Object.freeze({
    fileId: 'file-1',
    revision,
    exactSize,
    rangeAuthority: 'range-authority-1',
  })
}

export function fileAdmission(
  checkpoint: DirectZipWriterCheckpointV1,
  source = fileSource(),
): DirectZipMemberAdmissionV1 {
  return Object.freeze({
    plan: planDirectZipEntryV2({
      ordinal: checkpoint.nextEntryOrdinal,
      localHeaderOffset: checkpoint.archiveOffset,
      entry: {
        kind: 'file',
        path: [ROOT_COMPONENT, 'a.txt'],
        exactSize: source.exactSize,
      },
    }),
    layoutEvidence: new TextEncoder().encode('file-layout'),
    discoveryEvidence: new TextEncoder().encode('file-discovery'),
    source,
  })
}

export function observationDigest(bytes: Uint8Array): Uint8Array {
  const output = new Uint8Array(32)
  new DataView(output.buffer).setBigUint64(0, BigInt(bytes.byteLength), false)
  return output
}

export function candidateObservation(
  overrides: Partial<DirectZipCandidateObservationV1> = {},
): DirectZipCandidateObservationV1 {
  return Object.freeze({
    permission: 'granted',
    presence: 'present',
    ownership: 'matching',
    length: 'candidate',
    observationMatch: 'candidate',
    candidateIntegrity: 'not-read',
    predecessorIntegrity: 'not-read',
    observationDigest: fixedBytes(0x33),
    ...overrides,
  })
}

function markerFixture(): DirectZipOwnershipMarkerV1 {
  return snapshotDirectZipOwnershipMarkerV1({
    operationId: fixedBytes(0x01, 16),
    candidateId: fixedBytes(0x02, 16),
    ownershipNonce: fixedBytes(0x03),
    bindingDigest: fixedBytes(0x04),
  })
}

function fixedBytes(value: number, length = 32): Uint8Array {
  return new Uint8Array(length).fill(value)
}

function snapshotAdmission(admission: DirectZipMemberAdmissionV1): DirectZipMemberAdmissionV1 {
  return Object.freeze({
    ...admission,
    layoutEvidence: Uint8Array.from(admission.layoutEvidence),
    discoveryEvidence: Uint8Array.from(admission.discoveryEvidence),
  })
}
