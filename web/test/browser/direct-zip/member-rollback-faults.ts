import {
  IndexedDbDirectZipJournalRepository, type DirectZipJournalRepository,
} from '../../../src/output/direct-zip/journal'
import {
  createMemberRollbackFixture, readMemberRollbackState,
} from './member-rollback-fixture'
import { finishMemberRollbackFixture } from './member-rollback-probe'

export type MemberRollbackFault = 'before-truncate' | 'cancel-before-truncate' | 'after-truncate'
export type MemberRollbackTamper = 'ownership-marker' | 'completed-prefix'
type Fixture = Awaited<ReturnType<typeof createMemberRollbackFixture>>

export async function probeProductionMemberRollbackFault(databaseName: string, mode: MemberRollbackFault) {
  const fixture = await createMemberRollbackFixture(databaseName, 'changed-content')
  try {
    const interrupted = await interruptMemberRollback(fixture, mode)
    const active = await fixture.resume()
    const recovered = await readMemberRollbackState(databaseName, fixture.intent.operationId)
    const recoveredFile = new Uint8Array(await (await fixture.file.getFile()).arrayBuffer())
    return {
      completed: await finishMemberRollbackFixture(fixture, active),
      interrupted,
      recovered: {
        phase: recovered.checkpoint.phase, safePayload: recovered.checkpoint.committedSelectedPayloadBytes.toString(),
        ordinal: recovered.checkpoint.entryOrdinal.toString(), candidatePresent: recovered.candidate !== undefined,
        fileBytes: recoveredFile.byteLength, archiveOffset: Number(recovered.checkpoint.archiveOffset),
        prefix: Array.from(recoveredFile),
      },
      rollbackOffset: fixture.rollbackOffset, prefix: Array.from(fixture.prefix),
    }
  } finally { await fixture.close() }
}

export async function probeProductionMemberRollbackTamper(databaseName: string, mode: MemberRollbackTamper) {
  const fixture = await createMemberRollbackFixture(databaseName, 'changed-content')
  try {
    const interrupted = await interruptMemberRollback(fixture, 'before-truncate')
    const marker = mode === 'ownership-marker' ? fixture.ownershipNonce : Uint8Array.of(11, 12, 13)
    const offset = sequenceOffset(fixture.original, marker)
    if (offset < 0 || offset >= fixture.rollbackOffset) throw new Error('Tampering did not locate the retained prefix')
    const writable = await fixture.file.createWritable({ keepExistingData: true })
    await writable.write({ type: 'write', position: offset, data: Uint8Array.of(fixture.original[offset]! ^ 1) })
    await writable.close()
    const tampered = new Uint8Array(await (await fixture.file.getFile()).arrayBuffer())
    let rejected = false
    try { await fixture.resume() } catch { rejected = true }
    const retained = await readMemberRollbackState(databaseName, fixture.intent.operationId)
    const after = new Uint8Array(await (await fixture.file.getFile()).arrayBuffer())
    return {
      rejected, interrupted, before: Array.from(tampered), after: Array.from(after),
      checkpointDigest: retained.checkpoint.digest, expectedCheckpointDigest: fixture.paused.digest,
      candidateDigest: retained.candidate?.digest,
      resumedRanges: fixture.source.ranges.filter(range => range.phase === 'resumed'),
    }
  } finally { await fixture.close() }
}

export async function interruptMemberRollback(fixture: Fixture, mode: MemberRollbackFault) {
  const fault = faultingRollbackRepository(fixture.databaseName, mode)
  const active = await fixture.resume(fault.open)
  const execution = await active.plans.openDirectResumableZip(fixture.intent, fault.signal)
  let interrupted = false
  try { await fixture.source.run(execution, 'resumed', fault.signal) }
  catch (error) {
    if (error !== fault.error) throw error
    interrupted = true
  }
  if (!interrupted) throw new Error('Rollback did not reach its injected persistence boundary')
  const retained = await readMemberRollbackState(fixture.databaseName, fixture.intent.operationId)
  const file = new Uint8Array(await (await fixture.file.getFile()).arrayBuffer())
  if (retained.candidate?.kind !== 'rollback') throw new Error('Interrupted rollback lost its durable intent')
  await fixture.detach()
  return {
    checkpointPhase: retained.checkpoint.phase, safePayload: retained.checkpoint.committedSelectedPayloadBytes.toString(),
    checkpointDigest: retained.checkpoint.digest, candidateKind: retained.candidate.kind,
    candidateDigest: retained.candidate.digest, proposalPhase: retained.candidate.proposedCheckpoint.phase,
    fileBytes: file.byteLength, bytes: Array.from(file), originalBytes: Array.from(fixture.original),
    resumedRanges: fixture.source.ranges.filter(range => range.phase === 'resumed'),
  }
}

function faultingRollbackRepository(databaseName: string, mode: MemberRollbackFault) {
  const cancellation = new AbortController()
  const error = new DOMException('Injected rollback interruption: ' + mode,
    mode === 'cancel-before-truncate' ? 'AbortError' : 'UnknownError')
  let pending = true
  return {
    error, signal: cancellation.signal,
    open: async (): Promise<DirectZipJournalRepository> => {
      const repository: DirectZipJournalRepository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
      return new Proxy(repository, {
        get: (target, property) => {
          if (property === 'bindRollbackCandidate') return async (
            ...args: Parameters<DirectZipJournalRepository['bindRollbackCandidate']>
          ) => {
            await target.bindRollbackCandidate(...args)
            if (pending && mode === 'before-truncate') { pending = false; throw error }
            if (pending && mode === 'cancel-before-truncate') { pending = false; cancellation.abort(error) }
          }
          if (property === 'promoteRollbackCandidate') return async (
            ...args: Parameters<DirectZipJournalRepository['promoteRollbackCandidate']>
          ) => {
            if (pending && mode === 'after-truncate') { pending = false; throw error }
            return target.promoteRollbackCandidate(...args)
          }
          const value = Reflect.get(target, property)
          return typeof value === 'function' ? value.bind(target) : value
        },
      })
    },
  }
}

function sequenceOffset(bytes: Uint8Array, sequence: Uint8Array): number {
  return bytes.findIndex((_byte, offset) => sequence.every((value, index) => bytes[offset + index] === value))
}
