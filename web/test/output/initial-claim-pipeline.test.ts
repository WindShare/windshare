import { describe, expect, it } from 'vitest'
import { InitialClaimPipeline } from '../../src/output/persistent-tree/initial-claim-pipeline'
import { PersistentInitialClaimCoordinator } from '../../src/output/persistent-tree/recovery'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import { deferred } from './persistent-tree-file-fixture'
import { materializationFixture, revision } from './persistent-tree-session-fixture'
import { identity } from './planning/fixture'

describe('initial claim pipeline authority and backpressure', () => {
  it('bounds classified and ready claims while the journal is stalled, then refills', async () => {
    const maximumResident = 4
    const entered = deferred<void>()
    const release = deferred<void>()
    const residentInspected = deferred<void>()
    const admitted: string[] = []
    const inspected: string[] = []
    let firstCommit = true
    const pipeline = new InitialClaimPipeline<{ key: string; lineageId: string }, string, string>({
      maximumResident,
      maximumInspecting: 2,
      admitted: input => { admitted.push(input.key) },
      classify: async inputs => inputs.map(() => undefined),
      inspect: async input => {
        inspected.push(input.key)
        if (inspected.length === maximumResident) residentInspected.resolve()
        return input.key
      },
      settle: async ready => {
        if (firstCommit) {
          firstCommit = false
          entered.resolve()
          await release.promise
        }
        return ready.map(item => item.inspection)
      },
    })
    const results = Array.from({ length: 10 }, (_, index) => pipeline.select({
      key: String(index), lineageId: String(index),
    }))
    await entered.promise
    await residentInspected.promise
    expect(admitted).toEqual(['0', '1', '2', '3'])
    expect(inspected).toEqual(admitted)
    release.resolve()
    await expect(Promise.all(results)).resolves.toEqual(Array.from({ length: 10 }, (_, index) => String(index)))
  })

  it('coalesces matching active claims but reclassifies different revisions after installation', async () => {
    const fixture = await materializationFixture(undefined, 2)
    const coordinator = new PersistentInitialClaimCoordinator(fixture.tree, fixture.checkpoints, 2)
    const path = snapshotMaterializationRootRelativePath(['same.bin'])
    const inspection = fixture.tree.deferFileInspection(path)
    const firstRevision = revision(2n)
    const first = coordinator.select(firstRevision, path)
    await inspection.started
    const duplicate = coordinator.select(firstRevision, path)
    const changed = coordinator.select({ ...firstRevision, fileRevision: identity(100) }, path)
    const changedSize = coordinator.select({ ...firstRevision, exactSize: 3n }, path)
    expect(duplicate).toBe(first)
    inspection.resolve()
    await expect(first).resolves.toMatchObject({ kind: 'installed' })
    await expect(changed).resolves.toMatchObject({ kind: 'revision-conflict' })
    await expect(changedSize).resolves.toMatchObject({ kind: 'invalid' })
    expect(fixture.tree.inspectionStarts).toEqual(['same.bin'])
    expect(fixture.checkpoints.installedClaimBatches).toEqual([['same.bin']])
  })

  it.each(['classify', 'inspect', 'settle'] as const)(
    'contains a synchronous %s failure and releases the lineage for a later claim',
    async failedStage => {
      let fail = true
      const failure = new Error('injected synchronous operation failure')
      const run = (stage: typeof failedStage) => {
        if (fail && stage === failedStage) {
          fail = false
          throw failure
        }
      }
      const pipeline = new InitialClaimPipeline<{ key: string; lineageId: string }, string, string>({
        maximumResident: 2,
        maximumInspecting: 1,
        classify: inputs => { run('classify'); return Promise.resolve(inputs.map(() => undefined)) },
        inspect: input => { run('inspect'); return Promise.resolve(input.key) },
        settle: ready => { run('settle'); return Promise.resolve(ready.map(item => item.inspection)) },
        observe: () => { throw new Error('observation failure') },
        admitted: () => { throw new Error('observation failure') },
        completed: () => { throw new Error('observation failure') },
        drained: () => { throw new Error('observation failure') },
      })
      await expect(pipeline.select({ key: 'first', lineageId: 'same' })).rejects.toBe(failure)
      await expect(pipeline.select({ key: 'next', lineageId: 'same' })).resolves.toBe('next')
    },
  )

  it.each([[0, 1], [2, 0], [2, 3], [1.5, 1], [2, 1.5]])(
    'rejects invalid residence/inspection limits %s/%s',
    (maximumResident, maximumInspecting) => {
      expect(() => new InitialClaimPipeline({
        maximumResident: maximumResident!, maximumInspecting: maximumInspecting!,
        classify: async () => [], inspect: async () => undefined, settle: async () => [],
      })).toThrow('initial claim pipeline capacity is invalid')
    },
  )
})
