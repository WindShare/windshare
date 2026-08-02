import { readFile } from 'node:fs/promises'

import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import {
  requireCompleteArtifactGuardTrace,
  startScanSampleArtifacts,
  type ArtifactGuardTraceSnapshot,
  type ScanSampleArtifactsOptions,
} from '../../scripts/browser-evidence/artifact/guard.ts'
import {
  artifactRootForOutcome,
  createFrameworkWorkspace,
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

describe('artifact guard owned trace journal', () => {
  it('settles with exact immutable scan lifecycle evidence while a pull consumer stalls', async () => {
    const outcome = await syntheticOutcome()
    const execution = startScanSampleArtifacts(await scanOptions(outcome))
    const consumer = execution.traces[Symbol.asyncIterator]()

    await expect(consumer.next()).resolves.toMatchObject({
      done: false,
      value: {
        schemaVersion: 'windshare.artifact-guard-trace/v1',
        component: 'artifact-guard',
        scenario: 'artifact-scan',
        milestone: 'scan-started',
        outcome: 'started',
      },
    })

    const guard = await execution.result
    const snapshot = execution.traces.snapshot()
    requireCompleteArtifactGuardTrace(snapshot)

    expect(guard.guardOutcome).toBe('passed')
    expect(snapshot.events.filter(({ milestone }) => milestone === 'scan-started')).toHaveLength(1)
    expect(snapshot.events.filter(({ milestone }) => milestone === 'scan-terminal')).toHaveLength(1)
    expect(snapshot.events.at(-1)).toMatchObject({
      milestone: 'scan-terminal',
      outcome: 'succeeded',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'scan-artifacts-processed',
      },
    })
    expect(snapshot).toMatchObject({
      completed: true,
      truncated: false,
      failure: null,
      observedEvents: snapshot.capturedEvents,
      observedBytes: snapshot.capturedBytes,
    })
    expect(Object.isFrozen(snapshot)).toBe(true)
    expect(Object.isFrozen(snapshot.events)).toBe(true)
    expect(snapshot.events.every((event) =>
      Object.isFrozen(event) && Object.isFrozen(event.context))).toBe(true)
  })

  it('retains workflow progress when a declarative scanner failure cut still requires cleanup', async () => {
    const outcome = await syntheticOutcome()
    const options = await scanOptions(outcome)
    const target = outcome.result.artifacts[0]!.relativePath
    const execution = startScanSampleArtifacts({
      ...options,
      faultCut: {
        action: 'fail-before-artifact-scan',
        relativePath: target,
      },
    })

    const guard = await execution.result
    const snapshot = execution.traces.snapshot()
    requireCompleteArtifactGuardTrace(snapshot)

    expect(guard).toMatchObject({
      guardOutcome: 'failed',
      failureCode: 'scanner-crashed',
      uploadableArtifactIds: [],
    })
    expect(snapshot.events.at(-1)).toMatchObject({
      milestone: 'scan-terminal',
      outcome: 'failed',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'scan-fault-cut-reached',
      },
    })
  })

  it('rejects accessor and Proxy snapshots without inspecting them', () => {
    let inspected = false
    const accessorSnapshot = Object.create(null)
    Object.defineProperty(accessorSnapshot, 'completed', {
      enumerable: true,
      get: () => {
        inspected = true
        throw new Error('hostile trace getter executed')
      },
    })
    const proxySnapshot = new Proxy(Object.create(null), {
      get: () => {
        inspected = true
        throw new Error('hostile trace proxy getter executed')
      },
      ownKeys: () => {
        inspected = true
        throw new Error('hostile trace proxy ownKeys executed')
      },
      getOwnPropertyDescriptor: () => {
        inspected = true
        throw new Error('hostile trace descriptor trap executed')
      },
    })

    expect(() => requireCompleteArtifactGuardTrace(
      accessorSnapshot as ArtifactGuardTraceSnapshot,
    )).toThrow(/lifecycle owner/u)
    expect(() => requireCompleteArtifactGuardTrace(
      proxySnapshot as ArtifactGuardTraceSnapshot,
    )).toThrow(/lifecycle owner/u)
    expect(inspected).toBe(false)
  })

  it('never executes a structurally smuggled legacy scan callback', async () => {
    const outcome = await syntheticOutcome()
    let invoked = false
    const options: ScanSampleArtifactsOptions & {
      readonly beforeArtifactScan: () => void
    } = {
      ...await scanOptions(outcome),
      beforeArtifactScan: () => { invoked = true },
    }

    const execution = startScanSampleArtifacts(options)
    await expect(execution.result).resolves.toMatchObject({ guardOutcome: 'passed' })
    requireCompleteArtifactGuardTrace(execution.traces.snapshot())
    expect(invoked).toBe(false)
  })
})

async function syntheticOutcome() {
  const workspace = await createFrameworkWorkspace()
  workspaces.push(workspace)
  return runSyntheticSample({
    workspace,
    topology,
    suite: 'main',
    mode: 'main-unavailable',
  })
}

async function scanOptions(
  outcome: Awaited<ReturnType<typeof syntheticOutcome>>,
): Promise<ScanSampleArtifactsOptions> {
  return {
    sample: outcome.result,
    sampleResultBytes: await readFile(outcome.resultPath),
    artifactRoot: artifactRootForOutcome(outcome),
    explicitSecrets: [],
  }
}
