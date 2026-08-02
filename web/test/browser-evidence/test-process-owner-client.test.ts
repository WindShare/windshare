import { execFile } from 'node:child_process'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import {
  encodeTestProcessOwnerStartDecisionFrame,
  executeTestProcessOwner,
  parseTestProcessOwnerSettlementForRequest,
  parseTestProcessOwnerStartEvidenceFrameForRequest,
  startTestProcessOwner,
  testProcessOwnerFailureEvidence,
  TestProcessOwnerControlError,
  TestProcessOwnerDeadlineError,
  TestProcessOwnerTransportError,
  type TestProcessOwnerArtifact,
  type TestProcessOwnerEvent,
  type TestProcessOwnerExecution,
} from '../../scripts/browser-evidence/process/test-process-owner-client.mjs'
import { loadFrameworkProcessOwner } from './process-owner-fixtures.ts'

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL('../../../', import.meta.url)))
const execFileAsync = promisify(execFile)
const nativeOwnerDescribe = process.platform === 'win32' || process.platform === 'linux'
  ? describe
  : describe.skip
const windowsOwnerIt = process.platform === 'win32' ? it : it.skip
const linuxOwnerIt = process.platform === 'linux' ? it : it.skip

interface SharedStartGateVectors {
  readonly schema_version: string
  readonly vectors: readonly SharedStartGateVector[]
}

interface SharedStartGateVector {
  readonly name: 'windows' | 'linux'
  readonly evidence: Readonly<{
    readonly schema_version: string
    readonly run_id: string
    readonly operation_id: string
    readonly scenario: string
    readonly platform: 'windows_job' | 'linux_subreaper'
    readonly process_id: number
    readonly process_instance: string
    readonly executable: Readonly<{ readonly volume: string; readonly object: string }>
  }>
  readonly accepted_decision: unknown
  readonly rejected_decision: Readonly<{
    readonly failure_code: string
    readonly failure_message: string
  }>
  readonly rejected_settlement: unknown
  readonly evidence_frame_base64: string
  readonly accepted_decision_frame_base64: string
  readonly rejected_decision_frame_base64: string
}

nativeOwnerDescribe('test process owner Node client native contract', () => {
  let owner: TestProcessOwnerArtifact
  let workspace: string
  let relayPath: string

  beforeAll(async () => {
    owner = await loadFrameworkProcessOwner()
    workspace = await mkdtemp(join(tmpdir(), 'windshare-owner-client-'))
    relayPath = join(workspace, process.platform === 'win32' ? 'wsrelay.exe' : 'wsrelay')
    await execFileAsync(
      process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
      ['build', '-trimpath', '-buildvcs=false', '-ldflags=-buildid=', '-o', relayPath, './relay/cmd/wsrelay'],
      { cwd: REPOSITORY_ROOT, windowsHide: true },
    )
  }, 30_000)

  afterAll(async () => {
    if (workspace !== undefined) await rm(workspace, { recursive: true, force: true })
  })

  it('settles natural exit, exact stdin delivery, and an empty process tree', async () => {
    const execution = await executeTestProcessOwner({
      ...baseExecution('owner-client-natural'),
      owner,
      environment: Object.freeze({
        WINDSHARE_CANONICAL_TEXT: 'before\u2028between\u2029after',
      }),
      command: Object.freeze({
        executable: process.execPath,
        arguments: Object.freeze([
          '-e',
          "const chunks=[];process.stdin.on('data',c=>chunks.push(c));process.stdin.on('end',()=>process.stdout.write(process.env.WINDSHARE_CANONICAL_TEXT+Buffer.concat(chunks)))",
        ]),
        cwd: REPOSITORY_ROOT,
        stdin: Buffer.from('private-input', 'utf8'),
      }),
    })

    expect(Buffer.from(execution.output.stdout.bytes()).toString('utf8'))
      .toBe('before\u2028between\u2029afterprivate-input')
    expect(execution.processEvidence).toEqual({ terminal: 'exited', exitCode: 0 })
    expect(execution.inputEvidence.outcome).toBe('delivered')
    expect(execution.startEvidence).toMatchObject({
      schemaVersion: 'windshare.process-owner-start-evidence/v1',
      runId: 'owner-client-run',
      operationId: 'owner-client-natural',
      scenario: 'owner-client-contract',
      platform: process.platform === 'win32' ? 'windows_job' : 'linux_subreaper',
      processId: expect.any(Number),
      processInstance: expect.stringMatching(/^[1-9]\d*$/u),
      executable: {
        volume: expect.stringMatching(/^[0-9a-f]{16}$/u),
        object: expect.stringMatching(/^(?!0{32}$)[0-9a-f]{32}$/u),
      },
    })
    expectSettledTree(execution, 'natural')
  }, 30_000)

  it('accepts absent start evidence only when settlement proves no target was created', async () => {
    const execution = await executeTestProcessOwner({
      ...baseExecution('owner-client-not-created'),
      owner,
      command: Object.freeze({
        executable: join(workspace, process.platform === 'win32' ? 'missing.exe' : 'missing'),
        arguments: Object.freeze([]),
        cwd: REPOSITORY_ROOT,
      }),
    })

    expect(execution.startEvidence).toBeUndefined()
    expect(['not-started', 'spawn-failed']).toContain(execution.processEvidence.terminal)
    expectSettledTree(execution, 'initialization_failed')
  }, 30_000)

  it('freezes caller-owned stdin before asynchronous owner attachment', async () => {
    const stdin = Buffer.from('invocation-snapshot', 'utf8')
    const executionTask = executeTestProcessOwner({
      ...baseExecution('owner-client-stdin-snapshot'),
      owner,
      command: Object.freeze({
        executable: process.execPath,
        arguments: Object.freeze([
          '-e',
          "const chunks=[];process.stdin.on('data',c=>chunks.push(c));process.stdin.on('end',()=>process.stdout.write(Buffer.concat(chunks)))",
        ]),
        cwd: REPOSITORY_ROOT,
        stdin,
      }),
    })
    stdin.fill('x')

    const execution = await executionTask
    expect(Buffer.from(execution.output.stdout.bytes()).toString('utf8')).toBe('invocation-snapshot')
    expectSettledTree(execution, 'natural')
  }, 30_000)

  it('rejects non-scalar request text before opening owner transport', async () => {
    await expect(executeTestProcessOwner({
      ...baseExecution('owner-client-invalid-unicode'),
      owner,
      command: Object.freeze({
        executable: process.execPath,
        arguments: Object.freeze(['\ud800']),
        cwd: REPOSITORY_ROOT,
      }),
    })).rejects.toThrow('test process owner command is invalid')
  })

  it('preserves authenticated settlement while discarding output beyond its capture authority', async () => {
    let failure: unknown
    try {
      await executeTestProcessOwner({
        ...baseExecution('owner-client-output-failure'),
        owner,
        command: Object.freeze({
          executable: process.execPath,
          arguments: Object.freeze(['-e', "process.stdout.write('owned-output')"]),
          cwd: REPOSITORY_ROOT,
        }),
        capture: Object.freeze({ stdoutBytes: 5, stderrBytes: 64 }),
      })
    } catch (cause) {
      failure = cause
    }

    expect(failure).toBeInstanceOf(TestProcessOwnerTransportError)
    if (!(failure instanceof TestProcessOwnerTransportError)) throw failure
    expect(failure.terminal).toEqual({ code: 0, signal: null })
    expect(failure.settlement?.processEvidence).toEqual({ terminal: 'exited', exitCode: 0 })
    expect(failure.settlement?.treeEmpty).toBe(true)
    expect(failure.output.stdout).toMatchObject({
      observedBytes: 12,
      capturedBytes: 5,
      truncated: true,
      completed: true,
    })
    expect(Buffer.from(failure.output.stdout.bytes()).toString('utf8')).toBe('owned')
  }, 30_000)

  it('settles lifecycle and cleanup when a pull consumer never resumes', async () => {
    const run = await startTestProcessOwner({
      ...baseExecution('owner-client-blocked-pull'),
      owner,
      command: Object.freeze({
        executable: process.execPath,
        arguments: Object.freeze(['-e', "process.stdout.write('first');setTimeout(()=>process.exit(0),25)"]),
        cwd: REPOSITORY_ROOT,
      }),
    })
    const blockedConsumer = (async () => {
      await run.stdout[Symbol.asyncIterator]().next()
      await new Promise<never>(() => {
        // The adversarial consumer deliberately never requests another chunk.
      })
    })()
    expect(blockedConsumer).toBeInstanceOf(Promise)

    const execution = await run.completion
    expect(Buffer.from(execution.output.stdout.bytes()).toString('utf8')).toBe('first')
    expectSettledTree(execution, 'natural')
  }, 30_000)

  windowsOwnerIt('retains a pre-readiness owner-failure settlement', async () => {
    let failure: unknown
    try {
      await executeTestProcessOwner({
        ...baseExecution('owner-client-pre-readiness-settlement'),
        owner,
        environment: Object.freeze({ Ä_NAME: 'one', ä_name: 'two' }),
        command: Object.freeze({
          executable: process.execPath,
          arguments: Object.freeze(['-e', 'process.exit(0)']),
          cwd: REPOSITORY_ROOT,
        }),
      })
    } catch (cause) {
      failure = cause
    }

    expect(failure).toBeInstanceOf(TestProcessOwnerTransportError)
    if (!(failure instanceof TestProcessOwnerTransportError)) throw failure
    expect(failure.terminal).toEqual({ code: 0, signal: null })
    expect(failure.settlement).toMatchObject({
      processEvidence: { terminal: 'not-started' },
      treeEmpty: true,
      ownerFailure: { code: expect.any(String), message: expect.any(String) },
      ownershipEvidence: { terminationReason: 'owner_failure' },
    })
  }, 30_000)

  it('delivers one identity-bound private relay event and classifies deadline termination', async () => {
    const controller = new AbortController()
    const events: TestProcessOwnerEvent[] = []
    const identity = Object.freeze({
      runId: 'owner-client-run',
      operationId: 'owner-client-relay',
      scenario: 'owner-client-contract',
    })
    const run = await startTestProcessOwner({
      ...baseExecution(identity.operationId),
      ...identity,
      owner,
      command: Object.freeze({
        executable: relayPath,
        arguments: Object.freeze([
          '--listen', '127.0.0.1:0',
          '--state-dir', join(workspace, 'relay-state'),
          '--allow-localhost=true',
        ]),
        cwd: REPOSITORY_ROOT,
      }),
      terminationSignal: controller.signal,
      capture: Object.freeze({ stdoutBytes: 1_048_576, stderrBytes: 1_048_576, eventCount: 1 }),
    })
    const eventTask = (async () => {
      for await (const event of run.events) {
        events.push(event)
        controller.abort(new TestProcessOwnerDeadlineError('relay contract reached its terminal deadline'))
        return
      }
    })()
    const execution = await run.completion
    await eventTask

    expect(events).toEqual([expect.objectContaining({
      ...identity,
      schemaVersion: 'windshare.test-event/v1',
      component: 'wsrelay',
      milestone: 'listener_ready',
      outcome: 'succeeded',
      payload: { address: expect.stringMatching(/^127\.0\.0\.1:\d+$/u) },
    })])
    expectSettledTree(execution, 'deadline')
  }, 30_000)

  it('settles repeated stop requests that race the owner attachment handshake', async () => {
    for (let index = 1; index <= 30; index += 1) {
      const controller = new AbortController()
      const executionTask = executeTestProcessOwner({
        ...baseExecution(`owner-client-early-stop-${index}`),
        owner,
        command: Object.freeze({
          executable: process.execPath,
          arguments: Object.freeze(['-e', 'setInterval(() => {}, 1000)']),
          cwd: REPOSITORY_ROOT,
        }),
        terminationSignal: controller.signal,
      })
      controller.abort(new Error('contract stop'))
      const execution = await executionTask
      expectSettledTree(execution, 'stop')
      if (execution.processEvidence.terminal === 'not-started') {
        expect(execution.processEvidence).toEqual({ terminal: 'not-started' })
      }
    }
  }, 30_000)

  linuxOwnerIt('does not hold a natural settlement open for the guardian control lease', async () => {
    const startedAt = Date.now()
    const execution = await executeTestProcessOwner({
      ...baseExecution('owner-client-linux-natural-control-lease'),
      owner,
      deadlineMs: 3_000,
      terminationGraceMs: 500,
      command: Object.freeze({
        executable: process.execPath,
        arguments: Object.freeze(['-e', 'process.exit(0)']),
        cwd: REPOSITORY_ROOT,
      }),
    })

    expectSettledTree(execution, 'natural')
    expect(Date.now() - startedAt).toBeLessThan(5_000)
  }, 15_000)
})

describe('test process owner start-evidence framed oracle', () => {
  it('matches the shared Go and Node start-gate vectors byte for byte', async () => {
    const fixture = JSON.parse(await readFile(
      join(REPOSITORY_ROOT, 'testdata', 'process-owner', 'start-gate-v1.json'),
      'utf8',
    )) as SharedStartGateVectors
    expect(fixture.schema_version).toBe('windshare.process-owner-start-gate-test-vectors/v1')
    expect(fixture.vectors.map((vector) => vector.name)).toEqual(['windows', 'linux'])

    for (const vector of fixture.vectors) {
      const nodePlatform = vector.evidence.platform === 'windows_job' ? 'win32' : 'linux'
      const request = Object.freeze({
        run_id: vector.evidence.run_id,
        operation_id: vector.evidence.operation_id,
        scenario: vector.evidence.scenario,
      })
      const evidenceFrame = Buffer.from(vector.evidence_frame_base64, 'base64')
      expect(decodeFramedDocument(evidenceFrame)).toEqual(vector.evidence)
      const evidence = parseTestProcessOwnerStartEvidenceFrameForRequest(
        evidenceFrame,
        request,
        nodePlatform,
      )
      if (evidence === undefined) throw new Error('shared vector omitted start evidence')

      const acceptedFrame = Buffer.from(encodeTestProcessOwnerStartDecisionFrame(
        evidence,
        Object.freeze({ outcome: 'accepted' }),
      ))
      expect(acceptedFrame.toString('base64')).toBe(vector.accepted_decision_frame_base64)
      expect(decodeFramedDocument(acceptedFrame)).toEqual(vector.accepted_decision)

      const rejectedFrame = Buffer.from(encodeTestProcessOwnerStartDecisionFrame(
        evidence,
        Object.freeze({
          outcome: 'rejected',
          failureCode: vector.rejected_decision.failure_code,
          failureMessage: vector.rejected_decision.failure_message,
        }),
      ))
      expect(rejectedFrame.toString('base64')).toBe(vector.rejected_decision_frame_base64)
      expect(decodeFramedDocument(rejectedFrame)).toEqual(vector.rejected_decision)

      expect(parseTestProcessOwnerSettlementForRequest(
        vector.rejected_settlement,
        Object.freeze({ ...request, command: Object.freeze({ stdin: null }) }),
        nodePlatform,
      )).toMatchObject({
        terminationReason: 'start_rejected',
        target: { outcome: 'not_started' },
        treeState: 'proven_empty',
        cleanup: { outcome: 'completed' },
      })
    }
  })

  it('authenticates exact Windows and Linux start evidence', () => {
    for (const [platform, kind] of [
      ['win32', 'windows_job'],
      ['linux', 'linux_subreaper'],
    ] as const) {
      const parsed = parseTestProcessOwnerStartEvidenceFrameForRequest(
        frameDocument(startEvidence(kind)),
        startEvidenceRequest(),
        platform,
      )
      expect(parsed).toEqual({
        schemaVersion: 'windshare.process-owner-start-evidence/v1',
        runId: 'owner-client-run',
        operationId: 'owner-client-start-parser',
        scenario: 'owner-client-contract',
        platform: kind,
        processId: 42,
        processInstance: '18446744073709551615',
        executable: {
          volume: '0123456789abcdef',
          object: '0123456789abcdef0123456789abcdef',
        },
      })
    }
  })

  it('distinguishes unavailable EOF from truncated or trailing framed evidence', () => {
    expect(parseTestProcessOwnerStartEvidenceFrameForRequest(
      Buffer.alloc(0),
      startEvidenceRequest(),
      'win32',
    )).toBeUndefined()
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      Buffer.from([0, 0, 0]),
      startEvidenceRequest(),
      'win32',
    )).toThrow('frame is truncated')
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      Buffer.concat([frameDocument(startEvidence('windows_job')), Buffer.from([0])]),
      startEvidenceRequest(),
      'win32',
    )).toThrow('stream contains trailing bytes')
  })

  it('rejects noncanonical and request-mismatched start authority', () => {
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument({
        ...startEvidence('windows_job'),
        process_id: 0,
      }),
      startEvidenceRequest(),
      'win32',
    )).toThrow('PID is invalid')
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument({
        ...startEvidence('windows_job'),
        process_instance: '01',
      }),
      startEvidenceRequest(),
      'win32',
    )).toThrow('canonical positive uint64')
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument({
        ...startEvidence('windows_job'),
        process_instance: '18446744073709551616',
      }),
      startEvidenceRequest(),
      'win32',
    )).toThrow('canonical positive uint64')
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument({
        ...startEvidence('windows_job'),
        executable: { volume: '0123456789abcdef', object: '0'.repeat(32) },
      }),
      startEvidenceRequest(),
      'win32',
    )).toThrow('identity is unavailable')
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument({
        ...startEvidence('linux_subreaper'),
        executable: {
          volume: '0123456789abcdef',
          object: '0123456789abcdef',
        },
      }),
      startEvidenceRequest(),
      'linux',
    )).toThrow('not canonical lowercase hexadecimal')
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument(startEvidence('linux_subreaper')),
      startEvidenceRequest(),
      'win32',
    )).toThrow('platform is inconsistent')
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument({
        ...startEvidence('windows_job'),
        operation_id: 'other-operation',
      }),
      startEvidenceRequest(),
      'win32',
    )).toThrow('differs from its request identity')

    const canonical = startEvidence('windows_job')
    const reordered = {
      run_id: canonical.run_id,
      schema_version: canonical.schema_version,
      operation_id: canonical.operation_id,
      scenario: canonical.scenario,
      platform: canonical.platform,
      process_id: canonical.process_id,
      process_instance: canonical.process_instance,
      executable: canonical.executable,
    }
    expect(() => parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument(reordered),
      startEvidenceRequest(),
      'win32',
    )).toThrow('not canonical JSON')
  })

  it('matches Go JSON.stringify compatibility for line separators in rejection diagnostics', () => {
    const evidence = parseTestProcessOwnerStartEvidenceFrameForRequest(
      frameDocument(startEvidence('windows_job')),
      startEvidenceRequest(),
      'win32',
    )
    if (evidence === undefined) throw new Error('start evidence unexpectedly absent')
    const frame = Buffer.from(encodeTestProcessOwnerStartDecisionFrame(
      evidence,
      Object.freeze({
        outcome: 'rejected',
        failureCode: 'AUTHORITY_REJECTED',
        failureMessage: 'before\u2028between\u2029after',
      }),
    ))
    const payload = frame.subarray(4).toString('utf8')

    expect(payload).toContain('before\u2028between\u2029after')
    expect(payload).not.toContain('before\\u2028between\\u2029after')
    expect(decodeFramedDocument(frame)).toMatchObject({
      failure_message: 'before\u2028between\u2029after',
    })
  })
})

describe('test process owner failure branding', () => {
  it('extracts only constructor-minted inert evidence without invoking Proxy traps', () => {
    const settlement = Object.freeze({ marker: 'settlement' }) as unknown as TestProcessOwnerExecution
    const transport = new TestProcessOwnerTransportError(
      'transport failed',
      settlement,
      { code: 7, signal: null },
      undefined as never,
      undefined as never,
      new Error('transport cause'),
    )
    const control = new TestProcessOwnerControlError(
      'control failed',
      settlement,
      new Error('control cause'),
    )

    expect(testProcessOwnerFailureEvidence(transport)).toEqual({
      kind: 'transport-failed',
      settlement,
      transportEvidence: {
        kind: 'test-process-owner-transport',
        terminal: { code: 7, signal: null },
      },
    })
    expect(testProcessOwnerFailureEvidence(control)).toEqual({
      kind: 'control-failed',
      settlement,
      transportEvidence: {
        kind: 'test-process-owner-control',
        publication: 'failed',
      },
    })
    const transportFailure = testProcessOwnerFailureEvidence(transport)
    expect(Object.isFrozen(transportFailure)).toBe(true)
    expect(Object.isFrozen(transportFailure?.transportEvidence)).toBe(true)
    if (transportFailure?.kind !== 'transport-failed') throw new Error('transport brand was lost')
    expect(Object.isFrozen(transportFailure.transportEvidence.terminal)).toBe(true)
    expect(testProcessOwnerFailureEvidence(Object.create(
      TestProcessOwnerTransportError.prototype,
    ))).toBeUndefined()

    const trapped = new Proxy(transport, {
      get: () => { throw new Error('property trap') },
      getPrototypeOf: () => { throw new Error('prototype trap') },
      has: () => { throw new Error('has trap') },
      ownKeys: () => { throw new Error('ownKeys trap') },
    })
    expect(testProcessOwnerFailureEvidence(trapped)).toBeUndefined()
  })
})

describe('test process owner request-bound settlement oracle', () => {
  it('rejects Windows Job active-process evidence outside uint32', () => {
    const settlement = ownerFailureSettlement(0x1_0000_0000)
    expect(() => parseTestProcessOwnerSettlementForRequest(
      settlement,
      settlementRequest(),
      'win32',
    )).toThrow('active process count must be an integer in [0, 4294967295]')
  })

  it('accepts start rejection only with exact target-not-started evidence', () => {
    const settlement = startRejectedSettlement()
    expect(parseTestProcessOwnerSettlementForRequest(
      settlement,
      settlementRequest(),
      'win32',
    )).toMatchObject({
      terminationReason: 'start_rejected',
      target: {
        outcome: 'not_started',
        failureCode: 'START_REJECTED',
        failureMessage: 'consumer rejected live executable authority',
      },
      treeState: 'proven_empty',
      cleanup: { outcome: 'completed' },
    })

    settlement.target = {
      outcome: 'spawn_failed',
      failure_code: 'SPAWN_FAILED',
      failure_message: 'spawn failed before evidence',
    }
    expect(() => parseTestProcessOwnerSettlementForRequest(
      settlement,
      settlementRequest(),
      'win32',
    )).toThrow('start rejection requires exact target-not-started evidence')
  })

  it('accepts an owned Windows root terminated by any controlled pre-release trigger', () => {
    for (const terminationReason of ['stop', 'parent_lost', 'deadline', 'start_rejected']) {
      const settlement = {
        ...startRejectedSettlement(),
        termination_reason: terminationReason,
        target: {
          outcome: 'not_started',
          failure_code: 'TARGET_NOT_RELEASED',
          failure_message: 'target was terminated before its authenticated start decision',
        },
        platform: {
          kind: 'windows_job',
          owner_pid: 1,
          root: { pid: 42, state: 'exited', exit_code: 1 },
          active_process_count: 0,
        },
      }

      expect(parseTestProcessOwnerSettlementForRequest(
        settlement,
        settlementRequest(),
        'win32',
      )).toMatchObject({
        terminationReason,
        target: { outcome: 'not_started', failureCode: 'TARGET_NOT_RELEASED' },
        treeState: 'proven_empty',
        cleanup: { outcome: 'completed' },
        platform: { root: { pid: 42, state: 'exited', exitCode: 1 } },
      })
    }
  })

  it('rejects unpaired surrogates in settlement diagnostics', () => {
    const settlement = ownerFailureSettlement(1)
    settlement.owner_failure.message = '\ud800'
    expect(() => parseTestProcessOwnerSettlementForRequest(
      settlement,
      settlementRequest(),
      'win32',
    )).toThrow('process owner failure message is invalid')
  })
})

function startEvidenceRequest() {
  return Object.freeze({
    run_id: 'owner-client-run',
    operation_id: 'owner-client-start-parser',
    scenario: 'owner-client-contract',
  })
}

function startEvidence(platform: 'windows_job' | 'linux_subreaper') {
  return {
    schema_version: 'windshare.process-owner-start-evidence/v1',
    run_id: 'owner-client-run',
    operation_id: 'owner-client-start-parser',
    scenario: 'owner-client-contract',
    platform,
    process_id: 42,
    process_instance: '18446744073709551615',
    executable: {
      volume: '0123456789abcdef',
      object: '0123456789abcdef0123456789abcdef',
    },
  }
}

function frameDocument(value: unknown): Buffer {
  const payload = Buffer.from(JSON.stringify(value), 'utf8')
  const header = Buffer.alloc(4)
  header.writeUInt32BE(payload.byteLength)
  return Buffer.concat([header, payload])
}

function decodeFramedDocument(frame: Buffer): unknown {
  if (frame.byteLength < 4) throw new Error('framed document header is truncated')
  const payloadLength = frame.readUInt32BE(0)
  if (payloadLength !== frame.byteLength - 4) {
    throw new Error('framed document length does not match its exact payload')
  }
  return JSON.parse(frame.subarray(4).toString('utf8')) as unknown
}

function settlementRequest() {
  return Object.freeze({
    run_id: 'owner-client-run',
    operation_id: 'owner-client-parser',
    scenario: 'owner-client-contract',
    command: Object.freeze({ stdin: null }),
  })
}

function startRejectedSettlement() {
  return {
    schema_version: 'windshare.process-owner-settlement/v3',
    run_id: 'owner-client-run',
    operation_id: 'owner-client-parser',
    scenario: 'owner-client-contract',
    termination_reason: 'start_rejected',
    target: {
      outcome: 'not_started',
      failure_code: 'START_REJECTED',
      failure_message: 'consumer rejected live executable authority',
    },
    input: { outcome: 'not_requested' },
    tree_state: 'proven_empty',
    cleanup: { outcome: 'completed' },
    platform: {
      kind: 'windows_job',
      owner_pid: 1,
      active_process_count: 0,
    },
  }
}

function ownerFailureSettlement(activeProcessCount: number) {
  return {
    schema_version: 'windshare.process-owner-settlement/v3',
    run_id: 'owner-client-run',
    operation_id: 'owner-client-parser',
    scenario: 'owner-client-contract',
    termination_reason: 'owner_failure',
    target: {
      outcome: 'terminal_evidence_lost',
      failure_code: 'TERMINAL_EVIDENCE_LOST',
      failure_message: 'exact target evidence is unavailable',
    },
    input: { outcome: 'not_requested' },
    tree_state: 'nonempty',
    cleanup: {
      outcome: 'failed',
      failure_code: 'OWNERSHIP_EVIDENCE_LOST',
      failure_message: 'the retained tree remains nonempty',
    },
    owner_failure: { code: 'OWNER_FAILED', message: 'owner authority failed' },
    platform: {
      kind: 'windows_job',
      owner_pid: 1,
      active_process_count: activeProcessCount,
    },
  }
}

function baseExecution(operationId: string) {
  return Object.freeze({
    runId: 'owner-client-run',
    operationId,
    scenario: 'owner-client-contract',
    environment: Object.freeze({}),
    deadlineMs: 10_000,
    terminationGraceMs: 2_000,
    capture: Object.freeze({ stdoutBytes: 2_097_152, stderrBytes: 2_097_152 }),
  })
}

function expectSettledTree(
  execution: TestProcessOwnerExecution,
  terminationReason: string,
): void {
  expect(execution).toMatchObject({
    treeEmpty: true,
    cleanupOutcome: 'completed',
    ownershipEvidence: {
      kind: 'test-process-owner',
      backend: process.platform === 'win32' ? 'windows_job' : 'linux_subreaper',
      terminationReason,
    },
  })
}
