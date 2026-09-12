import { describe, expect, it } from 'vitest'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import { initialReceiveLifecycleState, nextReceiveLifecycleState } from '../../../src/output/workspace/state'
import { decodeStoredReceiveLifecycleState, storedReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import { validatePersistedReceiveRecord } from '../../../src/output/workspace/records'
import { receiveElapsedMilliseconds, snapshotReceiveTiming } from '../../../src/output/workspace/lifecycle/timing'

const OPERATION_ID = encodeBase64Url(new Uint8Array(16).fill(1))
const INTENT_DIGEST = encodeBase64Url(new Uint8Array(32).fill(2))
const LEASE_ID = encodeBase64Url(new Uint8Array(16).fill(3))
const PACKAGE_DIGEST = encodeBase64Url(new Uint8Array(32).fill(4))

function initial() {
  return initialReceiveLifecycleState({
    operationId: OPERATION_ID, receiveIntentDigest: INTENT_DIGEST, startedAtMilliseconds: 1_000,
  })
}

describe('receive elapsed time', () => {
  it('keeps the original start across pause, durable reload and resume, then freezes at ready', async () => {
    const receiving = nextReceiveLifecycleState(initial(), { kind: 'receiving', activeLeaseId: LEASE_ID })
    const paused = nextReceiveLifecycleState(receiving, {
      kind: 'resumable-receive', payloadKind: 'opfs-zip', objectId: PACKAGE_DIGEST,
      checkpointGeneration: 1n, occupiedBytes: 10n, completedFileCount: 1n,
      completedBytes: 10n, discoveryComplete: false,
    })
    expect(receiveElapsedMilliseconds(paused.timing)).toBeNull()
    const row = await validatePersistedReceiveRecord(structuredClone(await storedReceiveLifecycleState(paused)))
    const reopened = decodeStoredReceiveLifecycleState(row)
    const resumed = nextReceiveLifecycleState(reopened, { kind: 'receiving', activeLeaseId: LEASE_ID })
    const ready = nextReceiveLifecycleState(resumed,
      { kind: 'artifact-sealed', packageDigest: PACKAGE_DIGEST }, () => 126_000)
    expect(receiveElapsedMilliseconds(ready.timing)).toBe(125_000)
    const waiting = nextReceiveLifecycleState(ready,
      { kind: 'waiting-to-save', packageDigest: PACKAGE_DIGEST }, () => 200_000)
    const saved = nextReceiveLifecycleState(waiting,
      { kind: 'published', receiptDigest: PACKAGE_DIGEST, cleanupState: 'cleanup-pending' }, () => 300_000)
    const clean = nextReceiveLifecycleState(saved,
      { kind: 'published', receiptDigest: PACKAGE_DIGEST, cleanupState: 'clean' }, () => 400_000)
    expect(receiveElapsedMilliseconds(decodeStoredReceiveLifecycleState(await storedReceiveLifecycleState(clean)).timing))
      .toBe(125_000)
  })

  it.each([
    { kind: 'published', receiptDigest: PACKAGE_DIGEST, cleanupState: 'clean' } as const,
    { kind: 'download-started', attemptKind: 'portable', attemptId: LEASE_ID } as const,
    { kind: 'partial-directory', reason: 'failures', successCount: 1n, failureCount: 1n, receiptDigest: PACKAGE_DIGEST } as const,
    { kind: 'waiting-to-save', packageDigest: PACKAGE_DIGEST } as const,
  ])('records the first ready result for $kind', async payload => {
    const ready = nextReceiveLifecycleState(initial(), payload, () => 2_234)
    expect(receiveElapsedMilliseconds(ready.timing)).toBe(1_234)
    expect(decodeStoredReceiveLifecycleState(await storedReceiveLifecycleState(ready)).timing).toEqual(ready.timing)
  })

  it('does not let display observations change canonical authority or fabricate missing history', async () => {
    const ready = nextReceiveLifecycleState(initial(),
      { kind: 'published', receiptDigest: PACKAGE_DIGEST, cleanupState: 'clean' }, () => 5_000)
    const row = await storedReceiveLifecycleState(ready)
    const { timing, ...withoutTiming } = row
    expect(timing).toEqual(ready.timing)
    const older = decodeStoredReceiveLifecycleState(withoutTiming)
    expect(receiveElapsedMilliseconds(older.timing)).toBeNull()
    expect((await storedReceiveLifecycleState(older)).digest).toBe(row.digest)
    const malformed = decodeStoredReceiveLifecycleState({ ...row,
      timing: { startedAtMilliseconds: 5_000, resultReadyAtMilliseconds: 1_000 } })
    expect(malformed.kind).toBe('published')
    expect(malformed.timing).toBeUndefined()
  })

  it('bounds clock rollback and ignores invalid observations', () => {
    const ready = nextReceiveLifecycleState(initial(),
      { kind: 'artifact-sealed', packageDigest: PACKAGE_DIGEST }, () => 500)
    expect(receiveElapsedMilliseconds(ready.timing)).toBe(0)
    for (const value of [null, {}, { startedAtMilliseconds: NaN }, { startedAtMilliseconds: -1 },
      { startedAtMilliseconds: 1, resultReadyAtMilliseconds: Infinity }]) {
      expect(snapshotReceiveTiming(value)).toBeUndefined()
    }
  })
})
