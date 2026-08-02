import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

import { BrowserSampleContainmentError } from '../../scripts/browser-evidence/process/containment.ts'
import { createTestProcessOwnerContainmentBackend } from '../../scripts/browser-evidence/process/test-process-owner-backend.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'

describe('test process owner containment terminal tracing', () => {
  it('emits structured failure evidence when owner transport fails before settlement', async () => {
    const operationId = 'owner-backend-failure'
    const backend = createTestProcessOwnerContainmentBackend(
      Object.freeze({ path: join(tmpdir(), 'windshare-owner-does-not-exist') }),
      process.platform === 'win32' ? 'win32' : 'linux',
    )

    let failure: unknown
    try {
      await backend.execute({
        operationId,
        topologyProfilePath: join(tmpdir(), 'unused-profile.json'),
        topologyProfileSha256: '0'.repeat(64),
        topologyResolutionPath: join(tmpdir(), 'unused-resolution.json'),
        topologyResolutionSha256: '1'.repeat(64),
        readOnlyInputRoots: Object.freeze([]),
        command: Object.freeze({
          executable: process.execPath,
          arguments: Object.freeze(['-e', 'process.exit(0)']),
          cwd: process.cwd(),
        }),
        sampleDirectory: join(tmpdir(), 'unused-sample'),
        childAttachmentStagingRoot: join(tmpdir(), 'unused-attachments'),
        childContext: Object.freeze({
          runId: 'owner-backend-run',
          operationId,
          scenario: 'owner-backend-contract',
          runPolicy: browserRunPolicy('blocking'),
          suite: 'main',
          browser: 'chromium',
          sampleIndex: 1,
          checkoutSha: '2'.repeat(40),
          topologyProfileSha256: '0'.repeat(64),
          topologyResolutionSha256: '1'.repeat(64),
          topologyProfilePath: join(tmpdir(), 'unused-profile.json'),
          topologyResolutionPath: join(tmpdir(), 'unused-resolution.json'),
          evidencePath: join(tmpdir(), 'unused-evidence.jsonl'),
          artifactRoot: join(tmpdir(), 'unused-artifacts'),
        }),
        deadlineMs: 1_000,
        terminationGraceMs: 1_000,
        capture: Object.freeze({ stdoutBytes: 1024, stderrBytes: 1024 }),
      })
    } catch (cause) {
      failure = cause
    }

    expect(failure).toBeInstanceOf(BrowserSampleContainmentError)
    if (!(failure instanceof BrowserSampleContainmentError)) throw failure
    // Artifact validation occurs before a child owns output capabilities, so
    // this failure can carry terminal trace evidence without inventing bytes.
    expect(failure.output).toBeUndefined()
    expect(failure.traces).toMatchObject({ completed: true, truncated: false })
    expect(failure.traces.events).toEqual([{
      milestone: 'test-process-owner-failed',
      outcome: 'failed',
      context: expect.objectContaining({
        runId: 'owner-backend-run',
        operationId,
        scenario: 'owner-backend-contract',
        backend: process.platform === 'win32' ? 'windows_job' : 'linux_subreaper',
        terminal: 'unavailable',
        treeEmpty: false,
        cleanupOutcome: 'unavailable',
        terminationReason: 'unavailable',
        failure: expect.any(String),
      }),
    }])
  })
})
