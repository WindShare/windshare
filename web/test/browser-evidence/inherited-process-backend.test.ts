import { type ChildProcess, spawn as nodeSpawn } from 'node:child_process'
import { EventEmitter } from 'node:events'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PassThrough } from 'node:stream'

import { describe, expect, it, vi } from 'vitest'

import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'
import {
  BrowserSampleContainmentError,
  type BrowserSampleContainmentRequest,
} from '../../scripts/browser-evidence/process/containment.ts'
import { createInheritedProcessContainmentBackend } from '../../scripts/browser-evidence/process/inherited-process-backend.ts'

describe('inherited process containment channels', () => {
  it('rejects anonymous stdin close before writable finish', async () => {
    const child = fakeChild(true)
    const backend = inheritedBackend(child.process)
    const execution = backend.execute(request(Buffer.from('private input', 'utf8'), 64))

    child.exit(0, null)
    const failure = await rejectedContainment(execution)

    expect((failure.cause as Error & { cause?: Error }).cause?.message)
      .toContain('closed before its bytes finished')
    expect(failure.output?.stdout).toMatchObject({ completed: true, truncated: false })
    expect(failure.traces.events.at(-1)).toMatchObject({ milestone: 'inherited-leaf-failed' })
  })

  it('drains but fails closed when owned output exceeds its capture authority', async () => {
    const child = fakeChild(false)
    const backend = inheritedBackend(child.process)
    const execution = backend.execute(request(undefined, 4))

    child.stdout.write(Buffer.from('overflow', 'utf8'))
    child.exit(0, null)
    const failure = await rejectedContainment(execution)

    expect(failure.output?.stdout).toMatchObject({
      observedBytes: 8,
      capturedBytes: 4,
      truncated: true,
      completed: true,
    })
    expect(Buffer.from(failure.output?.stdout.bytes() ?? []).toString('utf8')).toBe('over')
    expect((failure.cause as Error).message).toContain('capture authority')
    expect(failure.traces.events.at(-1)).toMatchObject({ milestone: 'inherited-leaf-failed' })
  })
})

function inheritedBackend(child: ChildProcess) {
  return createInheritedProcessContainmentBackend(
    Object.freeze({
      kind: 'test-process-owner' as const,
      backend: 'linux_subreaper' as const,
      operationId: 'inherited-backend-operation',
    }),
    vi.fn(() => child) as unknown as typeof nodeSpawn,
  )
}

function request(
  stdin: Uint8Array | undefined,
  stdoutBytes: number,
): BrowserSampleContainmentRequest {
  const operationId = 'inherited-backend-operation'
  return Object.freeze({
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
      ...(stdin === undefined ? {} : { stdin }),
    }),
    sampleDirectory: join(tmpdir(), 'unused-sample'),
    childAttachmentStagingRoot: join(tmpdir(), 'unused-attachments'),
    childContext: Object.freeze({
      runId: 'inherited-backend-run',
      operationId,
      scenario: 'inherited-backend-contract',
      runPolicy: browserRunPolicy('blocking'),
      suite: 'main' as const,
      browser: 'chromium' as const,
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
    terminationGraceMs: 100,
    capture: Object.freeze({ stdoutBytes, stderrBytes: 64 }),
  })
}

async function rejectedContainment(
  execution: Promise<unknown>,
): Promise<BrowserSampleContainmentError> {
  let failure: unknown
  try {
    await execution
  } catch (cause) {
    failure = cause
  }
  expect(failure).toBeInstanceOf(BrowserSampleContainmentError)
  if (!(failure instanceof BrowserSampleContainmentError)) throw failure
  return failure
}

function fakeChild(prematureStdinClose: boolean) {
  const events = new EventEmitter()
  const stdout = new PassThrough()
  const stderr = new PassThrough()
  const stdin = new PassThrough()
  if (prematureStdinClose) {
    stdin.end = (() => {
      stdin.destroy()
      return stdin
    }) as typeof stdin.end
  }
  const state = Object.assign(events, {
    stdin,
    stdout,
    stderr,
    stdio: [stdin, stdout, stderr],
    exitCode: null as number | null,
    signalCode: null as NodeJS.Signals | null,
    kill: vi.fn(() => true),
  })
  return Object.freeze({
    process: state as unknown as ChildProcess,
    stdout,
    exit(code: number | null, signal: NodeJS.Signals | null) {
      state.exitCode = code
      state.signalCode = signal
      events.emit('exit', code, signal)
    },
  })
}
