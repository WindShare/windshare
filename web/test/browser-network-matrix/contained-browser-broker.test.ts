import { resolve } from 'node:path'

import { describe, expect, it, vi } from 'vitest'

import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentExecution,
  BrowserSampleContainmentRequest,
} from '../../scripts/browser-evidence/process/containment.ts'
import { completedOwnedOperation } from '../../scripts/browser-network-matrix/owned-operation.ts'
import type { NetworkMatrixSampleExecutionContext } from '../../scripts/browser-network-matrix/runner.ts'
import { networkMatrixSampleOperationId } from '../../scripts/browser-network-matrix/sample-authority.ts'
import type { NetworkMatrixProfileId } from '../../scripts/browser-network-matrix/vocabulary.ts'
import {
  ConcreteContainedBrowserProcessBroker,
  type ContainedBrowserSampleInputAuthority,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-broker.ts'
import { requireCompleteLinuxTopologyTrace } from '../../scripts/browser-network-matrix/linux-topology/trace/index.ts'
import { cloneJson, loadRegistry } from './fixtures.ts'
import {
  TEST_REMOTE_PEER_PUBLIC_IP,
  testContainedBrowserSampleOutput,
  testNetworkMatrixExecutionAuthority,
} from './signed-fixture.ts'

const SENSITIVE_VALUE = 'credentialAuthorityMustRemainSecret012345'
const RUN_ID = 'contained-browser-run'

describe('concrete contained browser process broker', () => {
  it('passes one anonymous secret frame and accepts one bounded child result', async () => {
    const authority = inputAuthority()
    const output = validOutput()
    const backend = successfulBackend(output)
    const broker = createBroker(backend, authority)

    const operation = broker.start(await sampleContext())
    const evidence = await operation.result
    const traces = operation.traces.snapshot()

    const request = vi.mocked(backend.execute).mock.calls[0]?.[0]
    expect(request?.command.stdin).toBe(authority.secretFrame)
    expect(JSON.stringify(request?.command)).not.toContain(SENSITIVE_VALUE)
    expect(request?.command.environment).toBeUndefined()
    expect(request?.readOnlyInputRoots).toEqual([])
    expect(request?.terminationSignal).toBeInstanceOf(AbortSignal)
    expect(request?.capture).toEqual({ stdoutBytes: 4_194_304, stderrBytes: 4_194_304 })
    expect(request).not.toHaveProperty('stdout')
    expect(request).not.toHaveProperty('stderr')
    expect(request).not.toHaveProperty('trace')
    requireCompleteLinuxTopologyTrace(traces)
    expect(traces.events.filter(({ milestone }) => milestone === 'contained-browser-started'))
      .toHaveLength(1)
    expect(traces.events).toContainEqual(expect.objectContaining({
      operationId: request?.operationId,
      milestone: 'test-containment-settled',
      context: { operationId: request?.operationId },
    }))
    expect(traces.events.at(-1)).toMatchObject({
      operationId: request?.operationId,
      milestone: 'contained-browser-terminal',
      outcome: 'succeeded',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'contained-evidence-accepted',
      },
    })
    expect(evidence).toMatchObject({
      processInstanceId: output.processInstanceId,
      attemptEvidence: {
        pionAuthority: 'external-remote',
        browserSelectedPair: { selectedPair: 'present' },
        pionSelectedPair: { selectedPair: 'present' },
      },
    })
  })

  it.each([
    ['malformed', '{not-json}\n'],
    ['multiple', `${JSON.stringify(validOutput())}\n${JSON.stringify(validOutput())}\n`],
  ])('rejects %s stdout and closes the acquired authority', async (_name, stdout) => {
    const authority = inputAuthority()
    const backend = successfulBackend(stdout, true)
    const broker = createBroker(backend, authority)

    await expect(broker.start(await sampleContext()).result).rejects.toThrow(
      'one bounded authoritative result',
    )
    expect(authority.close).toHaveBeenCalledOnce()
  })

  it('publishes failed cleanup without replacing the last workflow milestone', async () => {
    const authority = inputAuthority()
    authority.close.mockImplementation(() => Object.freeze({
      result: Promise.reject(new Error('input cleanup failed')),
      forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
    }))
    const operation = createBroker(successfulBackend(validOutput()), authority)
      .start(await sampleContext())

    await expect(operation.result).rejects.toThrow('input cleanup failed')

    const traces = operation.traces.snapshot()
    requireCompleteLinuxTopologyTrace(traces)
    expect(traces.events.at(-1)).toMatchObject({
      milestone: 'contained-browser-terminal',
      outcome: 'failed',
      context: {
        cleanupOutcome: 'failed',
        lastMilestone: 'contained-evidence-accepted',
      },
    })
  })

  it('retains a hostile failure opaquely without invoking its proxy traps', async () => {
    const authority = inputAuthority()
    let trapCalls = 0
    const hostile = new Proxy(new Error('must remain opaque'), {
      get() {
        trapCalls += 1
        throw new Error('hostile failure was inspected')
      },
      getPrototypeOf() {
        trapCalls += 1
        throw new Error('hostile failure prototype was inspected')
      },
    })
    const backend: BrowserSampleContainmentBackend = {
      kind: 'test',
      preflight: vi.fn().mockResolvedValue(undefined),
      execute: vi.fn().mockRejectedValue(hostile),
    }
    const operation = createBroker(backend, authority).start(await sampleContext())

    const settlement = await operation.result.then(
      () => Object.freeze({ cause: undefined }),
      (cause: unknown) => Object.freeze({ cause }),
    )

    expect(settlement.cause).toBe(hostile)
    expect(trapCalls).toBe(0)
    requireCompleteLinuxTopologyTrace(operation.traces.snapshot())
    expect(operation.traces.snapshot().events.at(-1)).toMatchObject({
      milestone: 'contained-browser-terminal',
      outcome: 'failed',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'contained-process-started',
      },
    })
  })

  it('independently aborts the backend signal and awaits terminal reaping', async () => {
    const authority = inputAuthority()
    let request: BrowserSampleContainmentRequest | undefined
    const backend: BrowserSampleContainmentBackend = {
      kind: 'test',
      preflight: vi.fn().mockResolvedValue(undefined),
      execute: vi.fn((value: BrowserSampleContainmentRequest): Promise<BrowserSampleContainmentExecution> => {
        request = value
        return new Promise((resolveExecution) => {
          value.terminationSignal?.addEventListener('abort', () => resolveExecution({
            processEvidence: { terminal: 'signaled', signal: 'SIGTERM' },
            terminationReason: 'stop',
            output: outputSnapshots('', ''),
            traces: eventSnapshot([]),
          }), { once: true })
        })
      }),
    }
    const operation = createBroker(backend, authority).start(await sampleContext())
    await vi.waitFor(() => expect(request).toBeDefined())

    await operation.forceTerminateAndWait('sample-execute')

    expect(request?.terminationSignal?.aborted).toBe(true)
    expect(authority.forceTerminateAndWait).toHaveBeenCalledWith('sample-execute')
    await expect(operation.result).rejects.toThrow()
  })

  it('rejects secret reflection without reflecting the secret in its error', async () => {
    const authority = inputAuthority()
    const reflected = validOutput()
    reflected.browserSelectedPair = {
      selectedPair: 'present',
      localCandidateType: 'srflx',
      localAddress: SENSITIVE_VALUE,
      localPort: 50_000,
      remoteCandidateType: 'host',
      remoteAddress: TEST_REMOTE_PEER_PUBLIC_IP,
      remotePort: 40_000,
      protocol: 'udp',
    }
    const broker = createBroker(successfulBackend(reflected), authority)
    let failure: unknown
    try {
      await broker.start(await sampleContext()).result
    } catch (cause) {
      failure = cause
    }

    expect(failure).toBeInstanceOf(Error)
    expect(String(failure)).not.toContain(SENSITIVE_VALUE)
    expect(authority.close).toHaveBeenCalledOnce()
  })

  it('rejects a deadline settlement even when the root exits zero', async () => {
    const authority = inputAuthority()
    const backend = successfulBackend(validOutput(), false, (execution) => Object.freeze({
      ...execution,
      terminationReason: 'deadline',
    }))

    await expect(createBroker(backend, authority).start(await sampleContext()).result)
      .rejects.toThrow('one bounded authoritative result')
    expect(authority.close).toHaveBeenCalledOnce()
  })

  it('rejects a truncated pull snapshot without parsing its retained prefix', async () => {
    const authority = inputAuthority()
    const backend = successfulBackend(validOutput(), false, (execution) => Object.freeze({
      ...execution,
      output: Object.freeze({
        ...execution.output,
        stdout: Object.freeze({
          ...execution.output.stdout,
          observedBytes: execution.output.stdout.observedBytes + 1,
          truncated: true,
        }),
      }),
    }))

    await expect(createBroker(backend, authority).start(await sampleContext()).result)
      .rejects.toThrow('one bounded authoritative result')
    expect(authority.close).toHaveBeenCalledOnce()
  })

  it('projects restricted UDP as two absent pairs bound to the external fixture', async () => {
    const authority = inputAuthority()
    const output = validOutput('scheduled-restricted-udp')
    const evidence = await createBroker(successfulBackend(output), authority)
      .start(await sampleContext('scheduled-restricted-udp')).result

    expect(evidence.attemptEvidence).toMatchObject({
      pionAuthority: 'external-remote',
      browserSelectedPair: { selectedPair: 'absent' },
      pionSelectedPair: { selectedPair: 'absent' },
      challenge: null,
    })
  })

  it('does not count an unrelated protocol failure as restricted UDP evidence', async () => {
    const authority = inputAuthority()
    const output = validOutput('scheduled-restricted-udp')
    ;(output.protocolResult as Record<string, unknown>).failureCode = 'invalid-offer'

    await expect(createBroker(successfulBackend(output), authority)
      .start(await sampleContext('scheduled-restricted-udp')).result).rejects.toThrow(
      'one bounded authoritative result',
    )
    expect(authority.close).toHaveBeenCalledOnce()
  })

  it('rejects a Pion pair whose two endpoints disagree on transport protocol', async () => {
    const authority = inputAuthority()
    const output = validOutput()
    const protocol = output.protocolResult as Record<string, unknown>
    const selectedPair = protocol.selectedPair as Record<string, unknown>
    const remote = selectedPair.remote as Record<string, unknown>
    remote.protocol = 'tcp'

    await expect(createBroker(successfulBackend(output), authority)
      .start(await sampleContext()).result).rejects.toThrow(
      'one bounded authoritative result',
    )
    expect(authority.close).toHaveBeenCalledOnce()
  })
})

function createBroker(
  containment: BrowserSampleContainmentBackend,
  input: ContainedBrowserSampleInputAuthority,
): ConcreteContainedBrowserProcessBroker {
  return new ConcreteContainedBrowserProcessBroker({
    containment,
    inputs: {
      acquire: () => completedOwnedOperation(input),
    },
    nodeExecutable: process.execPath,
    repositoryRoot: resolve('.'),
    processDeadlineMs: 1_000,
    terminationGraceMs: 100,
  })
}

function successfulBackend(
  output: Record<string, unknown> | string,
  encoded = false,
  projectExecution: (
    execution: BrowserSampleContainmentExecution,
  ) => BrowserSampleContainmentExecution = (execution) => execution,
): BrowserSampleContainmentBackend {
  return {
    kind: 'test',
    preflight: vi.fn().mockResolvedValue(undefined),
    execute: vi.fn(async (request: BrowserSampleContainmentRequest) => {
      const stdout = encoded ? output as string : `${JSON.stringify(output)}\n`
      return projectExecution(Object.freeze({
        processEvidence: { terminal: 'exited' as const, exitCode: 0 },
        terminationReason: 'natural' as const,
        output: outputSnapshots(stdout, ''),
        traces: eventSnapshot([{
          milestone: 'test-containment-settled',
          outcome: 'succeeded',
          context: { operationId: request.operationId },
        }]),
      }))
    }),
  }
}

function outputSnapshots(stdout: string, stderr: string): BrowserSampleContainmentExecution['output'] {
  return Object.freeze({ stdout: byteSnapshot(stdout), stderr: byteSnapshot(stderr) })
}

function byteSnapshot(encoded: string): BrowserSampleContainmentExecution['output']['stdout'] {
  const retained = new TextEncoder().encode(encoded)
  return Object.freeze({
    observedBytes: retained.byteLength,
    capturedBytes: retained.byteLength,
    truncated: false,
    completed: true,
    bytes: () => Uint8Array.from(retained),
  })
}

function eventSnapshot(
  events: BrowserSampleContainmentExecution['traces']['events'],
): BrowserSampleContainmentExecution['traces'] {
  return Object.freeze({
    events: Object.freeze([...events]),
    observedEvents: events.length,
    capturedEvents: events.length,
    truncated: false,
    completed: true,
  })
}

function inputAuthority(): ContainedBrowserSampleInputAuthority & {
  readonly close: ReturnType<typeof vi.fn>
  readonly forceTerminateAndWait: ReturnType<typeof vi.fn>
} {
  return {
    secretFrame: new TextEncoder().encode(SENSITIVE_VALUE),
    containsSensitiveValue: (encoded: string) => encoded.includes(SENSITIVE_VALUE),
    topologyProfilePath: resolve('testdata', 'browser-network-matrix', 'profile.json'),
    topologyProfileSha256: '1'.repeat(64),
    topologyResolutionPath: resolve('testdata', 'browser-network-matrix', 'resolution.json'),
    topologyResolutionSha256: '2'.repeat(64),
    sampleDirectory: resolve('test-temp', 'sample'),
    childAttachmentStagingRoot: resolve('test-temp', 'staging'),
    checkoutSha: '3'.repeat(40),
    close: vi.fn(() => completedOwnedOperation(undefined)),
    forceTerminateAndWait: vi.fn().mockResolvedValue(undefined),
  }
}

async function sampleContext(
  profileId: NetworkMatrixProfileId = 'scheduled-public-stun',
): Promise<NetworkMatrixSampleExecutionContext> {
  const registry = await loadRegistry()
  const profile = registry.profiles.find((candidate) => candidate.profileId === profileId)
  if (profile === undefined) throw new Error('test registry lacks requested profile')
  return Object.freeze({
    runId: RUN_ID,
    manifestSha256: registry.manifestSha256,
    identity: Object.freeze({
      profileId,
      browser: 'chromium',
      sampleOrdinal: 1,
    }),
    profile,
    authority: testNetworkMatrixExecutionAuthority(profileId),
    operationId: networkMatrixSampleOperationId(RUN_ID, {
      profileId,
      browser: 'chromium',
      sampleOrdinal: 1,
    }),
  })
}

function validOutput(
  profileId: NetworkMatrixProfileId = 'scheduled-public-stun',
): Record<string, unknown> & { browserSelectedPair: Record<string, unknown> } {
  return cloneJson(testContainedBrowserSampleOutput(RUN_ID, {
    profileId,
    browser: 'chromium',
    sampleOrdinal: 1,
  })) as Record<string, unknown> & { browserSelectedPair: Record<string, unknown> }
}
