import { mkdir, mkdtemp, readFile, readdir, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { basename, dirname, join } from 'node:path'

import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import {
  BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE,
  BrowserSampleStaging,
  requireFinalizedArtifactCollectionRoot,
  type BrowserSampleStagingFaultCut,
} from '../../scripts/browser-evidence/process/attachment-staging.ts'
import { createDeterministicTestContainmentBackend } from './deterministic-containment.ts'
import {
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  startSyntheticSample,
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
    const staging = await preparedStaging('replace-root-before-finalize')
    const ownedBackup =
      `${staging.childAttachmentRoot}${BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.backupSuffix}`
    await expect(staging.finalize())
      .rejects.toThrow(/owner-held directory/u)
    await expect(staging.dispose()).rejects.toThrow(/cleanup did not fully settle/u)

    await expect(readFile(
      join(staging.childAttachmentRoot, BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.markerName),
      'utf8',
    )).resolves.toBe(BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.markerText)
    await expect(readFile(join(ownedBackup, 'child', 'evidence.jsonl'), 'utf8'))
      .resolves.toBe('')
  })

  it('never deletes a foreign directory swapped in immediately before rollback', async () => {
    const staging = await preparedStaging('replace-root-before-rollback')
    const ownedBackup =
      `${staging.childAttachmentRoot}${BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.backupSuffix}`

    await expect(staging.dispose()).rejects.toThrow(/cleanup did not fully settle/u)
    await expect(readFile(
      join(staging.childAttachmentRoot, BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.markerName),
      'utf8',
    )).resolves.toBe(BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.markerText)
    await expect(readFile(join(ownedBackup, 'child', 'evidence.jsonl'), 'utf8'))
      .resolves.toBe('')
  })

  it('restores provisional authority and removes a partial collection after finalization crashes', async () => {
    const workspace = await trackedWorkspace()
    const execution = startSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      containmentBackend: createDeterministicTestContainmentBackend(),
      stagingFaultCut: 'fail-after-finalize',
    })
    await expect(execution.result).rejects.toThrow(/declarative staging failure/u)
    const traces = execution.traces.snapshot().events

    const resultPath = join(workspace, 'main', 'chromium', 'sample-1', 'result.json')
    expect(JSON.parse(await readFile(resultPath, 'utf8'))).toMatchObject({
      resultStatus: 'provisional',
      artifacts: [],
    })
    const sampleParent = join(workspace, 'main', 'chromium')
    expect((await readdir(sampleParent)).filter((name) =>
      name.includes('child-attachments'))).toEqual([])

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
      .toContain('declarative staging failure after finalization')
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
    const execution = startSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      containmentBackend: createDeterministicTestContainmentBackend(),
      stagingFaultCut: 'replace-root-and-fail-after-finalize',
    })
    await expect(execution.result).rejects.toThrow(/staging rollback both failed/u)
    const traces = execution.traces.snapshot().events
    const sampleParent = join(workspace, 'main', 'chromium')
    const roots = (await readdir(sampleParent)).filter((name) =>
      name.startsWith('.sample-1-child-attachments-'))
    const replacementName = roots.find((name) =>
      !name.endsWith(BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.backupSuffix))
    if (replacementName === undefined) throw new Error('declarative replacement root is absent')
    const replacementRoot = join(sampleParent, replacementName)
    const ownedBackup =
      `${replacementRoot}${BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.backupSuffix}`

    await expect(readFile(
      join(replacementRoot, BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.markerName),
      'utf8',
    )).resolves.toBe(BROWSER_SAMPLE_STAGING_FAULT_EVIDENCE.markerText)
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
  faultCut?: BrowserSampleStagingFaultCut,
): Promise<BrowserSampleStaging> {
  const workspace = await trackedWorkspace()
  const sampleDirectory = join(workspace, 'sample-1')
  await mkdir(sampleDirectory)
  const staging = await BrowserSampleStaging.create(sampleDirectory, faultCut)
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
