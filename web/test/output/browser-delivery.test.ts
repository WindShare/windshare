import { describe, expect, it } from 'vitest'
import {
  advanceBrowserDeliveryRecord, assertBrowserDeliveryTransition, createBrowserDeliveryRecord,
  createBrowserSavePolicy, snapshotBrowserDeliveryRecord, summarizeBrowserDeliveries,
  validateBrowserDeliveryRecord, validateBrowserSavePolicy, BrowserDeliveryLiveProjection, type BrowserDeliveryState,
} from '../../src/output/browser-delivery'
import { newFileCheckpointV2 } from '../../src/output/persistence/checkpoint'
import { deliveryFixture, deliveryIdentity, deliveryPolicy } from './browser-delivery-fixture'

describe('immutable browser save policy', () => {
  it('reuses only canonical immutable snapshots and validates clones and foreign policies', () => {
    const f = deliveryFixture('direct')
    const saved = f.advance(f.initial, { kind: 'target-saved', target: f.target })
    expect(validateBrowserSavePolicy(f.policy)).toBe(f.policy)
    expect(validateBrowserDeliveryRecord(f.policy, saved)).toBe(saved)
    const clone = structuredClone(saved)
    const validated = validateBrowserDeliveryRecord(f.policy, clone)
    expect(validated).not.toBe(clone)
    expect(validated).toEqual(saved)
    if (clone.state.kind !== 'target-saved') throw new Error('Expected target proof')
    Object.assign(clone.state.target.verifiedRanges[0]!, { end: 1n })
    expect(() => validateBrowserDeliveryRecord(f.policy, Object.freeze(clone))).toThrow()
    expect(() => validateBrowserDeliveryRecord(deliveryPolicy(), saved)).toThrow()
    expect(() => validateBrowserSavePolicy(Object.freeze({ ...f.policy, digest: deliveryIdentity(99) }))).toThrow()
    expect(Object.isFrozen(saved.source.canonicalPath)).toBe(true)
    expect(Object.isFrozen(saved.state)).toBe(true)
    if (saved.state.kind !== 'target-saved') throw new Error('Expected target proof')
    expect(Object.isFrozen(saved.state.target.verifiedRanges)).toBe(true)
    expect(Object.isFrozen(saved.state.target.verifiedRanges[0])).toBe(true)
  })

  it('binds both independent storage authorities without changing the final intent', () => {
    const policy = deliveryPolicy()
    expect(validateBrowserSavePolicy(structuredClone(policy))).toEqual(policy)
    expect(policy.target.operationId).toBe(policy.operationId)
    expect(policy.staging!.operationId).not.toBe(policy.operationId)
    expect(Object.isFrozen(policy.target)).toBe(true)
  })

  it.each(['preference', 'target', 'staging'] as const)('rejects changed %s under the saved digest', field => {
    const policy = deliveryPolicy()
    const modified = field === 'preference' ? { ...policy, preference: 'direct' as const, staging: undefined }
      : { ...policy, [field]: { ...policy[field], authorityRef: deliveryIdentity(30) } }
    expect(() => validateBrowserSavePolicy(modified as typeof policy)).toThrow()
  })

  it('rejects direct preference with OPFS authority and operation-aliasing namespaces', () => {
    const policy = deliveryPolicy()
    expect(() => createBrowserSavePolicy({ ...policy, preference: 'direct' })).toThrow(/direct/)
    expect(() => createBrowserSavePolicy({
      ...policy, staging: { ...policy.staging!, operationId: policy.operationId },
    })).toThrow(/distinct/)
  })

  it('does not allow a receiving destination or source intent to be substituted', () => {
    const policy = deliveryPolicy()
    expect(() => createBrowserSavePolicy({ ...policy, operationId: deliveryIdentity(50, 16) })).toThrow(/original/)
    expect(() => createBrowserSavePolicy({ ...policy, receiveIntentDigest: deliveryIdentity(50) })).toThrow(/original/)
  })
})

describe('durable per-file delivery semantics', () => {
  it('holds complete OPFS evidence separately from target saved evidence through cleanup', () => {
    const f = deliveryFixture()
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    expect(summarizeBrowserDeliveries(f.policy, [complete])).toMatchObject({
      recoverableBytes: 8n, stagedBytes: 8n, targetSavedBytes: 0n,
      localContinuation: 'save-staged-files',
    })
    const copying = f.advance(complete, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'first' } })
    const saved = f.advance(copying, { kind: 'target-saved', stage: f.stage!, target: f.target })
    const pending = f.advance(saved, { kind: 'cleanup-pending', stage: f.stage!, target: f.target })
    const cleaned = f.advance(pending, { kind: 'cleaned', target: f.target })
    expect(summarizeBrowserDeliveries(f.policy, [pending])).toMatchObject({
      recoverableBytes: 8n, targetSavedBytes: 8n, stagedBytes: 8n,
      localContinuation: 'retry-staging-cleanup',
    })
    expect(summarizeBrowserDeliveries(f.policy, [cleaned])).toMatchObject({
      targetSavedFiles: 1, targetSavedBytes: 8n, stagedBytes: 0n, reservedStagingBytes: 0n,
    })
  })

  it('permits direct saved completion without creating stage authority', () => {
    const f = deliveryFixture('direct')
    const saved = f.advance(f.initial, { kind: 'target-saved', target: f.target })
    const cleaned = f.advance(saved, { kind: 'cleaned', target: f.target })
    expect(summarizeBrowserDeliveries(f.policy, [cleaned]).targetSavedBytes).toBe(8n)
    expect(() => createBrowserDeliveryRecord({
      policy: f.policy, source: f.source, materializationRelativePath: f.source.canonicalPath, placement: 'staged', placementReason: 'not authorized',
    })).toThrow(/placement/)
  })

  it('reserves full staged file capacity but reports only confirmed durable bytes', () => {
    const f = deliveryFixture()
    const receiving = f.advance(f.initial, { kind: 'receiving', checkpoint: f.checkpoint('staged', 3n) })
    expect(summarizeBrowserDeliveries(f.policy, [receiving])).toMatchObject({
      recoverableBytes: 3n, stagedBytes: 3n, reservedStagingBytes: 8n, targetSavedBytes: 0n,
    })
  })

  it('cannot pass staging proof as a final target checkpoint', () => {
    const f = deliveryFixture()
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    const copying = f.advance(complete, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'one' } })
    expect(() => f.advance(copying, { kind: 'target-saved', target: f.stage!, stage: f.stage! })).toThrow(/checkpoint/)
  })

  it.each([
    { kind: 'copying' },
    { kind: 'target-saved' },
    { kind: 'cleanup-pending' },
    { kind: 'cleaned' },
  ] as const)('cannot skip initial staging phase into $kind', ({ kind }) => {
    const f = deliveryFixture()
    const state = { kind, stage: f.stage!, target: f.target, attempt: { attemptId: 'one' } } as BrowserDeliveryState
    expect(() => f.advance(f.initial, state)).toThrow()
  })

  it('requires complete durable ranges before network-independent copy continuation', () => {
    const f = deliveryFixture()
    expect(() => f.advance(f.initial, { kind: 'staged-complete', stage: f.checkpoint('staged', 7n) })).toThrow(/checkpoint/)
  })

  it.each(['fileRevision', 'exactSize', 'canonicalPath'] as const)('rejects a changed authenticated %s', field => {
    const f = deliveryFixture()
    const changes = {
      fileRevision: deliveryIdentity(31, 16), exactSize: 9n, canonicalPath: ['elsewhere.bin'],
    }
    const source = { ...f.source, [field]: changes[field] }
    const changed = snapshotBrowserDeliveryRecord(f.policy, { ...f.initial, source, generation: 2n })
    expect(() => assertBrowserDeliveryTransition(f.policy, f.initial, changed)).toThrow(/immutable/)
  })

  it('retains complete staging after a failed and drained target copy attempt', () => {
    const f = deliveryFixture()
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    const copying = f.advance(complete, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'first' } })
    const retry = f.advance(copying, { kind: 'staged-complete', stage: f.stage!, failureReason: 'target denied' })
    expect(summarizeBrowserDeliveries(f.policy, [retry]).localContinuation).toBe('save-staged-files')
    expect(() => f.advance(copying, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'second' } })).toThrow(/drained/)
    expect(f.advance(retry, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'second' } }).state.kind).toBe('copying')
  })

  it('binds an opened target object to its copy attempt and prevents substitution at save', () => {
    const f = deliveryFixture()
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    const copying = f.advance(complete, {
      kind: 'copying', stage: f.stage!, attempt: { attemptId: 'first', targetOwnedObjectId: deliveryIdentity(33) },
    })
    expect(() => f.advance(copying, { kind: 'target-saved', stage: f.stage!, target: f.target })).toThrow(/owned copy/)
  })

  it('does not regress checkpoint ranges or change a started storage object', () => {
    const f = deliveryFixture()
    const receiving = f.advance(f.initial, { kind: 'receiving', checkpoint: f.checkpoint('staged', 4n) })
    expect(() => f.advance(receiving, { kind: 'receiving', checkpoint: f.checkpoint('staged', 3n) })).toThrow(/discard/)
    const foreign = newFileCheckpointV2({ ...f.stage!, ownedObjectId: deliveryIdentity(40) })
    expect(() => f.advance(receiving, { kind: 'staged-complete', stage: foreign })).toThrow(/ownership/)
  })

  it('keeps stage and saved target immutable until cleanup finishes', () => {
    const f = deliveryFixture()
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    const different = newFileCheckpointV2({ ...f.stage!, checkpointGeneration: 2n })
    expect(() => f.advance(complete, { kind: 'copying', stage: different, attempt: { attemptId: 'one' } })).toThrow(/proof cannot change/)
    const copying = f.advance(complete, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'one' } })
    const saved = f.advance(copying, { kind: 'target-saved', stage: f.stage!, target: f.target })
    expect(() => f.advance(saved, { kind: 'cleaned', target: f.target })).toThrow(/cleanup-pending/)
    const otherTarget = newFileCheckpointV2({ ...f.target, checkpointGeneration: 2n })
    expect(() => f.advance(saved, { kind: 'cleanup-pending', stage: f.stage!, target: otherTarget })).toThrow(/Saved target/)
  })

  it('detects stored-state tampering and snapshots mutable source arrays', () => {
    const f = deliveryFixture()
    const original = structuredClone(f.initial)
    expect(validateBrowserDeliveryRecord(f.policy, original)).toEqual(f.initial)
    expect(() => validateBrowserDeliveryRecord(f.policy, { ...original, placementReason: 'changed' })).toThrow(/digest/)
    expect(Object.isFrozen(f.initial.source.canonicalPath)).toBe(true)
    expect(() => summarizeBrowserDeliveries(f.policy, [f.initial, f.initial])).toThrow(/repeats/)
  })

  it('offers local continuation for a complete stage before its delivery phase catches up', () => {
    const f = deliveryFixture()
    const receiving = f.advance(f.initial, { kind: 'receiving', checkpoint: f.stage! })
    expect(summarizeBrowserDeliveries(f.policy, [receiving])).toMatchObject({
      receivingFiles: 0, stagedCompleteFiles: 1, targetSavedBytes: 0n, localContinuation: 'save-staged-files',
    })
  })

  it('persists explicit abandonment without fabricating a successful target save', () => {
    const f = deliveryFixture()
    const receiving = f.advance(f.initial, { kind: 'receiving', checkpoint: f.checkpoint('staged', 3n) })
    const discarding = f.advance(receiving, { kind: 'discarding', checkpoint: f.checkpoint('staged', 3n) })
    expect(summarizeBrowserDeliveries(f.policy, [discarding])).toMatchObject({
      cleanupPendingFiles: 1, stagedBytes: 3n, targetSavedBytes: 0n, localContinuation: 'retry-staging-cleanup',
    })
    const discarded = f.advance(discarding, { kind: 'discarded' })
    expect(summarizeBrowserDeliveries(f.policy, [discarded])).toMatchObject({
      discardedFiles: 1, targetSavedFiles: 0, recoverableBytes: 0n, stagedBytes: 0n,
    })
    expect(() => f.advance(discarded, { kind: 'receiving' })).toThrow(/durable phase/)
  })

  it('retains complete stage ownership throughout explicit abandonment', () => {
    const f = deliveryFixture()
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    expect(() => f.advance(complete, { kind: 'discarding' })).toThrow(/retain/)
    expect(f.advance(complete, { kind: 'discarding', checkpoint: f.stage! }).state.kind).toBe('discarding')
  })

  it('updates live summary with only the changed file and ignores exact duplicate events', () => {
    const f = deliveryFixture()
    const live = new BrowserDeliveryLiveProjection(f.policy)
    live.replace(f.initial)
    const other = createBrowserDeliveryRecord({
      policy: f.policy, source: { ...f.source, fileId: deliveryIdentity(61, 16) },
      materializationRelativePath: f.source.canonicalPath, placement: 'direct', placementReason: 'small-file',
    })
    live.replace(other)
    const complete = f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! })
    live.replace(complete)
    live.replace(complete)
    expect(live.summary()).toEqual(summarizeBrowserDeliveries(f.policy, [complete, other]))
    expect(() => live.replace(f.initial)).toThrow(/stale/)
    const copying = f.advance(complete, { kind: 'copying', stage: f.stage!, attempt: { attemptId: 'local' } })
    const saved = f.advance(copying, { kind: 'target-saved', target: f.target, stage: f.stage! })
    live.replace(saved)
    expect(live.summary()).toEqual(summarizeBrowserDeliveries(f.policy, [saved, other]))
  })

  it('rejects generation jumps independently of valid state hashes', () => {
    const f = deliveryFixture('direct')
    const next = advanceBrowserDeliveryRecord(f.policy, f.initial, { kind: 'target-saved', target: f.target })
    const skipped = snapshotBrowserDeliveryRecord(f.policy, { ...next, generation: 9n })
    expect(() => assertBrowserDeliveryTransition(f.policy, f.initial, skipped)).toThrow(/immutable/)
  })
})
