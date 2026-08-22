import { describe, expect, it } from 'vitest'

import { acquireFSARootMutationLease } from '../../src/output/browser/namespace-mutation'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { createIncidentScopeIssuer } from '../../src/diagnostics/incident'
import { authorizeFSAParent } from '../../src/output/capability/acquisition'
import type { AcquiredFSAParentAuthority } from '../../src/output/capability/contract'
import {
  bindReceiveIntent,
  type ResolvedArtifactAction,
} from '../../src/output/planning'
import {
  createAttemptOutputFailureCapability,
  type LocalOutputOperationFailureDiagnosticsPort,
} from '../../src/output/diagnostics'
import type { FSAFileCheckpointRepositoryFactory } from '../../src/output/file-system-access/session'
import {
  prepareCompatibleNameRootRepair,
} from '../../src/output/file-system-access/compatible-name/coordinator'
import {
  type CompatibleNameRepairSummary,
} from '../../src/output/file-system-access/compatible-name/model'
import { decodeCompatibleNameSidecar } from '../../src/output/file-system-access/compatible-name/sidecar-codec'
import type { ReopenedDirectTreeOperation } from '../../src/output/resume/reopen-authority'
import {
  decodeStoredReceiveOperation,
  RECEIVE_RECORD_OPERATION,
} from '../../src/output/workspace/records'
import type { ArtifactChoiceID } from '../../src/transfer/intent'
import {
  FSAReceiveOperation,
} from '../../src/ui/browser-receive/fsa'
import {
  MemoryDirectory,
  MemoryLockManager,
  memoryCheckpointFactory,
} from '../output/file-system-access-lifecycle-fixture'
import {
  identity,
} from '../output/planning/fixture'
import {
  MemoryCompatibleNameActivationLedger,
  TestRepository,
  acquiredParent,
  classifiedRootRejection,
  commitInput,
  commitInputForAction,
  deferred,
  fsaReservationName,
  planningFixture,
  replacementResolvedAction,
  requireDirectRoute,
  requireDirectTreeIntent,
  routeFixture,
} from './fsa-route-activation-fixture'


describe('FSA presentation route activation', () => {
  it('starts no route work while the one picker is pending and drains late success after release', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const picked = deferred<AcquiredFSAParentAuthority>()
    const observed: string[] = []
    const route = routeFixture(planning.offered, picked.promise, parent, new TestRepository(), {
      authorizeParent: async (authority) => {
        observed.push('authorize')
        await authorizeFSAParent(authority)
      },
      acquireRootLease: async (handle) => {
        observed.push('root-lease')
        return acquireFSARootMutationLease(handle, new MemoryLockManager())
      },
    })

    const commit = route.commit(commitInput(planning)).catch(error => error)
    await Promise.resolve()
    expect(observed).toEqual([])
    route.release(new DOMException('replaced', 'AbortError'))
    picked.resolve(acquiredParent(parent, planning.offered))

    await expect(route.ready).resolves.toBeUndefined()
    await expect(commit).resolves.toBeInstanceOf(Error)
    expect(observed).toEqual([])
  })

  it('drains a late picker refusal after release without opening route resources', async () => {
    const planning = await planningFixture()
    const picked = deferred<AcquiredFSAParentAuthority>()
    const repository = new TestRepository()
    const route = routeFixture(
      planning.offered,
      picked.promise,
      new MemoryDirectory('downloads'),
      repository,
    )
    route.release(new DOMException('cancelled', 'AbortError'))
    picked.reject(new DOMException('picker refused', 'AbortError'))

    await expect(route.ready).rejects.toMatchObject({ outcome: 'picker_refused' })
    expect(repository.transitions).toEqual([])
    expect(repository.closeCount).toBe(0)
  })

  it('authorizes before acquiring the root lease and keeps the namespace absent through bound return', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const order: string[] = []
    const locks = new MemoryLockManager()
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        authorizeParent: async (authority) => {
          order.push('authorize')
          await authorizeFSAParent(authority)
        },
        acquireRootLease: async (handle) => {
          order.push('root-lease')
          return acquireFSARootMutationLease(handle, locks)
        },
      },
    )

    await route.ready
    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') {
      throw result.kind === 'owned-effects' ? result.cause : new Error('expected a bound FSA operation')
    }
    expect(order).toEqual(['authorize', 'root-lease'])
    expect(repository.transitions[0]).toMatchObject({
      operationId: result.operation.intent.operationId,
      records: expect.arrayContaining([expect.objectContaining({ operationId: result.operation.intent.operationId })]),
      handles: [expect.objectContaining({ operationId: result.operation.intent.operationId })],
      lifecycle: expect.objectContaining({ kind: 'intent-frozen' }),
      lease: expect.objectContaining({ kind: 'put' }),
    })
    expect(parent.entryNames()).toEqual([])

    const directIntent = requireDirectTreeIntent(result.operation.intent)
    await result.operation.plans.openDirectTree(directIntent, new AbortController().signal)
    expect(parent.directoryNames()).toEqual([fsaReservationName(directIntent)])
    await result.operation.detach()
  })

  it('persists the immutable pre-click ranking instead of substituting the selected DirectTree choice', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const secondary = identity(96, 32) as ArtifactChoiceID
    const ranking = [planning.offered.choice.choiceId, secondary]
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {},
      undefined,
      ranking,
    )
    ranking.splice(1, 1)

    const result = await route.commit(commitInput(planning))
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    const records = await repository.listRecords(result.operation.intent.operationId)
    const record = records.find(candidate => candidate.kind === RECEIVE_RECORD_OPERATION)
    if (record === undefined) throw new Error('FSA operation record was not persisted')
    const operation = await decodeStoredReceiveOperation(record)
    expect(operation.preClickRanking).toEqual([planning.offered.choice.choiceId, secondary])
    expect(Object.isFrozen(operation.preClickRanking)).toBe(true)
    await result.operation.detach()
  })

  it('keeps the ordinary root on the zero-repair path with expected-kind-first inspection', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const lookups: string[] = []
    let repairFactoryCalls = 0
    let ledgerFactoryCalls = 0
    parent.onEntryLookup = (lookup) => {
      if (!lookup.create) lookups.push(`${lookup.kind}:${lookup.name}`)
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        prepareCompatibleNameRootRepair: async () => {
          repairFactoryCalls += 1
          throw new Error('ordinary reservation must not activate repair')
        },
        openCompatibleNameLedger: async () => {
          ledgerFactoryCalls += 1
          throw new Error('ordinary reservation must not open the repair ledger')
        },
      },
    )

    const result = await route.commit(commitInput(planning))
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    const reservation = requireDirectTreeIntent(result.operation.intent).plan.reservation
    if (reservation.kind !== 'named-container-entry') throw new Error('expected named reservation')
    expect(reservation.logicalReservedName).toBe(reservation.physicalName)
    expect(lookups).toEqual(['directory:photos', 'file:photos'])
    expect(repairFactoryCalls).toBe(0)
    expect(ledgerFactoryCalls).toBe(0)
    expect(repository.compatibleBootstrapCommits).toBe(0)
    expect(result.operation.repairProjection).toBeUndefined()
    expect(parent.entryNames()).toEqual([])
    await result.operation.detach()
  })

})

describe('FSA compatible-name route activation', () => {
  it('commits rejected-root repair atomically and verifies the owned pair before root creation', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const ledger = new MemoryCompatibleNameActivationLedger(repository)
    const mutationOrder: string[] = []
    let refused = false
    let ledgerFactoryCalls = 0
    parent.onEntryLookup = (lookup) => {
      if (!refused && !lookup.create && lookup.kind === 'directory' && lookup.name === 'photos') {
        refused = true
        throw new TypeError('injected native root refusal')
      }
      if (lookup.create) mutationOrder.push(`${lookup.kind}:${lookup.name}`)
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        prepareCompatibleNameRootRepair: input => prepareCompatibleNameRootRepair(input, {
          platform: 'windows',
          randomBits: () => 0,
          randomOwnedObjectId: (() => {
            const ids = [identity(52, 32), identity(53, 32)]
            return () => ids.shift()!
          })(),
        }),
        openCompatibleNameLedger: async () => {
          ledgerFactoryCalls += 1
          return ledger
        },
      },
    )
    const baseInput = commitInput(planning)
    const result = await route.commit({
      ...baseInput,
      freezeAtFence: async (candidate) => {
        expect(repository.compatibleBootstrap).toBeUndefined()
        expect(parent.entryNames()).toEqual([])
        return baseInput.freezeAtFence(candidate)
      },
    })
    if (result.kind !== 'bound-operation') {
      throw result.kind === 'owned-effects' ? result.cause : new Error('expected a bound FSA operation')
    }

    const reservation = requireDirectTreeIntent(result.operation.intent).plan.reservation
    if (reservation.kind !== 'named-container-entry') throw new Error('expected named reservation')
    expect(reservation).toMatchObject({
      logicalReservedName: 'photos',
      physicalName: 'photos.windshare-aaaaaa',
    })
    expect(repository.compatibleBootstrapCommits).toBe(1)
    expect(repository.compatibleBootstrap?.initialMapping.physicalComponent)
      .toBe(reservation.physicalName)
    expect(ledgerFactoryCalls).toBe(1)
    expect(ledger.header?.activationState).toBe('pair-ready')
    expect(ledger.header?.pair.script.ownershipState).toBe('owned')
    expect(ledger.header?.pair.sidecar.ownershipState).toBe('owned')
    expect(parent.entryNames()).toEqual([
      'restore.windshare-aaaaaa.data',
      'restore.windshare-aaaaaa.ps1',
    ])
    const checkpoint = decodeCompatibleNameSidecar(
      await parent.fileBytes('restore.windshare-aaaaaa.data'),
    )
    expect(checkpoint).toMatchObject({
      header: { operationId: result.operation.intent.operationId, placement: 'beside' },
      footer: { committedCount: 0, state: 'active' },
      mappings: [],
      trailingByteLength: 0,
    })
    expect(mutationOrder).toEqual([
      'file:restore.windshare-aaaaaa.ps1',
      'file:restore.windshare-aaaaaa.data',
    ])
    const repairSummaries: CompatibleNameRepairSummary[] = []
    const repairProjection = result.operation.repairProjection
    if (repairProjection === undefined) throw new Error('repaired runtime omitted its projection')
    const unsubscribeRepair = repairProjection.subscribe(summary => repairSummaries.push(summary))
    expect(repairSummaries).toEqual([])

    await result.operation.plans.openDirectTree(
      requireDirectTreeIntent(result.operation.intent),
      new AbortController().signal,
    )
    expect(mutationOrder).toEqual([
      'file:restore.windshare-aaaaaa.ps1',
      'file:restore.windshare-aaaaaa.data',
      'directory:photos.windshare-aaaaaa',
    ])
    expect(ledger.mapping?.commitState).toBe('committed')
    expect(ledger.mapping?.ownedObjectId).toBeDefined()
    expect(repairSummaries[0]).toMatchObject({ committedCount: 0, pendingCatchUp: false })
    expect(repairSummaries).toContainEqual(expect.objectContaining({
      committedCount: 1,
      pendingCatchUp: true,
    }))
    unsubscribeRepair()
    await result.operation.detach()
    expect(ledger.closeCount).toBe(1)
  })

  it('advances root and pair allocation only for candidates proven occupied', async () => {
    const parent = new MemoryDirectory('downloads')
    await parent.getDirectoryHandle('photos.windshare-aaaaaa', { create: true })
    await parent.getFileHandle('restore.windshare-aaaaaa.ps1', { create: true })
    const prepared = await prepareCompatibleNameRootRepair({
      rejection: classifiedRootRejection('photos', 'directory'),
      parent: parent as unknown as FileSystemDirectoryHandle,
      operationId: identity(40),
      authorityRef: identity(42, 32),
      logicalReservedName: 'photos',
      entryKind: 'directory',
    }, {
      platform: 'windows',
      randomBits: () => 0,
      randomOwnedObjectId: (() => {
        const ids = [identity(52, 32), identity(53, 32)]
        return () => ids.shift()!
      })(),
    })

    expect(prepared.bootstrap.initialMapping).toMatchObject({ attempt: 1 })
    expect(prepared.bootstrap.initialMapping.physicalComponent)
      .not.toBe('photos.windshare-aaaaaa')
    expect(prepared.bootstrap.header.pair.script.physicalName)
      .not.toBe('restore.windshare-aaaaaa.ps1')
    expect(prepared.bootstrap.header.pair.sidecar.physicalName)
      .toBe('restore.windshare-aaaaaa.data')
    expect(parent.entryNames()).toEqual([
      'photos.windshare-aaaaaa',
      'restore.windshare-aaaaaa.ps1',
    ])
  })

  it('does not reinterpret a compatible-candidate TypeError as occupancy', async () => {
    const parent = new MemoryDirectory('downloads')
    const cause = new TypeError('derived candidate rejected')
    const inspected: string[] = []
    parent.onEntryLookup = (lookup) => {
      inspected.push(`${lookup.kind}:${lookup.name}`)
      if (lookup.kind === 'directory' && lookup.name === 'photos.windshare-aaaaaa') throw cause
    }

    await expect(prepareCompatibleNameRootRepair({
      rejection: classifiedRootRejection('photos', 'directory'),
      parent: parent as unknown as FileSystemDirectoryHandle,
      operationId: identity(40),
      authorityRef: identity(42, 32),
      logicalReservedName: 'photos',
      entryKind: 'directory',
    }, {
      platform: 'windows',
      randomBits: () => 0,
    })).rejects.toBe(cause)
    expect(inspected).toEqual(['directory:photos.windshare-aaaaaa'])
    expect(parent.entryNames()).toEqual([])
  })

  it('keeps the compatible target absent when restoration-pair creation fails', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const ledger = new MemoryCompatibleNameActivationLedger(repository)
    const pairFailure = new DOMException('sidecar creation refused', 'NotAllowedError')
    let refused = false
    parent.onEntryLookup = (lookup) => {
      if (!refused && !lookup.create && lookup.kind === 'directory' && lookup.name === 'photos') {
        refused = true
        throw new TypeError('injected native root refusal')
      }
      if (lookup.create && lookup.name === 'restore.windshare-aaaaaa.data') throw pairFailure
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        prepareCompatibleNameRootRepair: input => prepareCompatibleNameRootRepair(input, {
          platform: 'windows',
          randomBits: () => 0,
          randomOwnedObjectId: (() => {
            const ids = [identity(52, 32), identity(53, 32)]
            return () => ids.shift()!
          })(),
        }),
        openCompatibleNameLedger: async () => ledger,
      },
    )

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned repair effects')
    expect(result.cause).toBe(pairFailure)
    expect(repository.compatibleBootstrapCommits).toBe(1)
    expect(parent.entryNames()).toEqual(['restore.windshare-aaaaaa.ps1'])
    expect(parent.directoryNames()).not.toContain('photos.windshare-aaaaaa')
    expect(ledger.header?.pair.script.ownershipState).toBe('owned')
    expect(ledger.header?.pair.sidecar.ownershipState).toBe('claimed')
    expect(ledger.closeCount).toBe(1)
    await result.authority.settleActivationFailure(result.cause)
    await result.authority.detach()
  })

  it('uses one output-session identity for local stage diagnostics and DirectTree execution', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const outputSessionId = identity(44)
    const transferJobId = identity(43)
    const observedSessionIds: string[] = []
    const observedTransferJobIds: string[] = []
    const localOutputFailures: LocalOutputOperationFailureDiagnosticsPort = {
      forAttempt: (input) => {
        observedSessionIds.push(input.outputSessionId)
        observedTransferJobIds.push(input.transferJobId)
        expect(input.attempt.claim()?.scope.scopeKind).toBe('authority_activation')
        return Object.freeze({
          outputSessionId: input.outputSessionId,
          observe: () => undefined,
        })
      },
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      new TestRepository(),
      {
        createOutputSessionId: () => outputSessionId,
        createTransferJobId: () => transferJobId,
      },
      localOutputFailures,
    )

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    const execution = await result.operation.plans.openDirectTree(
      requireDirectTreeIntent(result.operation.intent),
      new AbortController().signal,
    )

    expect(observedSessionIds).toEqual([outputSessionId])
    expect(observedTransferJobIds).toEqual([transferJobId])
    expect(execution.output.identity.outputSessionId).toBe(outputSessionId)
    await result.operation.detach()
  })
})

describe('FSA output diagnostic correlation', () => {
  it('pre-creates the continuation identities before binding its owning attempt', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const transferJobIds = [identity(43), identity(45)]
    const outputSessionIds = [identity(44), identity(46)]
    const observed: Array<Readonly<{
      transferJobId: string
      outputSessionId: string
      scopeKind: string | undefined
    }>> = []
    const stopBeforeNativeReopen = new Error('continuation correlation observed')
    const localOutputFailures: LocalOutputOperationFailureDiagnosticsPort = {
      forAttempt: (input) => {
        observed.push(Object.freeze({
          transferJobId: input.transferJobId,
          outputSessionId: input.outputSessionId,
          scopeKind: input.attempt.claim()?.scope.scopeKind,
        }))
        if (observed.length === 2) throw stopBeforeNativeReopen
        return Object.freeze({
          outputSessionId: input.outputSessionId,
          observe: () => undefined,
        })
      },
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      new TestRepository(),
      {
        createTransferJobId: () => transferJobIds.shift()!,
        createOutputSessionId: () => outputSessionIds.shift()!,
      },
      localOutputFailures,
    )

    const result = await route.commit(commitInput(planning))
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    const lifecycle = Object.freeze({
      kind: 'resumable-receive' as const,
      payloadKind: 'file-set' as const,
      operationId: result.operation.intent.operationId,
      receiveIntentDigest: result.operation.intent.digest,
      generation: 1n,
      checkpointSetDigest: identity(47, 32),
      completedFileCount: 0n,
      completedBytes: 0n,
      expiresAt: 5_000,
    })

    await expect(result.operation.startLifecycleAction('continue', lifecycle))
      .rejects.toBe(stopBeforeNativeReopen)
    expect(observed).toEqual([
      {
        transferJobId: identity(43),
        outputSessionId: identity(44),
        scopeKind: 'authority_activation',
      },
      {
        transferJobId: identity(45),
        outputSessionId: identity(46),
        scopeKind: 'authority_activation',
      },
    ])
    await result.operation.detach()
  })

  it('binds retained reopen identities to the retained action incident before native reopen', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
    )
    const committed = await route.commit(commitInput(planning))
    if (committed.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    await committed.operation.plans.openDirectTree(
      requireDirectTreeIntent(committed.operation.intent),
      new AbortController().signal,
    )
    const receiving = repository.transitions.at(-1)?.lifecycle
    if (receiving?.kind !== 'receiving') throw new Error('expected an active receiving lifecycle')

    const fallback = Object.freeze({
      kind: 'resumable-receive' as const,
      payloadKind: 'file-set' as const,
      operationId: committed.operation.intent.operationId,
      receiveIntentDigest: committed.operation.intent.digest,
      generation: receiving.generation - 1n,
      checkpointSetDigest: identity(48, 32),
      completedFileCount: 0n,
      completedBytes: 0n,
      expiresAt: 5_000,
    })
    const retainedOperation = Object.freeze({
      kind: 'direct-tree' as const,
      intent: committed.operation.intent,
      lifecycle: receiving,
      receiveAdmissionFallback: fallback,
      repository,
      lease: Object.freeze({
        operationId: committed.operation.intent.operationId,
        leaseId: receiving.activeLeaseId,
        acquiredAt: 1_000,
        heartbeat: () => Promise.reject(new Error('unexpected retained heartbeat')),
        release: () => Promise.resolve(),
      }),
      binding: Object.freeze({}),
      close: () => Promise.resolve(),
    }) as unknown as ReopenedDirectTreeOperation
    const issuer = createIncidentScopeIssuer()
    const scope = issuer.open('retained_action')
    const attempt = createAttemptOutputFailureCapability(scope.handle)
    const observed: Array<Readonly<{
      transferJobId: string
      outputSessionId: string
      scopeKind: string | undefined
    }>> = []
    const stopBeforeNativeReopen = new Error('retained correlation observed')
    const localOutputFailures: LocalOutputOperationFailureDiagnosticsPort = {
      forAttempt: (input) => {
        observed.push(Object.freeze({
          transferJobId: input.transferJobId,
          outputSessionId: input.outputSessionId,
          scopeKind: input.attempt.claim()?.scope.scopeKind,
        }))
        throw stopBeforeNativeReopen
      },
    }

    await expect(FSAReceiveOperation.reopen(
      retainedOperation,
      { backend: 'file_system_access', failures: attempt.sinks },
      localOutputFailures,
      {
        createTransferJobId: () => identity(49),
        createOutputSessionId: () => identity(50),
      },
    )).rejects.toBe(stopBeforeNativeReopen)
    expect(observed).toEqual([{
      transferJobId: identity(49),
      outputSessionId: identity(50),
      scopeKind: 'retained_action',
    }])

    attempt.revoke()
    scope.close()
    await committed.operation.detach()
  })
})

describe('FSA presentation route activation retry cuts', () => {
  it('reuses the settled picker authority after clean cancellation before candidate preparation', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const firstAttempt = new AbortController()
    let pickerSettlementCount = 0
    let authorizationCount = 0
    const picked = Promise.resolve(acquiredParent(parent, planning.offered)).then((authority) => {
      pickerSettlementCount += 1
      return authority
    })
    const route = routeFixture(planning.offered, picked, parent, repository, {
      authorizeParent: async (authority) => {
        authorizationCount += 1
        await authorizeFSAParent(authority)
        if (authorizationCount === 1) {
          firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
        }
      },
    })

    await route.ready
    await expect(route.commit({
      ...commitInput(planning),
      signal: firstAttempt.signal,
    })).resolves.toEqual({ kind: 'retryable-precut' })
    expect(repository.transitions).toEqual([])
    expect(repository.closeCount).toBe(0)

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    expect(pickerSettlementCount).toBe(1)
    expect(authorizationCount).toBe(2)
    await result.operation.detach()
  })

  it('recommits with the newest resolved artifact after the final fence cancels cleanly', async () => {
    const planning = await planningFixture()
    const replacementAction = await replacementResolvedAction(planning)
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const rootLocks = new MemoryLockManager()
    const firstAttempt = new AbortController()
    let pickerSettlementCount = 0
    const picked = Promise.resolve(acquiredParent(parent, planning.offered)).then((authority) => {
      pickerSettlementCount += 1
      return authority
    })
    const route = routeFixture(planning.offered, picked, parent, repository, {
      acquireRootLease: handle => acquireFSARootMutationLease(handle, rootLocks),
    })

    const firstResult = await route.commit({
      action: planning.action,
      signal: firstAttempt.signal,
      freezeAtFence: async (candidate) => {
        await bindReceiveIntent({ selection: planning.selection, action: planning.action, candidate })
        firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
        firstAttempt.signal.throwIfAborted()
        throw new Error('unreachable final fence')
      },
    })
    expect(firstResult).toEqual({
      kind: 'retryable-precut',
      receiverOperationId: identity(40),
    })
    expect(repository.transitions).toEqual([])
    expect(repository.closeCount).toBe(1)
    expect(rootLocks.releaseCount).toBe(1)

    const result = await route.commit(commitInputForAction(planning.selection, replacementAction))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    expect(replacementAction.artifact.digest).not.toBe(planning.action.artifact.digest)
    expect(result.operation.intent.artifact.digest).toBe(replacementAction.artifact.digest)
    expect(pickerSettlementCount).toBe(1)
    await result.operation.detach()
  })

  it('recommits after cancellation between the final fence and the durable cut', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const operationLocks = new MemoryLockManager()
    const firstAttempt = new AbortController()
    const leaseEntered = deferred<void>()
    const releaseLeaseAttempt = deferred<void>()
    let leaseAttemptCount = 0
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        acquireOperationLease: async (store, operationId, options) => {
          leaseAttemptCount += 1
          if (leaseAttemptCount === 1) {
            leaseEntered.resolve()
            await releaseLeaseAttempt.promise
            firstAttempt.signal.throwIfAborted()
          }
          return acquireBrowserReceiveOperationLease(store, operationId, {
            ...options,
            manager: operationLocks,
            clock: { now: () => 1_000 },
            randomBytes: length => new Uint8Array(length).fill(9),
          })
        },
      },
    )

    const firstCommit = route.commit({ ...commitInput(planning), signal: firstAttempt.signal })
    await leaseEntered.promise
    firstAttempt.abort(new DOMException('projection replacement', 'AbortError'))
    releaseLeaseAttempt.resolve()
    await expect(firstCommit).resolves.toEqual({
      kind: 'retryable-precut',
      receiverOperationId: identity(40),
    })
    expect(repository.records.size).toBe(0)
    expect(repository.handles.size).toBe(0)
    expect(repository.leases.size).toBe(0)

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('bound-operation')
    if (result.kind !== 'bound-operation') throw new Error('expected a bound FSA operation')
    expect(leaseAttemptCount).toBe(2)
    await result.operation.detach()
  })
})

describe('FSA presentation route failure ownership', () => {
  it('releases every transient authority when the final fence invalidates', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const rootLocks = new MemoryLockManager()
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      {
        acquireRootLease: handle => acquireFSARootMutationLease(handle, rootLocks),
      },
    )

    await expect(route.commit({
      action: planning.action,
      signal: new AbortController().signal,
      freezeAtFence: async () => {
        throw new DOMException('selection changed', 'AbortError')
      },
    })).rejects.toThrow('selection changed')
    expect(repository.records.size).toBe(0)
    expect(repository.handles.size).toBe(0)
    expect(repository.leases.size).toBe(0)
    expect(repository.closeCount).toBe(1)
    expect(rootLocks.releaseCount).toBe(1)
    expect(parent.entryNames()).toEqual([])
  })

  it('rejects an exact route-ID mismatch before authorization despite compatible guarantees', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    let authorizationCount = 0
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      new TestRepository(),
      { authorizeParent: async () => { authorizationCount += 1 } },
    )
    const mismatched = Object.freeze({
      ...planning.action,
      route: Object.freeze({
        ...planning.action.route,
        target: Object.freeze({ ...requireDirectRoute(planning.action).target, routeId: 'other-fsa' }),
      }),
    }) as ResolvedArtifactAction

    await expect(route.commit({ ...commitInput(planning), action: mismatched })).rejects.toThrow(
      /installed FSA DirectTree route/u,
    )
    expect(authorizationCount).toBe(0)
  })

  it('rejects picker authority whose facts do not exactly match the installed route', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const authority = acquiredParent(parent, planning.offered)
    const mismatchedAuthority: AcquiredFSAParentAuthority = Object.freeze({
      ...authority,
      offer: Object.freeze({ ...authority.offer, hardMaximumOutputBytes: 1n }),
    })
    let authorizationCount = 0
    const route = routeFixture(
      planning.offered,
      Promise.resolve(mismatchedAuthority),
      parent,
      new TestRepository(),
      { authorizeParent: async () => { authorizationCount += 1 } },
    )

    await expect(route.ready).rejects.toThrow(/installed route identity/u)
    await expect(route.commit(commitInput(planning))).rejects.toThrow(/installed route identity/u)
    expect(authorizationCount).toBe(0)
  })

  it('treats an aborted atomic transition as pre-cut and stores nothing', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    repository.failNextTransition = true
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
    )

    await expect(route.commit(commitInput(planning))).rejects.toThrow('injected transaction abort')
    expect(repository.records.size).toBe(0)
    expect(repository.handles.size).toBe(0)
    expect(repository.leases.size).toBe(0)
    expect(parent.entryNames()).toEqual([])
    await expect(route.commit(commitInput(planning))).rejects.toThrow(/no longer available/u)
  })

  it('returns owned effects for post-cut assembly failure and settles the unopened operation', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const failingCheckpoints: FSAFileCheckpointRepositoryFactory = async () => {
      throw new DOMException('checkpoint repository unavailable', 'UnknownError')
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      { checkpointRepositoryFactory: failingCheckpoints },
    )

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    expect(repository.records.size).toBeGreaterThan(0)
    expect(parent.entryNames()).toEqual([])
    const settled = await result.authority.settleActivationFailure(result.cause)
    expect(settled.lifecycle.kind).toBe('discarded')
    await result.authority.detach()
    await result.authority.detach()
    expect(repository.leases.size).toBe(0)
    expect(repository.closeCount).toBe(1)
  })

  it('keeps ownership when cancellation lands as the atomic durable transition commits', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const attempt = new AbortController()
    repository.afterNextTransition = () => {
      attempt.abort(new DOMException('projection replacement', 'AbortError'))
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
    )

    const result = await route.commit({ ...commitInput(planning), signal: attempt.signal })
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    expect(repository.records.size).toBeGreaterThan(0)
    expect(repository.leases.size).toBe(1)
    await expect(result.authority.settleActivationFailure(result.cause)).resolves.toMatchObject({
      lifecycle: { kind: 'discarded' },
    })
    await result.authority.detach()
    expect(repository.leases.size).toBe(0)
  })

  it('keeps post-cut ownership when cancellation arrives before the bound return', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    const attempt = new AbortController()
    const checkpointsEntered = deferred<void>()
    const continueCheckpoints = deferred<void>()
    const openCheckpoints = memoryCheckpointFactory()
    const delayedCheckpoints: FSAFileCheckpointRepositoryFactory = async (binding) => {
      checkpointsEntered.resolve()
      await continueCheckpoints.promise
      return openCheckpoints(binding)
    }
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
      { checkpointRepositoryFactory: delayedCheckpoints },
    )

    const commit = route.commit({ ...commitInput(planning), signal: attempt.signal })
    await checkpointsEntered.promise
    attempt.abort(new DOMException('selection changed', 'AbortError'))
    continueCheckpoints.resolve()
    const result = await commit
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    expect(repository.records.size).toBeGreaterThan(0)
    await expect(result.authority.settleActivationFailure(result.cause)).resolves.toMatchObject({
      lifecycle: { kind: 'discarded' },
    })
    await result.authority.detach()
    expect(repository.leases.size).toBe(0)
  })

  it('persists NeedsAttention when post-cut owner verification is inconclusive', async () => {
    const planning = await planningFixture()
    const parent = new MemoryDirectory('downloads')
    const repository = new TestRepository()
    repository.hideNextCommittedLifecycle = true
    const route = routeFixture(
      planning.offered,
      Promise.resolve(acquiredParent(parent, planning.offered)),
      parent,
      repository,
    )

    const result = await route.commit(commitInput(planning))
    expect(result.kind).toBe('owned-effects')
    if (result.kind !== 'owned-effects') throw new Error('expected owned FSA effects')
    await expect(result.authority.settleActivationFailure(result.cause)).resolves.toMatchObject({
      lifecycle: { kind: 'needs-attention', reason: 'target-ownership-unknown' },
    })
    await result.authority.detach()
    expect(parent.entryNames()).toEqual([])
  })
})
