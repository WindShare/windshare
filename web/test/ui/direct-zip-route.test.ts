import { describe, expect, it, vi } from 'vitest'
import type { ArtifactChoiceID } from '../../src/transfer/intent'
import type { ReviewedDirectZipRuntimeFactsV1 } from '../../src/output/direct-zip/session'
import type { OfferedArtifactChoice } from '../../src/output/planning'
import type { ReceiveOperationMutationPort } from '../../src/output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationMutationResult,
  ReopenedDirectZipOperation,
} from '../../src/output/resume/reopen-authority'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import {
  BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID,
  startBrowserDirectZipAuthority,
  type BrowserDirectZipCompositionPort,
  type InstalledBrowserDirectZipRoute,
} from '../../src/ui/browser-receive/direct-zip'
import { listBrowserRetainedOperations } from '../../src/ui/browser-receive/retained'
import type { BrowserReceiveWindow } from '../../src/ui/browser-receive/contracts'
import type { V2ArtifactPresentationAuthority } from '../../src/ui/v2-receive-runtime'

const CHOICE = 'choice' as ArtifactChoiceID

describe('browser Direct ZIP route', () => {
  it('invokes the parent picker synchronously and preserves the exact displayed ranking', () => {
    let pickerInvoked = false
    const windowPort = {
      showDirectoryPicker: vi.fn(() => {
        pickerInvoked = true
        return Promise.resolve({ kind: 'directory' } as FileSystemDirectoryHandle)
      }),
    } as unknown as BrowserReceiveWindow
    let started: Parameters<BrowserDirectZipCompositionPort['runtime']['startFresh']>[0] | undefined
    const startFresh = vi.fn((input: Parameters<
      BrowserDirectZipCompositionPort['runtime']['startFresh']
    >[0]) => {
      started = input
      return authority()
    })
    const installed = installedRoute({ startFresh })
    const ranking = [CHOICE, 'workspace-choice' as ArtifactChoiceID]

    const result = startBrowserDirectZipAuthority(
      windowPort,
      offered(),
      ranking,
      installed,
    )
    ranking.splice(1, 1)

    expect(pickerInvoked).toBe(true)
    expect(result).toBeDefined()
    const frozenRanking = started?.preClickRanking
    expect(frozenRanking).toEqual([CHOICE, 'workspace-choice'])
    expect(Object.isFrozen(frozenRanking)).toBe(true)
  })

  it('refuses activation when the exact pre-click ranking was not retained', () => {
    const windowPort = {
      showDirectoryPicker: vi.fn(() => Promise.resolve({ kind: 'directory' })),
    } as unknown as BrowserReceiveWindow
    const installed = installedRoute()
    expect(() => startBrowserDirectZipAuthority(windowPort, offered(), [], installed))
      .toThrowError(/exact pre-click choice ranking/u)
    expect(windowPort.showDirectoryPicker).not.toHaveBeenCalled()
  })

  it('dispatches pre-intent bootstrap candidates before lifecycle inventory', async () => {
    const order: string[] = []
    const source = {
      listDirectZipBootstrapCandidates: vi.fn(async () => {
        order.push('candidates-read')
        return [{ operationId: 'operation', candidateId: 'candidate' } as never]
      }),
      listLifecycleStates: vi.fn(async () => {
        order.push('lifecycles-read')
        return []
      }),
      close: vi.fn(),
    }
    const directZip = directZipPort({
      dispatchBootstrapCandidate: vi.fn(async () => {
        order.push('candidate-dispatched')
      }),
    })
    const inventory = await listBrowserRetainedOperations(
      { indexedDB: { open: vi.fn() } } as unknown as BrowserReceiveWindow,
      { openResumeSource: async () => source, directZip },
      new AbortController().signal,
    )
    expect(order).toEqual(['candidates-read', 'candidate-dispatched', 'lifecycles-read'])
    expect(inventory.operations).toEqual([])
    inventory.close()
  })

  it('fails closed instead of hiding an unowned bootstrap effect', async () => {
    const source = {
      listDirectZipBootstrapCandidates: async () => [{ operationId: 'operation' } as never],
      listLifecycleStates: async () => [],
      close: vi.fn(),
    }
    await expect(listBrowserRetainedOperations(
      { indexedDB: { open: vi.fn() } } as unknown as BrowserReceiveWindow,
      { openResumeSource: async () => source },
      new AbortController().signal,
    )).rejects.toMatchObject({ name: 'NotSupportedError' })
    expect(source.close).toHaveBeenCalledOnce()
  })

  it.each([
    ['expired cleanup', expiredDirectZipLifecycle(), 'delete', 'expire'],
    ['published cleanup retry', publishedDirectZipLifecycle(), 'delete', 'expire'],
    ['published cleanup catch-up', publishedDirectZipLifecycle(), 'catch-up', 'catchUp'],
  ] as const)(
    'dispatches %s through the retained Direct ZIP runtime',
    async (_case, lifecycle, action, expectedMutation) => {
      const fixture = directZipRetainedActionFixture(lifecycle, () => 2_000)
      const inventory = await listRetainedFixture(fixture)
      const retained = inventory.operations[0]!

      await inventory.act(retained, action, new AbortController().signal)

      expect(fixture.mutations[expectedMutation]).toHaveBeenCalledOnce()
      expect(fixture.mutations.resume).not.toHaveBeenCalled()
      expect(fixture.mutations.discard).not.toHaveBeenCalled()
      expect(fixture.deleteRetained).toHaveBeenCalledWith(
        fixture.operation,
        expect.any(AbortSignal),
      )
      expect(fixture.operation.close).toHaveBeenCalledOnce()
      inventory.close()
    },
  )

  it.each(['continue', 'delete'] as const)(
    'converges deadline-crossing Direct ZIP %s on retained cleanup',
    async action => {
      let now = 999
      const fixture = directZipRetainedActionFixture(
        resumableDirectZipLifecycle(1_000),
        () => now,
      )
      const inventory = await listRetainedFixture(fixture)
      const retained = inventory.operations[0]!
      expect(retained.continuation).toBe('resume-direct-zip')
      now = 1_000

      await inventory.act(retained, action, new AbortController().signal)

      expect(fixture.mutations.expire).toHaveBeenCalledOnce()
      expect(fixture.mutations.resume).not.toHaveBeenCalled()
      expect(fixture.runtimeResume).not.toHaveBeenCalled()
      expect(fixture.deleteRetained).toHaveBeenCalledOnce()
      expect(fixture.operation.close).toHaveBeenCalledOnce()
      inventory.close()
    },
  )

  it('uses retained deletion for an unexpired Direct ZIP instead of opening execution', async () => {
    const fixture = directZipRetainedActionFixture(
      resumableDirectZipLifecycle(3_000),
      () => 2_000,
    )
    const inventory = await listRetainedFixture(fixture)

    await inventory.act(
      inventory.operations[0]!,
      'delete',
      new AbortController().signal,
    )

    expect(fixture.mutations.resume).toHaveBeenCalledOnce()
    expect(fixture.runtimeResume).not.toHaveBeenCalled()
    expect(fixture.deleteRetained).toHaveBeenCalledOnce()
    expect(fixture.operation.close).toHaveBeenCalledOnce()
    inventory.close()
  })
})

function directZipRetainedActionFixture(
  lifecycle: ReceiveLifecycleState,
  now: () => number,
) {
  const operation = {
    kind: 'direct-zip',
    close: vi.fn(async () => undefined),
  } as unknown as ReopenedDirectZipOperation
  const active = Object.freeze({
    kind: 'continuation' as const,
    continuation: Object.freeze({ kind: 'direct-zip' as const, operation }),
  })
  const cleanup = Object.freeze({
    kind: 'continuation' as const,
    continuation: Object.freeze({
      kind: 'direct-zip-retained-cleanup' as const,
      operation,
    }),
  })
  const mutations = {
    resume: vi.fn(async () => active),
    expire: vi.fn(async () => cleanup),
    discard: vi.fn(async () => ({ kind: 'already-absent' as const })),
    catchUp: vi.fn(async () => cleanup),
  } satisfies ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult>
  const deleteRetained = vi.fn(async () => undefined)
  const runtimeResume = vi.fn()
  return {
    source: {
      listLifecycleStates: vi.fn(async () => [lifecycle]),
      close: vi.fn(),
    },
    directZip: directZipPort({ resume: runtimeResume, deleteRetained }),
    mutations,
    operation,
    deleteRetained,
    runtimeResume,
    now,
  }
}

async function listRetainedFixture(
  fixture: ReturnType<typeof directZipRetainedActionFixture>,
) {
  return listBrowserRetainedOperations(
    { indexedDB: { open: vi.fn() } } as unknown as BrowserReceiveWindow,
    {
      openResumeSource: async () => fixture.source,
      resumeMutations: fixture.mutations,
      directZip: fixture.directZip,
      now: fixture.now,
    },
    new AbortController().signal,
  )
}

function expiredDirectZipLifecycle(): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'expired',
    operationId: 'expired-direct-zip',
    receiveIntentDigest: 'intent',
    generation: 3n,
    priorStableState: 'resumable-receive',
    expiresAt: 1_000,
    cleanupState: 'cleanup-pending',
    expiryReceiptDigest: 'expiry',
  })
}

function publishedDirectZipLifecycle(): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'published',
    operationId: 'published-direct-zip',
    receiveIntentDigest: 'intent',
    generation: 3n,
    receiptDigest: 'published',
    cleanupState: 'cleanup-pending',
  })
}

function resumableDirectZipLifecycle(expiresAt: number): ReceiveLifecycleState {
  return Object.freeze({
    kind: 'resumable-receive',
    payloadKind: 'direct-zip',
    operationId: 'resumable-direct-zip',
    receiveIntentDigest: 'intent',
    generation: 3n,
    directZipCheckpointDigest: 'checkpoint',
    safeSelectedPayloadBytes: 64n,
    committedArchiveLength: 128n,
    checkpointPhase: 'between-members',
    expiresAt,
  })
}

function installedRoute(
  runtimeOverrides: Partial<BrowserDirectZipCompositionPort['runtime']> = {},
): InstalledBrowserDirectZipRoute {
  return Object.freeze({ directZip: directZipPort(runtimeOverrides), reviewed: reviewed() })
}

function directZipPort(
  runtimeOverrides: Partial<BrowserDirectZipCompositionPort['runtime']> = {},
): BrowserDirectZipCompositionPort {
  return {
    evidence: { read: vi.fn() },
    runtime: {
      startFresh: () => authority(),
      dispatchBootstrapCandidate: vi.fn(async () => undefined),
      resume: vi.fn(),
      deleteRetained: vi.fn(async () => undefined),
      ...runtimeOverrides,
    },
  }
}

function authority(): V2ArtifactPresentationAuthority {
  return Object.freeze({
    ready: Promise.resolve(),
    commit: vi.fn(),
    release: vi.fn(),
  })
}

function offered(): OfferedArtifactChoice {
  const support = reviewed().support
  return {
    choice: { choiceId: CHOICE },
    route: {
      kind: 'direct-resumable-zip',
      target: { routeId: BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID, support },
    },
  } as unknown as OfferedArtifactChoice
}

function reviewed(): ReviewedDirectZipRuntimeFactsV1 {
  const digest = 'digest'
  return {
    support: {
      kind: 'reviewed-supported',
      supportMatrixDigest: digest,
      browserBinaryDigest: digest,
      browserVersion: '1',
      operatingSystemBuild: 'os',
      filesystemProfile: 'fs',
      rawEvidenceDigest: digest,
      requiredFeatureFactsDigest: digest,
      recommendationPolicyDigest: digest,
      policies: {
        zipEncoding: digest,
        layout: digest,
        checkpoint: digest,
        journalBudget: digest,
        epoch: digest,
      },
    },
    recommendationPolicy: {
      version: 1,
      kind: 'available',
      workspacePeakBytesThreshold: 1n,
      policyDigest: digest,
    },
    automaticEpochBudget: {
      maximumPrefixCopyBytes: 1n,
      maximumCumulativePrefixCopyBytes: 1n,
      maximumModeledPeakTemporaryBytes: 1n,
    },
  }
}
