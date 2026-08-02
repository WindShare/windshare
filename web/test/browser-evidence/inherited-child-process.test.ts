import type { ChildProcess, SpawnOptions } from 'node:child_process'
import { EventEmitter } from 'node:events'
import { PassThrough } from 'node:stream'

import { describe, expect, it, vi } from 'vitest'

import {
  createInheritedChildProcessBackend,
  InheritedChildProcessError,
  type InheritedChildLaunchRequest,
} from '../../scripts/browser-evidence/process/inherited-child-process.mjs'
import {
  TEST_EVENT_SCHEMA_VERSION,
} from '../../scripts/browser-evidence/process/test-event-channel.mjs'

const TEST_IDENTITY = Object.freeze({
  runId: 'inherited-child-run',
  operationId: 'inherited-child-operation',
  scenario: 'inherited-child-contract',
})

describe('inherited child process backend', () => {
  it('gives only the target child a private fd3 event capability', async () => {
    const child = fakeChild(true)
    let spawnOptions: SpawnOptions | undefined
    const backend = createInheritedChildProcessBackend({
      platform: 'win32',
      spawnProcess: (_executable, _arguments, options) => {
        spawnOptions = options
        return child.process
      },
    })
    const session = backend.launch(launchRequest({
      events: Object.freeze({
        minimumEvents: 1,
        maximumEvents: 1,
      }),
    }))
    const readyEvent = session.events[Symbol.asyncIterator]().next()

    expect(spawnOptions?.detached).toBe(false)
    expect(spawnOptions?.stdio).toEqual(['ignore', 'pipe', 'pipe', 'pipe'])
    expect(spawnOptions?.env).toMatchObject({
      WINDSHARE_TEST_EVENT_FD: '3',
      WINDSHARE_TEST_RUN_ID: TEST_IDENTITY.runId,
      WINDSHARE_TEST_OPERATION_ID: TEST_IDENTITY.operationId,
      WINDSHARE_TEST_SCENARIO: TEST_IDENTITY.scenario,
    })
    expect(spawnOptions?.env).not.toHaveProperty('WINDSHARE_TEST_EVENT_HANDLE')

    child.event!.end(encodedEvent())
    child.stdout.end()
    child.stderr.end()
    child.exit(0, null)
    await expect(session.terminal).resolves.toEqual({ terminal: 'exited', exitCode: 0 })
    const execution = await session.completion
    await expect(readyEvent).resolves.toMatchObject({ done: false, value: {
      component: 'probe',
      milestone: 'listener_ready',
      outcome: 'succeeded',
      payload: { address: '127.0.0.1:49152' },
    } })
    expect(execution.events.events).toMatchObject([{
      component: 'probe',
      milestone: 'listener_ready',
      outcome: 'succeeded',
    }])
  })

  it('reports spawn failure but does not complete until every reader and close join', async () => {
    const child = fakeChild(false)
    const backend = createInheritedChildProcessBackend({
      platform: 'linux',
      spawnProcess: () => child.process,
    })
    const session = backend.launch(launchRequest())
    const firstOutput = session.stdout[Symbol.asyncIterator]().next()
    let completionSettled = false
    session.completion.finally(() => { completionSettled = true }).catch(() => undefined)

    child.stdout.write('diagnostic tail')
    const firstChunk = await firstOutput
    expect(firstChunk.done).toBe(false)
    expect(Buffer.from(firstChunk.value ?? []).toString('utf8')).toBe('diagnostic tail')
    child.spawnFailure('ENOENT', 'fixture executable is absent')
    await expect(session.terminal).resolves.toEqual({
      terminal: 'spawn-failed',
      errorCode: 'ENOENT',
      errorMessage: 'fixture executable is absent',
    })
    await Promise.resolve()
    expect(completionSettled).toBe(false)

    child.stdout.end()
    child.stderr.end()
    child.close(null, null)
    const execution = await session.completion
    expect(Buffer.from(execution.output.stdout.bytes()).toString('utf8')).toBe('diagnostic tail')
  })

  it('requests SIGTERM once and leaves hard retirement to the outer owner', async () => {
    const child = fakeChild(false)
    const backend = createInheritedChildProcessBackend({
      platform: 'linux',
      spawnProcess: () => child.process,
    })
    const session = backend.launch(launchRequest())

    session.requestStop()
    session.requestStop()
    expect(child.kill).toHaveBeenCalledTimes(1)
    expect(child.kill).toHaveBeenCalledWith('SIGTERM')

    child.stdout.end()
    child.stderr.end()
    child.exit(null, 'SIGTERM')
    await expect(session.completion).resolves.toMatchObject({
      terminal: { terminal: 'signaled', signal: 'SIGTERM' },
    })
  })

  it('drains overflow to EOF and preserves terminal evidence with bounded snapshots', async () => {
    const child = fakeChild(false)
    const backend = createInheritedChildProcessBackend({
      platform: 'linux',
      spawnProcess: () => child.process,
    })
    const session = backend.launch(launchRequest({
      capture: Object.freeze({ stdoutBytes: 5, stderrBytes: 16 }),
    }))

    child.stdout.end('overflow-output')
    child.stderr.end()
    child.exit(0, null)
    let failure: unknown
    try {
      await session.completion
    } catch (cause) {
      failure = cause
    }

    expect(failure).toBeInstanceOf(InheritedChildProcessError)
    if (!(failure instanceof InheritedChildProcessError)) throw failure
    expect(failure.terminal).toEqual({ terminal: 'exited', exitCode: 0 })
    expect(failure.output.stdout).toMatchObject({
      observedBytes: 15,
      capturedBytes: 5,
      truncated: true,
      completed: true,
    })
    expect(Buffer.from(failure.output.stdout.bytes()).toString('utf8')).toBe('overf')
  })

  it('rejects an outer event endpoint even when its environment name changes case', () => {
    const backend = createInheritedChildProcessBackend({
      platform: 'win32',
      spawnProcess: () => fakeChild(false).process,
    })
    expect(() => backend.launch(launchRequest({
      environment: Object.freeze({ windshare_test_event_handle: 'outer-handle' }),
    }))).toThrow('attempts to reuse an outer event capability')
  })
})

function launchRequest(
  replacement: Partial<InheritedChildLaunchRequest> = {},
): InheritedChildLaunchRequest {
  return Object.freeze({
    identity: TEST_IDENTITY,
    command: Object.freeze({
      executable: process.execPath,
      arguments: Object.freeze([]),
      cwd: process.cwd(),
    }),
    environment: Object.freeze({}),
    capture: Object.freeze({ stdoutBytes: 4_194_304, stderrBytes: 4_194_304 }),
    ...replacement,
  })
}

function encodedEvent(): Buffer {
  return Buffer.from(`${JSON.stringify({
    schema_version: TEST_EVENT_SCHEMA_VERSION,
    run_id: TEST_IDENTITY.runId,
    operation_id: TEST_IDENTITY.operationId,
    scenario: TEST_IDENTITY.scenario,
    component: 'probe',
    milestone: 'listener_ready',
    outcome: 'succeeded',
    payload: { address: '127.0.0.1:49152' },
  })}\n`)
}

function fakeChild(withEvent: boolean): {
  readonly process: ChildProcess
  readonly stdout: PassThrough
  readonly stderr: PassThrough
  readonly event: PassThrough | undefined
  readonly kill: ReturnType<typeof vi.fn>
  exit(code: number | null, signal: NodeJS.Signals | null): void
  close(code: number | null, signal: NodeJS.Signals | null): void
  spawnFailure(code: string, message: string): void
} {
  const emitter = new EventEmitter()
  const stdout = new PassThrough()
  const stderr = new PassThrough()
  const event = withEvent ? new PassThrough() : undefined
  const kill = vi.fn(() => true)
  const processState = Object.assign(emitter, {
    stdout,
    stderr,
    stdio: [null, stdout, stderr, ...(event === undefined ? [] : [event])],
    exitCode: null as number | null,
    signalCode: null as NodeJS.Signals | null,
    kill,
  })
  const process_ = processState as unknown as ChildProcess
  const close = (code: number | null, signal: NodeJS.Signals | null) => {
    emitter.emit('close', code, signal)
  }
  return Object.freeze({
    process: process_,
    stdout,
    stderr,
    event,
    kill,
    exit(code: number | null, signal: NodeJS.Signals | null) {
      processState.exitCode = code
      processState.signalCode = signal
      emitter.emit('exit', code, signal)
      close(code, signal)
    },
    close,
    spawnFailure(code: string, message: string) {
      const cause = Object.assign(new Error(message), { code })
      emitter.emit('error', cause)
    },
  })
}
