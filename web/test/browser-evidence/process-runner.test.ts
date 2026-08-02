import { access, readFile, readdir, stat } from 'node:fs/promises'
import { join } from 'node:path'

import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import { writeAtomicJson } from '../../scripts/browser-evidence/contract/atomic-json.ts'
import {
  CHILD_EVIDENCE_CONTEXT_ENV,
  readChildEvidenceContext,
} from '../../scripts/browser-evidence/child-evidence.ts'
import {
  startBrowserSample,
} from '../../scripts/browser-evidence/sample-runner.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'
import {
  BrowserSampleContainmentError,
  type BrowserSampleContainmentBackend,
  type BrowserSampleContainmentRequest,
} from '../../scripts/browser-evidence/process/containment.ts'
import { createProcessTestContainmentBackend } from './process-test-containment.ts'
import {
  createFrameworkWorkspace,
  FRAMEWORK_CHECKOUT_SHA,
  FRAMEWORK_RUN_ID,
  artifactRootForOutcome,
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  runSyntheticSample as runFrameworkSyntheticSample,
  startSyntheticSample as startFrameworkSyntheticSample,
  type FrameworkTopology,
  type SyntheticSampleOptions,
} from './framework-fixtures.ts'
import {
  runNativeOwnedSyntheticSample,
  startNativeOwnedSyntheticSample,
} from './process-owner-fixtures.ts'

let topology: FrameworkTopology
const workspaces: string[] = []
const nativeOwnerIt = process.platform === 'win32' || process.platform === 'linux' ? it : it.skip

beforeAll(async () => { topology = await loadFrameworkTopology() })
afterEach(async () => {
  await Promise.all(workspaces.splice(0).map(removeFrameworkWorkspace))
})

describe('process-isolated browser evidence runner', () => {
  it('strictly parses the injected child authority instead of accepting duplicate JSON members', () => {
    expect(() => readChildEvidenceContext({
      [CHILD_EVIDENCE_CONTEXT_ENV]: '{"runId":"first","runId":"second"}',
    })).toThrow(/duplicate object member/u)
    expect(() => readChildEvidenceContext({
      [CHILD_EVIDENCE_CONTEXT_ENV]: JSON.stringify({
        runId: FRAMEWORK_RUN_ID,
        operationId: 'main-chromium-sample-4',
        scenario: 'synthetic-browser-evidence',
        runPolicy: browserRunPolicy('closure'),
        suite: 'main',
        browser: 'chromium',
        sampleIndex: 4,
        checkoutSha: FRAMEWORK_CHECKOUT_SHA,
        topologyProfileSha256: 'a'.repeat(64),
        topologyResolutionSha256: 'b'.repeat(64),
        topologyProfilePath: 'C:\\profile.json',
        topologyResolutionPath: 'C:\\resolution.json',
        evidencePath: 'C:\\evidence.jsonl',
        artifactRoot: 'C:\\attachments',
      }),
    })).toThrow(/\[1, 3\]/u)
  })

  it('leaves a valid provisional result before spawn completes and atomically replaces it once', async () => {
    const workspace = await trackedWorkspace()
    const running = runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      delayMs: 250,
    })
    const resultPath = join(workspace, 'main', 'chromium', 'sample-1', 'result.json')
    await waitForPath(resultPath)
    const provisional = JSON.parse(await readFile(resultPath, 'utf8')) as Record<string, unknown>
    expect(provisional).toMatchObject({
      resultStatus: 'provisional',
      rtcCapability: 'unknown',
      peerAttemptOutcome: 'not-started',
      deliveryOutcome: 'not-started',
      executionOutcome: 'unknown',
    })

    const outcome = await running
    expect(outcome.acceptedBeforeGuard).toBe(true)
    expect(outcome.result).toMatchObject({
      resultStatus: 'final-valid',
      rtcCapability: 'available',
      peerAttemptOutcome: 'admitted',
      deliveryOutcome: 'succeeded',
      executionOutcome: 'healthy',
      playwrightOutcome: 'passed',
    })
    const persisted = JSON.parse(await readFile(resultPath, 'utf8')) as Record<string, unknown>
    expect(persisted).toMatchObject({ resultStatus: 'final-valid' })

    const replacementPath = join(workspace, 'windows-atomic.json')
    await writeAtomicJson(replacementPath, { generation: 1 })
    await writeAtomicJson(replacementPath, { generation: 2 })
    expect(JSON.parse(await readFile(replacementPath, 'utf8'))).toEqual({ generation: 2 })
  })

  it('preserves the maximum legal run identity across the process authority boundary', async () => {
    const workspace = await trackedWorkspace()
    const runId = 'r'.repeat(128)
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      runId,
      suite: 'main',
      mode: 'main-pass',
    })

    expect(outcome.result).toMatchObject({
      runId,
      resultStatus: 'final-valid',
    })
  })

  it('records spawn failure without inventing capability, attempt, delivery, or crash evidence', async () => {
    const workspace = await trackedWorkspace()
    const execution = startBrowserSample({
      runId: FRAMEWORK_RUN_ID,
      operationId: 'main-chromium-sample-1',
      scenario: 'synthetic-browser-evidence',
      runPolicy: browserRunPolicy('closure'),
      suite: 'main',
      browser: 'chromium',
      sampleIndex: 1,
      checkoutSha: FRAMEWORK_CHECKOUT_SHA,
      sampleDirectory: join(workspace, 'main', 'chromium', 'sample-1'),
      topologyLock: topology.lock,
      topologyProfilePath: topology.profilePath,
      topologyResolutionPath: topology.resolutionPath,
      command: {
        executable: join(workspace, 'missing-playwright-child.exe'),
        arguments: [],
      },
      // This process-gate contract owns spawn-failure classification without
      // selecting a platform production backend.
      containmentBackend: createProcessTestContainmentBackend(),
    })
    const outcome = await execution.result
    expect(execution.traces.snapshot()).toMatchObject({
      completed: true,
      truncated: false,
      observedEvents: execution.traces.snapshot().capturedEvents,
    })
    expect(outcome.result).toMatchObject({
      resultStatus: 'final-invalid',
      rtcCapability: 'unknown',
      peerAttemptOutcome: 'not-started',
      deliveryOutcome: 'not-started',
      executionOutcome: 'infrastructure-failed',
      playwrightOutcome: 'not-started',
    })
    expect(outcome.result.executionEvidence).toMatchObject({
      pageCrashed: false,
      targetCrashed: false,
      runnerProcess: { terminal: 'spawn-failed' },
    })
  })

  it('uses public crash events as crash authority before probe or delivery begins', async () => {
    const workspace = await trackedWorkspace()
    const pageCrash = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      sampleIndex: 1,
      mode: 'crash-before-probe',
    })
    const targetCrash = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      sampleIndex: 2,
      mode: 'target-crash',
    })
    for (const outcome of [pageCrash, targetCrash]) {
      expect(outcome.result).toMatchObject({
        resultStatus: 'final-valid',
        rtcCapability: 'unknown',
        peerAttemptOutcome: 'not-started',
        deliveryOutcome: 'not-started',
        executionOutcome: 'crashed',
        playwrightOutcome: 'failed',
      })
    }
    expect(pageCrash.result.executionEvidence.pageCrashed).toBe(true)
    expect(targetCrash.result.executionEvidence.targetCrashed).toBe(true)
  })

  it('fails closed on missing terminals, truncated JSONL, and bounded process output loss', async () => {
    const workspace = await trackedWorkspace()
    const missingTerminal = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      sampleIndex: 1,
      mode: 'missing-terminal',
    })
    const truncated = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      sampleIndex: 2,
      mode: 'truncated-event',
    })
    const outputLoss = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      sampleIndex: 3,
      mode: 'output-overflow',
      maximumCapturedStreamBytes: 64,
    })
    expect(missingTerminal.result.integrityViolations.join(' ')).toMatch(/no terminal/u)
    expect(truncated.result.integrityViolations.join(' ')).toMatch(/truncated event/u)
    expect(outputLoss.result.integrityViolations.join(' ')).toMatch(/stdout.*truncated/u)
    expect(outputLoss.result).toMatchObject({
      resultStatus: 'final-invalid',
      peerAttemptOutcome: 'admitted',
      deliveryOutcome: 'succeeded',
      routeEvidence: { mode: 'hot-switch' },
    })
    if (outputLoss.result.suite !== 'main') throw new Error('synthetic output-loss sample changed suites')
    expect(outputLoss.result.attempts).toHaveLength(1)
    expect(await stat(join(artifactRootForOutcome(outputLoss), 'runner', 'stdout.log')))
      .toMatchObject({ size: 64 })
  })

  it('keeps completed attempts and independent delivery facts in a final-invalid sample', async () => {
    const workspace = await trackedWorkspace()
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-valid-with-incomplete-attempt',
    })
    expect(outcome.result).toMatchObject({
      resultStatus: 'final-invalid',
      peerAttemptOutcome: 'admitted',
      deliveryOutcome: 'succeeded',
      routeEvidence: { mode: 'hot-switch' },
    })
    if (outcome.result.suite !== 'main') throw new Error('synthetic preservation sample changed suites')
    expect(outcome.result.attempts).toHaveLength(1)
    expect(outcome.result.attempts[0]?.events[0]?.receiveSequence).toBe(2)
    expect(outcome.result.integrityViolations.join(' ')).toMatch(/no terminal/u)
  })
})

describe('process-isolated browser evidence containment boundary', () => {
  it('terminates a child at the process deadline and still publishes a terminal result', async () => {
    const workspace = await trackedWorkspace()
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      delayMs: 30_000,
      processDeadlineMs: 50,
    })
    expect(outcome.result).toMatchObject({
      resultStatus: 'final-invalid',
      rtcCapability: 'unknown',
      peerAttemptOutcome: 'not-started',
      deliveryOutcome: 'not-started',
      executionOutcome: 'infrastructure-failed',
      playwrightOutcome: 'failed',
    })
    expect(outcome.result.integrityViolations.join(' ')).toMatch(/process deadline/u)
    expect(JSON.parse(await readFile(outcome.resultPath, 'utf8'))).toMatchObject({
      resultStatus: 'final-invalid',
    })
  })

  nativeOwnerIt('terminates the timed-out process tree before publishing its terminal result', async () => {
    const workspace = await trackedWorkspace()
    const execution = await startNativeOwnedSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'descendant-timeout',
      delayMs: 30_000,
      processDeadlineMs: 100,
    })
    const outcome = await execution.result
    const traces = execution.traces.snapshot().events
    await new Promise((resolve) => setTimeout(resolve, 800))
    expect(JSON.parse(await readFile(outcome.resultPath, 'utf8'))).toMatchObject({
      resultStatus: 'final-invalid',
    })
    await expect(access(join(artifactRootForOutcome(outcome), 'descendant-ran.txt'))).rejects.toThrow()
    expect(traces.find(({ milestone }) => milestone === 'containment-preflight-started'))
      .toMatchObject({ context: { backend: 'test-process-owner' } })
  })

  nativeOwnerIt('waits for its owner-held detached descendant before replacing provisional', async () => {
    const workspace = await trackedWorkspace()
    const outcome = await runNativeOwnedSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'descendant-after-root',
      processDeadlineMs: 5_000,
    })

    await expect(readFile(join(artifactRootForOutcome(outcome), 'descendant-ran.txt'), 'utf8'))
      .resolves.toBe('ran')
    await expect(readFile(outcome.resultPath, 'utf8')).resolves.not.toContain('lateWriter')
    expect(outcome.result).toMatchObject({
      resultStatus: 'final-valid',
      executionOutcome: 'healthy',
      playwrightOutcome: 'passed',
    })
  })

  it('restores the exact provisional snapshot after containment authority fails', async () => {
    const workspace = await trackedWorkspace()
    const resultPath = join(workspace, 'main', 'chromium', 'sample-1', 'result.json')
    let initialProvisional = ''
    const failingContainment: BrowserSampleContainmentBackend = Object.freeze({
      kind: 'test',
      preflight: async () => undefined,
      execute: async (request: BrowserSampleContainmentRequest) => {
        initialProvisional = await readFile(resultPath, 'utf8')
        await writeAtomicJson(resultPath, { lateWriter: true })
        await writeAtomicJson(
          join(request.childAttachmentStagingRoot, 'spoofed-child-artifact.json'),
          { spoofed: true },
        )
        throw new Error('synthetic containment authority failure')
      },
    })

    await expect(runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      containmentBackend: failingContainment,
    })).rejects.toThrow(/synthetic containment authority failure/u)

    expect(initialProvisional).not.toBe('')
    await expect(readFile(resultPath, 'utf8')).resolves.toBe(initialProvisional)
    expect(JSON.parse(initialProvisional)).toMatchObject({
      resultStatus: 'provisional',
      rtcCapability: 'unknown',
      peerAttemptOutcome: 'not-started',
    })
    expect((await readdir(join(workspace, 'main', 'chromium')))
      .filter((name) => name.includes('child-attachments'))).toEqual([])
  })

  it('preserves the deadline workflow milestone when containment cleanup also fails', async () => {
    const workspace = await trackedWorkspace()
    const containmentFailure = new Error('synthetic containment deadline cleanup failure')
    const failingContainment: BrowserSampleContainmentBackend = Object.freeze({
      kind: 'test',
      preflight: async () => undefined,
      execute: async () => {
        throw new BrowserSampleContainmentError(
          containmentFailure.message,
          Object.freeze({
            events: Object.freeze([
              Object.freeze({
                milestone: 'test-process-owner-failed',
                outcome: 'failed' as const,
                context: Object.freeze({
                  terminationReason: 'deadline',
                  cleanupOutcome: 'failed',
                }),
              }),
            ]),
            observedEvents: 1,
            capturedEvents: 1,
            truncated: false,
            completed: true,
          }),
          undefined,
          containmentFailure,
        )
      },
    })

    const execution = startSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
      containmentBackend: failingContainment,
    })
    await expect(execution.result).rejects.toThrow(containmentFailure.message)

    const snapshot = execution.traces.snapshot()
    expect(snapshot).toMatchObject({
      completed: true,
      truncated: false,
      observedEvents: snapshot.capturedEvents,
    })
    expect(snapshot.events.find(({ milestone }) => milestone === 'operation-terminal'))
      .toMatchObject({
        outcome: 'failed',
        context: {
          cleanupOutcome: 'failed',
          lastMilestone: 'containment-test-process-owner-failed',
        },
      })
    expect(snapshot.events.map(({ milestone }) => milestone)).toEqual(expect.arrayContaining([
      'containment-test-process-owner-failed',
      'attachment-rollback-completed',
    ]))
  })

  it('captures stdout, stderr, and nonzero assertion exits without misclassifying healthy execution', async () => {
    const workspace = await trackedWorkspace()
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'exit-assertion',
    })
    expect(outcome.result).toMatchObject({
      resultStatus: 'final-valid',
      executionOutcome: 'healthy',
      playwrightOutcome: 'failed',
    })
    const stdout = await readFile(join(artifactRootForOutcome(outcome), 'runner', 'stdout.log'), 'utf8')
    const stderr = await readFile(join(artifactRootForOutcome(outcome), 'runner', 'stderr.log'), 'utf8')
    expect(stdout).toContain('exit-assertion:stdout')
    expect(stderr).toContain('exit-assertion:stderr')
    expect(outcome.result.artifacts.map(({ kind }) => kind)).toEqual(expect.arrayContaining([
      'runner-stdout',
      'runner-stderr',
      'attempt-evidence',
      'result-diagnostic',
    ]))
  })

  it('keeps result authority independent from an unconsumed trace journal', async () => {
    const workspace = await trackedWorkspace()
    const execution = startSyntheticSample({
      workspace,
      topology,
      suite: 'main',
      mode: 'main-pass',
    })
    const outcome = await execution.result
    expect(outcome.acceptedBeforeGuard).toBe(true)
    expect(execution.traces.snapshot()).toMatchObject({ completed: true, truncated: false })
    expect(outcome.result.resultStatus).toBe('final-valid')
    expect(JSON.parse(await readFile(outcome.resultPath, 'utf8'))).toMatchObject({
      resultStatus: 'final-valid',
    })
  })
})

async function trackedWorkspace(): Promise<string> {
  const workspace = await createFrameworkWorkspace()
  workspaces.push(workspace)
  return workspace
}

function runSyntheticSample(options: SyntheticSampleOptions) {
  return runFrameworkSyntheticSample({
    ...options,
    containmentBackend: options.containmentBackend ?? createProcessTestContainmentBackend(),
  })
}

function startSyntheticSample(options: SyntheticSampleOptions) {
  return startFrameworkSyntheticSample({
    ...options,
    containmentBackend: options.containmentBackend ?? createProcessTestContainmentBackend(),
  })
}

async function waitForPath(path: string): Promise<void> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    try {
      await access(path)
      return
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 10))
    }
  }
  throw new Error(`timed out waiting for ${path}`)
}
