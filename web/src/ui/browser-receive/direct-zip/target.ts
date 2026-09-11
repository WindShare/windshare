import { decodeBase64Url, encodeBase64Url, equalBytes } from '../../../crypto/bytes'
import {
  DirectZipSha256Accumulator,
  chainDirectZipEpochDigestV1,
  deriveDirectZipOwnershipHeaderReadBytes,
  digestDirectZipArchiveBytes,
  directZipEpochGenesisRoot,
  encodeDirectZipBootstrapPrefixV1,
} from '../../../output/direct-zip/format'
import {
  createDirectZipTargetObservationV1,
  type DirectZipTargetObservationV1,
} from '../../../output/direct-zip/journal'
import {
  DIRECT_ZIP_RANGE_HASH_CHUNK_BYTES,
  observeDirectZipTarget,
  type DirectZipFileSnapshotPort,
  type DirectZipFileSystemPort,
  type DirectZipOwnedTargetBinding,
  type DirectZipWritablePort,
} from '../../../output/direct-zip/target'
import type {
  DirectZipCandidateObservationV1,
  DirectZipCloseAttemptV1,
  DirectZipEpochCandidateV1,
  DirectZipEpochProofV1,
  DirectZipOpenEpochResultV1,
  DirectZipPredecessorVerificationV1,
  DirectZipTargetVerificationPort,
  DirectZipTruncateResultV1,
  DirectZipWriterCheckpointV1,
} from '../../../output/direct-zip/writer'

export type BrowserDirectZipBinding =
  DirectZipOwnedTargetBinding<FileSystemDirectoryHandle, FileSystemFileHandle>

export interface DirectZipNamespaceMutationPort {
  run<T>(operation: () => Promise<T>): Promise<T>
}

export interface BrowserDirectZipTargetOptions {
  readonly binding: BrowserDirectZipBinding
  readonly fileSystem: DirectZipFileSystemPort<FileSystemDirectoryHandle, FileSystemFileHandle>
  readonly proofs: () => AsyncIterable<DirectZipEpochProofV1>
  readonly namespaceMutations: DirectZipNamespaceMutationPort
}

/**
 * Persisted handles locate the selected entry; only archive bytes establish ownership.
 * A restored session verifies every committed epoch before it accepts any new writes.
 */
export class BrowserDirectZipTarget implements DirectZipTargetVerificationPort {
  readonly #input: BrowserDirectZipTargetOptions
  #verified: Readonly<{ generation: bigint; observation: string }> | undefined
  #writable: DirectZipWritablePort | undefined
  #candidateRoot: Uint8Array | undefined
  #closedEpoch: Readonly<{
    generation: bigint; start: bigint; end: bigint; contentDigest: Uint8Array
  }> | undefined

  constructor(input: BrowserDirectZipTargetOptions) {
    this.#input = input
  }

  async observe(epochRoot: Uint8Array): Promise<DirectZipTargetObservationV1> {
    return this.#observation(await this.#ownedSnapshot(), epochRoot)
  }

  async verifyCheckpoint(checkpoint: DirectZipWriterCheckpointV1): Promise<void> {
    const snapshot = await this.#ownedSnapshot()
    if (snapshot.size !== checkpoint.committedLength ||
        !await this.#verifyProofs(snapshot, checkpoint.committedLength, checkpoint.epochRoot)) {
      throw new DOMException('The saved ZIP no longer matches its committed bytes', 'InvalidStateError')
    }
    const observation = await this.#observation(snapshot, checkpoint.epochRoot)
    this.#verified = { generation: checkpoint.generation, observation: observation.digest }
  }

  async verifyPredecessor(checkpoint: DirectZipWriterCheckpointV1):
    Promise<DirectZipPredecessorVerificationV1> {
    try {
      const snapshot = await this.#ownedSnapshot()
      if (snapshot.size !== checkpoint.committedLength) return { kind: 'target-verification-required' }
      const observation = await this.#observation(snapshot, checkpoint.epochRoot)
      // A journal-only transition (such as entering closing) changes generation without changing verified bytes.
      if (this.#verified?.observation === observation.digest) return { kind: 'accepted-fast' }
      if (!await this.#verifyProofs(snapshot, checkpoint.committedLength, checkpoint.epochRoot)) {
        return { kind: 'foreign-target' }
      }
      this.#verified = { generation: checkpoint.generation, observation: observation.digest }
      return { kind: 'accepted-fast' }
    } catch (error) {
      return { kind: targetFailure(error) }
    }
  }

  async openEpoch(checkpoint: DirectZipWriterCheckpointV1): Promise<DirectZipOpenEpochResultV1> {
    const decision = await this.verifyPredecessor(checkpoint)
    if (decision.kind !== 'accepted-fast') {
      return { kind: decision.kind === 'foreign-target' || decision.kind === 'digest-readback-required'
        ? 'target-verification-required' : decision.kind }
    }
    if (this.#writable !== undefined) throw new DOMException('ZIP writer is already open', 'InvalidStateError')
    try {
      const writable = await this.#input.fileSystem.createWritable(
        this.#input.binding.fileBinding.persistedHandle, true,
      )
      this.#writable = writable
      let closed = false
      let end = checkpoint.committedLength
      const digest = new DirectZipSha256Accumulator()
      this.#closedEpoch = undefined
      return {
        kind: 'opened',
        writable: {
          write: async (position, bytes) => {
            if (position !== end) throw new RangeError('ZIP epoch must append contiguous archive bytes')
            await writable.write(position, bytes)
            digest.update(bytes)
            end += BigInt(bytes.byteLength)
          },
          closeOnce: async () => {
            if (closed) throw new DOMException('ZIP epoch was already closed', 'InvalidStateError')
            closed = true
            try {
              await writable.close()
              this.#closedEpoch = { generation: checkpoint.generation,
                start: checkpoint.committedLength, end, contentDigest: digest.digest() }
              return { kind: 'closed' }
            } catch (error) {
              return { kind: 'threw', error }
            } finally {
              this.#writable = undefined
              this.#verified = undefined
            }
          },
          abort: async reason => {
            if (closed) return
            closed = true
            try { await writable.abort(reason) } finally { this.#writable = undefined }
          },
        },
      }
    } catch (error) {
      if (error instanceof DOMException && error.name === 'QuotaExceededError') {
        return { kind: 'destination-space-required' }
      }
      const kind = targetFailure(error)
      return { kind: kind === 'foreign-target' ? 'target-verification-required' : kind }
    }
  }

  async observeCandidate(candidate: DirectZipEpochCandidateV1, closeAttempt?: DirectZipCloseAttemptV1): Promise<DirectZipCandidateObservationV1> {
    try {
      const snapshot = await this.#ownedSnapshot()
      const live = this.#closedEpoch
      const bounded = closeAttempt?.kind === 'closed' && live !== undefined &&
        live.generation === candidate.predecessorGeneration &&
        live.start === candidate.rangeStart && live.end === candidate.stagedEnd &&
        equalBytes(live.contentDigest, candidate.contentDigest)
      const predecessor = bounded || await this.#verifyProofs(
        snapshot, candidate.predecessorLength, await this.#predecessorRoot(),
      )
      const isCandidate = snapshot.size === candidate.stagedEnd
      const candidateValid = isCandidate && predecessor && (bounded ||
        equalBytes(await digestRange(snapshot, candidate.rangeStart, candidate.stagedEnd), candidate.contentDigest))
      const isPredecessor = snapshot.size === candidate.predecessorLength && predecessor
      const observation = await this.#observation(snapshot,
        candidateValid ? candidate.expectedEpochRoot : await this.#predecessorRoot())
      this.#candidateRoot = candidateValid ? Uint8Array.from(candidate.expectedEpochRoot) : undefined
      if (candidateValid) this.#verified = {
        generation: candidate.proposed.generation, observation: observation.digest,
      }
      return candidateObservation({
        isCandidate, isPredecessor, candidateValid, bounded, predecessor,
        hasTail: snapshot.size > candidate.predecessorLength, digest: observation.digest,
      })
    } catch (error) {
      const failure = targetFailure(error)
      return {
        permission: failure === 'authorization-required' ? 'unavailable' : 'granted',
        presence: failure === 'target-deleted' ? 'deleted' : 'present',
        ownership: failure === 'foreign-target' ? 'foreign' : 'ambiguous',
        length: 'other', observationMatch: 'neither',
        candidateIntegrity: 'not-read', predecessorIntegrity: 'not-read',
      }
    }
  }

  async digestRange(start: bigint, end: bigint): Promise<Uint8Array> {
    return digestRange(await this.#ownedSnapshot(), start, end)
  }

  async truncateToPredecessor(checkpoint: DirectZipWriterCheckpointV1): Promise<DirectZipTruncateResultV1> {
    try {
      const snapshot = await this.#ownedSnapshot()
      if (!await this.#verifyProofs(snapshot, checkpoint.committedLength, checkpoint.epochRoot)) {
        return { kind: 'refused' }
      }
      const writable = await this.#input.fileSystem.createWritable(
        this.#input.binding.fileBinding.persistedHandle, true,
      )
      try { await writable.truncate(checkpoint.committedLength); await writable.close() }
      catch (error) { await writable.abort(error).catch(() => undefined); throw error }
      this.#verified = undefined
      await this.verifyCheckpoint(checkpoint)
      return { kind: 'truncated', observationDigest: bytes((await this.observe(checkpoint.epochRoot)).digest) }
    } catch (error) {
      if (error instanceof DOMException && error.name === 'QuotaExceededError') {
        return { kind: 'destination-space-required' }
      }
      return { kind: targetFailure(error) === 'authorization-required'
        ? 'authorization-required' : 'target-verification-required' }
    }
  }

  async recoverMemberRollback(
    previous: DirectZipWriterCheckpointV1,
    rollback: DirectZipWriterCheckpointV1,
    proofs: () => AsyncIterable<DirectZipEpochProofV1>,
    signal?: AbortSignal,
  ): Promise<DirectZipTargetObservationV1> {
    signal?.throwIfAborted()
    if (this.#writable !== undefined) throw new DOMException('ZIP writer is active', 'InvalidStateError')
    const verified = await this.#verifyMemberRollbackTarget(previous, rollback, proofs)
    signal?.throwIfAborted()
    if (verified.checkpoint === rollback) return this.#acceptMemberRollback(rollback, verified.observation)

    // The caller binds the rollback candidate durably before entering here. After
    // publication, recovery must accept the shorter prefix without restoring old bytes.
    const closeFailure = await this.#input.namespaceMutations.run(async () => {
      let failure: Readonly<{ error: unknown }> | undefined
      signal?.throwIfAborted()
      await this.#recheckObservation(verified.observation, previous.epochRoot)
      signal?.throwIfAborted()
      const writable = await this.#input.fileSystem.createWritable(
        this.#input.binding.fileBinding.persistedHandle, true,
      )
      this.#writable = writable
      try {
        signal?.throwIfAborted()
        await this.#recheckObservation(verified.observation, previous.epochRoot)
        signal?.throwIfAborted()
        await writable.truncate(rollback.committedLength)
        signal?.throwIfAborted()
        try { await writable.close() } catch (error) { failure = { error } }
      } catch (error) {
        await writable.abort(error).catch(() => undefined)
        throw error
      } finally {
        if (failure !== undefined) await writable.abort(failure.error).catch(() => undefined)
        this.#writable = undefined
        this.#verified = undefined
        this.#closedEpoch = undefined
        this.#candidateRoot = undefined
      }
      return failure
    })
    const published = await this.#verifyMemberRollbackTarget(previous, rollback, proofs)
    if (published.checkpoint !== rollback) {
      if (closeFailure !== undefined) throw closeFailure.error
      throw new DOMException('The ZIP rollback was not published', 'DataError')
    }
    return this.#acceptMemberRollback(rollback, published.observation)
  }

  async deleteOwnedMemberRollback(
    previous: DirectZipWriterCheckpointV1,
    rollback: DirectZipWriterCheckpointV1,
    proofs: () => AsyncIterable<DirectZipEpochProofV1>,
  ): Promise<void> {
    if (this.#writable !== undefined) throw new DOMException('ZIP writer is active', 'InvalidStateError')
    try {
      const verified = await this.#verifyMemberRollbackTarget(previous, rollback, proofs)
      await this.#removeVerifiedTarget(verified.observation.digest, verified.checkpoint.epochRoot)
    } catch (error) {
      // An interrupted deletion can leave the candidate durable after its exact entry is gone.
      if (error instanceof DOMException && error.name === 'NotFoundError') return
      throw error
    }
  }

  async readBoundedCompletionProof(input: Readonly<{
    exactArchiveBytes: bigint; rootCentralRecordOffset: bigint
    rootCentralRecordBytes: bigint; closingTailBytes: number
  }>) {
    const snapshot = await this.#ownedSnapshot()
    if (snapshot.size !== input.exactArchiveBytes) throw new Error('ZIP completion length changed')
    const headerLength = deriveDirectZipOwnershipHeaderReadBytes(await snapshot.read(0n, 30n))
    const epochRoot = this.#candidateRoot ?? await this.#predecessorRoot()
    return {
      localOwnershipHeader: await snapshot.read(0n, BigInt(headerLength)),
      rootCentralRecord: await snapshot.read(input.rootCentralRecordOffset,
        input.rootCentralRecordOffset + input.rootCentralRecordBytes),
      closingTail: await snapshot.read(snapshot.size - BigInt(input.closingTailBytes), snapshot.size),
      observationDigest: bytes((await this.#observation(snapshot, epochRoot)).digest),
    }
  }

  async deleteOwned(checkpoint: DirectZipWriterCheckpointV1, candidate?: DirectZipEpochCandidateV1): Promise<void> {
    if (this.#writable !== undefined) throw new DOMException('ZIP writer is active', 'InvalidStateError')
    try { await this.#ownedSnapshot() } catch (error) {
      // Deletion may finish before its lifecycle transaction. A retry proves absence without recreating the file.
      if (error instanceof DOMException && error.name === 'NotFoundError') return
      throw error
    }
    let expectedObservation: string
    let expectedRoot = checkpoint.epochRoot
    if (candidate === undefined) {
      await this.verifyCheckpoint(checkpoint)
      expectedObservation = this.#verified!.observation
    } else {
      const observed = await this.observeCandidate(candidate)
      if (observed.ownership !== 'matching' ||
          !(observed.length === 'candidate' && observed.candidateIntegrity === 'verified' ||
            observed.length === 'predecessor' && observed.predecessorIntegrity === 'verified')) {
        throw new DOMException('The retained ZIP candidate cannot be safely deleted', 'DataError')
      }
      expectedObservation = encodeBase64Url(observed.observationDigest!)
      if (observed.length === 'candidate') expectedRoot = candidate.expectedEpochRoot
    }
    await this.#removeVerifiedTarget(expectedObservation, expectedRoot)
  }

  async #removeVerifiedTarget(expectedObservation: string, expectedRoot: Uint8Array): Promise<void> {
    // Proof scans retain only target exclusivity. Namespace coordination is needed
    // for the bounded ownership recheck and removal, not for reading the archive.
    await this.#input.namespaceMutations.run(async () => {
      await this.#recheckObservation({ digest: expectedObservation }, expectedRoot)
      await this.#input.fileSystem.removeExactName(
        this.#input.binding.parentBinding.persistedHandle, this.#input.binding.stableName,
      )
    })
  }

  async #recheckObservation(expected: Readonly<{ digest: string }>, root: Uint8Array): Promise<void> {
    const observation = await this.#observation(await this.#ownedSnapshot(), root)
    if (observation.digest !== expected.digest) {
      throw new DOMException('The retained ZIP changed before mutation', 'DataError')
    }
  }

  async #verifyMemberRollbackTarget(
    previous: DirectZipWriterCheckpointV1,
    rollback: DirectZipWriterCheckpointV1,
    proofs: () => AsyncIterable<DirectZipEpochProofV1>,
  ) {
    if (rollback.operationId !== previous.operationId ||
        rollback.phase !== 'between-members' || rollback.member !== undefined ||
        rollback.committedLength < this.#input.binding.bootstrapPrefixLength ||
        rollback.committedLength >= previous.committedLength) {
      throw new DOMException('The ZIP member rollback is invalid', 'DataError')
    }
    const snapshot = await this.#ownedSnapshot()
    let checkpoint: DirectZipWriterCheckpointV1 | undefined
    if (snapshot.size === previous.committedLength) checkpoint = previous
    else if (snapshot.size === rollback.committedLength) checkpoint = rollback
    // A valid retained prefix never authorizes removal of an unverified discarded
    // suffix. Both durable shapes must be exact; unknown tails remain untouched.
    if (checkpoint === undefined ||
        checkpoint === previous &&
          !await this.#verifyProofs(snapshot, previous.committedLength, previous.epochRoot) ||
        !await this.#verifyProofs(snapshot, rollback.committedLength, rollback.epochRoot, proofs)) {
      throw new DOMException('The retained ZIP does not match its rollback proofs', 'DataError')
    }
    return { checkpoint, observation: await this.#observation(snapshot, checkpoint.epochRoot) }
  }

  #acceptMemberRollback(
    checkpoint: DirectZipWriterCheckpointV1,
    observation: DirectZipTargetObservationV1,
  ): DirectZipTargetObservationV1 {
    this.#verified = { generation: checkpoint.generation, observation: observation.digest }
    this.#closedEpoch = undefined
    this.#candidateRoot = undefined
    return observation
  }

  async abort(reason: unknown): Promise<void> {
    const writable = this.#writable
    this.#writable = undefined
    if (writable !== undefined) await writable.abort(reason)
  }

  async #ownedSnapshot(): Promise<DirectZipFileSnapshotPort> {
    const { binding, fileSystem } = this.#input
    if (await fileSystem.queryPermission(binding.parentBinding.persistedHandle) !== 'granted') {
      throw new DOMException('Access to the saved ZIP needs permission', 'NotAllowedError')
    }
    const found = await fileSystem.lookupExactName(binding.parentBinding.persistedHandle, binding.stableName)
    if (found.kind === 'absent') throw new DOMException('The saved ZIP was deleted', 'NotFoundError')
    if (found.kind !== 'file' ||
        !await binding.fileBinding.persistedHandle.isSameEntry(found.handle)) {
      throw new DOMException('The saved ZIP was replaced', 'DataError')
    }
    const snapshot = await fileSystem.snapshot(found.handle)
    const observation = await observeDirectZipTarget(snapshot, {
      resultRootComponent: binding.resultRootComponent, marker: binding.marker,
      parentLocator: 'same', fileLocator: 'same',
    })
    if (observation.marker.kind !== 'matching') {
      throw new DOMException('The ZIP ownership marker changed', 'DataError')
    }
    return snapshot
  }

  async #observation(snapshot: DirectZipFileSnapshotPort, epochRoot: Uint8Array) {
    const binding = this.#input.binding
    return createDirectZipTargetObservationV1({
      operationId: encodeBase64Url(binding.operationId),
      parentBindingDigest: encodeBase64Url(binding.parentBinding.bindingDigest),
      fileBindingDigest: encodeBase64Url(binding.fileBinding.bindingDigest),
      ownershipMarkerDigest: encodeBase64Url(digestDirectZipArchiveBytes(
        encodeDirectZipBootstrapPrefixV1(binding.resultRootComponent, binding.marker))),
      exactLength: snapshot.size, lastModifiedMilliseconds: snapshot.lastModified,
      epochRootDigest: encodeBase64Url(epochRoot),
    })
  }

  async #predecessorRoot(): Promise<Uint8Array> {
    let root: Uint8Array = directZipEpochGenesisRoot()
    for await (const proof of this.#input.proofs()) root = proof.epochRoot
    return root
  }

  async #verifyProofs(
    snapshot: DirectZipFileSnapshotPort,
    length: bigint,
    root: Uint8Array,
    proofs: () => AsyncIterable<DirectZipEpochProofV1> = () => this.#input.proofs(),
  ) {
    if (snapshot.size < length) return false
    let offset = 0n
    let expectedRoot: Uint8Array = directZipEpochGenesisRoot()
    for await (const proof of proofs()) {
      if (proof.end > length) break
      if (proof.start !== offset || !equalBytes(proof.predecessorRoot, expectedRoot)) return false
      const digest = await digestRange(snapshot, proof.start, proof.end)
      if (!equalBytes(digest, proof.contentDigest)) return false
      expectedRoot = chainDirectZipEpochDigestV1({ ...proof, contentDigest: digest })
      if (!equalBytes(expectedRoot, proof.epochRoot)) return false
      offset = proof.end
    }
    return offset === length && equalBytes(expectedRoot, root)
  }
}

export function bytes(value: string): Uint8Array<ArrayBuffer> {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined) throw new TypeError('Invalid Direct ZIP identity')
  return Uint8Array.from(decoded)
}

async function digestRange(snapshot: DirectZipFileSnapshotPort, start: bigint, end: bigint) {
  if (start < 0n || end < start || end > snapshot.size) throw new RangeError('ZIP proof escaped target')
  const digest = new DirectZipSha256Accumulator()
  for (let offset = start; offset < end;) {
    const next = offset + BigInt(DIRECT_ZIP_RANGE_HASH_CHUNK_BYTES) < end
      ? offset + BigInt(DIRECT_ZIP_RANGE_HASH_CHUNK_BYTES) : end
    const chunk = await snapshot.read(offset, next)
    if (chunk.byteLength !== Number(next - offset)) throw new Error('ZIP proof read was incomplete')
    digest.update(chunk)
    offset = next
  }
  return digest.digest()
}

function targetFailure(error: unknown):
  'authorization-required' | 'target-deleted' | 'foreign-target' | 'target-verification-required' {
  if (error instanceof DOMException) {
    if (error.name === 'NotAllowedError' || error.name === 'SecurityError') return 'authorization-required'
    if (error.name === 'NotFoundError') return 'target-deleted'
    if (error.name === 'DataError') return 'foreign-target'
  }
  return 'target-verification-required'
}

function candidateObservation(input: {
  isCandidate: boolean; isPredecessor: boolean; candidateValid: boolean
  bounded: boolean; predecessor: boolean; hasTail: boolean; digest: string
}): DirectZipCandidateObservationV1 {
  let length: DirectZipCandidateObservationV1['length'] = input.hasTail ? 'unknown-tail' : 'other'
  if (input.isCandidate) length = 'candidate'
  else if (input.isPredecessor) length = 'predecessor'
  let observationMatch: DirectZipCandidateObservationV1['observationMatch'] = 'neither'
  if (input.candidateValid) observationMatch = 'candidate'
  else if (input.isPredecessor) observationMatch = 'predecessor'
  let candidateIntegrity: DirectZipCandidateObservationV1['candidateIntegrity'] = 'mismatch'
  if (input.candidateValid) candidateIntegrity = input.bounded ? 'writer-bounded-proof' : 'verified'
  return {
    permission: 'granted', presence: 'present', ownership: 'matching', length,
    observationMatch, candidateIntegrity, predecessorIntegrity: input.predecessor ? 'verified' : 'mismatch',
    observationDigest: bytes(input.digest),
  }
}
