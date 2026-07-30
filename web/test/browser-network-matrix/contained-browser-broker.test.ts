import { resolve } from 'node:path'

import { describe, expect, it, vi } from 'vitest'

import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentRequest,
} from '../../scripts/browser-evidence/process/containment.ts'
import { completedOwnedOperation } from '../../scripts/browser-network-matrix/owned-operation.ts'
import type { NetworkMatrixSampleExecutionContext } from '../../scripts/browser-network-matrix/runner.ts'
import type { NetworkMatrixProfileId } from '../../scripts/browser-network-matrix/vocabulary.ts'
import {
  ConcreteContainedBrowserProcessBroker,
  type ContainedBrowserSampleInputAuthority,
} from '../../scripts/browser-network-matrix/linux-topology/contained-browser-broker.ts'
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

    const evidence = await broker.start(await sampleContext()).result

    const request = vi.mocked(backend.execute).mock.calls[0]?.[0]
    expect(request?.command.stdin).toBe(authority.secretFrame)
    expect(JSON.stringify(request?.command)).not.toContain(SENSITIVE_VALUE)
    expect(request?.command.environment).toBeUndefined()
    expect(request?.readOnlyInputRoots).toEqual([])
    expect(request?.terminationSignal).toBeInstanceOf(AbortSignal)
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

  it('independently aborts the backend signal and awaits terminal reaping', async () => {
    const authority = inputAuthority()
    let request: BrowserSampleContainmentRequest | undefined
    const backend: BrowserSampleContainmentBackend = {
      kind: 'test',
      preflight: vi.fn().mockResolvedValue(undefined),
      execute: vi.fn((value: BrowserSampleContainmentRequest): Promise<{
        readonly processEvidence: { readonly terminal: 'signaled'; readonly signal: string }
        readonly timedOut: boolean
      }> => {
        request = value
        return new Promise((resolveExecution) => {
          value.terminationSignal?.addEventListener('abort', () => resolveExecution({
            processEvidence: { terminal: 'signaled', signal: 'SIGTERM' },
            timedOut: true,
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
): BrowserSampleContainmentBackend {
  return {
    kind: 'test',
    preflight: vi.fn().mockResolvedValue(undefined),
    execute: vi.fn(async (request: BrowserSampleContainmentRequest) => {
      request.stdout(new TextEncoder().encode(
        encoded ? output as string : `${JSON.stringify(output)}\n`,
      ))
      return {
        processEvidence: { terminal: 'exited' as const, exitCode: 0 },
        timedOut: false,
      }
    }),
  }
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
    operationId: `${RUN_ID}-${profileId}-chromium-1`,
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
