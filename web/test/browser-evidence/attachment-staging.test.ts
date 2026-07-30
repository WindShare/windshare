import { access, mkdir, mkdtemp, readFile, readdir, rename, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { basename, dirname, join } from 'node:path'

import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import {
  BrowserSampleStaging,
  requireFinalizedArtifactCollectionRoot,
  type BrowserSampleStagingHooks,
  type FinalizedArtifactCollection,
} from '../../scripts/browser-evidence/process/attachment-staging.ts'
import { createDeterministicTestContainmentBackend } from './deterministic-containment.ts'
import {
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  runSyntheticSample,
  type FrameworkTopology,
} from './framework-fixtures.ts'

let topology: FrameworkTopology
const workspaces: string[] = []

beforeAll(async () => { topology = await loadFrameworkTopology() })
afterEach(async () => {
  await Promise.all(workspaces.splice(0).map(removeFrameworkWorkspace))
})

describe('owner-fenced browser attachment collection', () => {
  it('fails closed when the collection path is swapped during finalization', async () => {
    let ownedBackup = ''
    const staging = await preparedStaging({
      beforeFinalize: async () => {
        ownedBackup = `${staging.childAttachmentRoot}.owned`
        await rename(staging.childAttachmentRoot, ownedBackup)
        await mkdir(staging.childAttachmentRoot)
        await writeFile(join(staging.childAttachmentRoot, 'foreign.txt'), 'must survive rollback', 'utf8')
      },
    })
    await expect(staging.finalize())
      .rejects.toThrow(/owner-held directory/u)
    await expect(staging.dispose()).rejects.toThrow(/cleanup did not fully settle/u)

    await expect(readFile(join(staging.childAttachmentRoot, 'foreign.txt'), 'utf8'))
      .resolves.toBe('must survive rollback')
    await expect(readFile(join(ownedBackup, 'child', 'evidence.jsonl'), 'utf8'))
      .resolves.toBe('')
  })

  it('never deletes a foreign directory swapped in immediately before rollback', async () => {
    let ownedBackup = ''
    const staging = await preparedStaging({
      beforeRollback: async () => {
        ownedBackup = `${staging.childAttachmentRoot}.owned`
        await rename(staging.childAttachmentRoot, ownedBackup)
        await mkdir(staging.childAttachmentRoot)
        await writeFile(
          join(staging.childAttachmentRoot, 'foreign.txt'),
          'foreign rollback target',
          'utf8',
        )
      },
    })

    await expect(staging.dispose()).rejects.toThrow(/cleanup did not fully settle/u)
    await expect(readFile(join(staging.childAttachmentRoot, 'foreign.txt'), 'utf8'))
      .resolves.toBe('foreign rollback target')
    await expect(readFile(join(ownedBackup, 'child', 'evidence.jsonl'), 'utf8'))
      .resolves.toBe('')
  })

  it('restores provisional authority and removes a partial collection after finalization crashes', async () => {
    const workspace = await trackedWorkspace()
    const traces: Array<{ readonly milestone: string; readonly context?: Readonly<Record<string, unknown>> }> = []
    let finalized: FinalizedArtifactCollection | undefined

    await expect(runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      containmentBackend: createDeterministicTestContainmentBackend(),
      stagingHooks: {
        afterFinalize: async (collection) => {
          finalized = collection
          await writeFile(join(collection.absoluteRoot, 'partial-collection.txt'), 'partial', 'utf8')
          throw new Error('synthetic finalization crash')
        },
      },
      trace: (event) => { traces.push(event) },
    })).rejects.toThrow(/synthetic finalization crash/u)

    const resultPath = join(workspace, 'main', 'chromium', 'sample-1', 'result.json')
    expect(JSON.parse(await readFile(resultPath, 'utf8'))).toMatchObject({
      resultStatus: 'provisional',
      artifacts: [],
    })
    if (finalized === undefined) throw new Error('finalization hook did not observe the collection')
    await expect(access(finalized.absoluteRoot)).rejects.toThrow()

    const byMilestone = new Map(traces.map((trace) => [trace.milestone, trace]))
    expect(byMilestone.get('attachment-finalization-started')?.context).toMatchObject({
      backend: 'test',
      phase: 'assemble-parent-collection',
      settledFailureCount: 0,
    })
    expect(byMilestone.get('attachment-finalization-failed')?.context).toMatchObject({
      backend: 'test',
      phase: 'assemble-parent-collection',
      settledFailureCount: 1,
      settledFailuresTruncated: false,
    })
    expect(byMilestone.get('attachment-finalization-failed')?.context?.causeChain)
      .toContain('synthetic finalization crash')
    expect(byMilestone.get('attachment-rollback-started')?.context).toMatchObject({
      backend: 'test',
      phase: 'remove-owned-staging',
      settledFailureCount: 0,
    })
    expect(byMilestone.get('attachment-rollback-completed')?.context).toMatchObject({
      backend: 'test',
      phase: 'remove-owned-staging',
      settledFailureCount: 0,
    })
  })

  it('traces a settled rollback failure without deleting a collection-path replacement', async () => {
    const workspace = await trackedWorkspace()
    const traces: Array<{ readonly milestone: string; readonly context?: Readonly<Record<string, unknown>> }> = []
    let replacementRoot = ''
    let ownedBackup = ''

    await expect(runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      containmentBackend: createDeterministicTestContainmentBackend(),
      stagingHooks: {
        afterFinalize: async (collection) => {
          replacementRoot = collection.absoluteRoot
          ownedBackup = `${replacementRoot}.owned`
          await rename(replacementRoot, ownedBackup)
          await mkdir(replacementRoot)
          await writeFile(join(replacementRoot, 'foreign.txt'), 'foreign publication target', 'utf8')
          throw new Error('synthetic finalization path swap')
        },
      },
      trace: (event) => { traces.push(event) },
    })).rejects.toThrow(/staging rollback both failed/u)

    await expect(readFile(join(replacementRoot, 'foreign.txt'), 'utf8'))
      .resolves.toBe('foreign publication target')
    await expect(readFile(join(ownedBackup, 'child', 'evidence.jsonl'), 'utf8'))
      .resolves.not.toBe('')
    const byMilestone = new Map(traces.map((trace) => [trace.milestone, trace]))
    expect(byMilestone.get('attachment-finalization-failed')?.context).toMatchObject({
      backend: 'test',
      phase: 'assemble-parent-collection',
      settledFailureCount: 1,
    })
    expect(byMilestone.get('attachment-rollback-failed')?.context).toMatchObject({
      backend: 'test',
      phase: 'remove-owned-staging',
      settledFailureCount: 1,
      settledFailuresTruncated: false,
    })
  })

  it('finalizes one parent-owned collection before ownership transfer', async () => {
    const staging = await preparedStaging()
    const collection = await staging.finalize()

    expect(collection.absoluteRoot).toBe(staging.childAttachmentRoot)
    expect(dirname(collection.absoluteRoot)).toBe(dirname(staging.sampleDirectory))
    expect(basename(collection.absoluteRoot))
      .toMatch(/^\.sample-1-child-attachments-.{6}$/u)
    expect(requireFinalizedArtifactCollectionRoot(
      staging.sampleDirectory,
      collection.absoluteRoot,
    )).toBe(collection.absoluteRoot)
    expect(() => requireFinalizedArtifactCollectionRoot(
      staging.sampleDirectory,
      join(staging.sampleDirectory, 'child-attachments'),
    )).toThrow(/private direct sibling/u)
    expect(await readdir(collection.absoluteRoot)).toEqual(['child', 'runner'])
    await expect(staging.commit()).resolves.toMatchObject({ failures: [] })
  })
})

async function preparedStaging(
  hooks: BrowserSampleStagingHooks = {},
): Promise<BrowserSampleStaging> {
  const workspace = await trackedWorkspace()
  const sampleDirectory = join(workspace, 'sample-1')
  await mkdir(sampleDirectory)
  const staging = await BrowserSampleStaging.create(sampleDirectory, hooks)
  await mkdir(staging.childPath('child'))
  await writeFile(staging.childPath('child', 'evidence.jsonl'), '', 'utf8')
  await writeFile(staging.runnerPath('stdout.log'), 'stdout', 'utf8')
  await writeFile(staging.runnerPath('stderr.log'), 'stderr', 'utf8')
  return staging
}

async function trackedWorkspace(): Promise<string> {
  const workspace = await mkdtemp(join(tmpdir(), 'windshare-staging-collection-'))
  workspaces.push(workspace)
  return workspace
}
