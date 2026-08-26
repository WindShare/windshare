import { describe, expect, it, vi } from 'vitest'

import {
  createIncidentScopeIssuer,
  type FailureFact,
  type FailureFactRelation,
} from '../../src/diagnostics/incident'
import {
  createAttemptOutputFailureCapability,
  recordOutputException,
  type OutputFailureSinks,
  type OutputTraceEvent,
} from '../../src/output/diagnostics'
import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
} from '../../src/output/planning'
import type {
  ReceiveOperationMutationPort,
  ReceiveOperationResumeRequest,
} from '../../src/output/resume/authority'
import type { ReceiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import type {
  AuthorityOwnedReceiveOperationMutationResult,
} from '../../src/output/resume/reopen-authority'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { createSelectionSpec } from '../../src/transfer/intent'

import {
  createBrowserReceiveComposition,
} from '../../src/ui/v2-browser-receive-composition'
import {
  COMPLETE_DISCOVERY,
  projection,
  singleFileProof,
  treeProof,
} from '../output/planning/fixture'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity as binaryIdentity,
  identityText as binaryIdentityText,
  readerFixture,
  transferJobFixture,
} from '../transfer/v2-job-fixture'
import {
  FakeResumeSource,
  capableWindow,
  directoryHandle,
  identity,
  receiveLifecycle,
  retainedLifecycles,
  testSelection,
} from './v2-browser-receive-composition-fixture'

describe('browser production receive composition', () => {
  it.each([
    ['FSA', 'direct-tree', 'directory-tree', treeProof, 0n],
    ['workspace', 'workspace-then-publish', 'zip-archive', treeProof, 0n],
    ['portable', 'portable-handoff', 'original-file', singleFileProof, 3n],
  ] as const)(
    'installs %s through the common release-before-commit authority contract',
    async (_name, routeKind, artifactKind, proof, byteCount) => {
      const picker = vi.fn(async () => directoryHandle())
      const windowPort = capableWindow(picker)
      if (routeKind === 'portable-handoff') {
        Object.defineProperty(windowPort.navigator, 'locks', { value: undefined })
      }
      const composition = createBrowserReceiveComposition(windowPort)
      const selection = await testSelection()
      const projected = projection(selection, proof(), byteCount)
      const environment = await composition.environment(new AbortController().signal)
      const offers = await offerArtifacts(projected, COMPLETE_DISCOVERY, environment)
      if (offers.kind !== 'artifact-actions') throw new Error('route choice was not offered')
      const offered = [offers.primary, ...offers.alternatives].find(candidate =>
        candidate.route.kind === routeKind && candidate.choice.artifactKind === artifactKind)
      if (offered === undefined) throw new Error(`${routeKind} choice was not offered`)
      const resolution = await reconcileArtifactChoice({
        choice: offered.choice,
        preferredRoute: materializationRouteIdentity(offered.route),
        expectedSelectionDigest: selection.digest,
        projection: projected,
        discovery: COMPLETE_DISCOVERY,
        environment,
        previousObservation: null,
      })
      if (resolution.kind !== 'resolved') throw new Error(`${routeKind} choice did not resolve`)
      const authority = composition.startArtifactAuthority(offered, [offered.choice.choiceId])
      await authority.ready
      const freezeAtFence = vi.fn()

      await authority.release('common contract cancellation')

      await expect(authority.commit({
        action: resolution.action,
        signal: new AbortController().signal,
        freezeAtFence,
      })).rejects.toBeInstanceOf(DOMException)
      expect(freezeAtFence).not.toHaveBeenCalled()
    },
  )

  it('advertises only complete installed route assemblies and never probes with a picker', async () => {
    const picker = vi.fn(async () => directoryHandle())
    const windowPort = capableWindow(picker)
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(picker).not.toHaveBeenCalled()
    expect(environment.targets.map(target => target.kind)).toEqual([
      'fsa-parent-directory',
      'browser-handoff',
    ])
    expect(environment.targets.some(target => target.kind === 'managed-atomic-file-target')).toBe(false)
    expect(environment.workspace?.kind).toBe('origin-private-workspace')
    expect(environment.portable?.kind).toBe('portable-memory')
  })

  it('keeps installed routes available when quota estimation is absent', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    Object.defineProperty(windowPort.navigator.storage, 'estimate', { value: undefined })
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(environment.targets.map(target => target.kind)).toEqual([
      'fsa-parent-directory',
      'browser-handoff',
    ])
    expect(environment.workspace?.quotaAvailabilityEstimateBytes).toBeNull()
    expect(environment.portable?.kind).toBe('portable-memory')
  })

  it('offers only portable handoff when picker and origin-private storage are unavailable', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    Object.defineProperty(windowPort, 'showDirectoryPicker', { value: undefined })
    Object.defineProperty(windowPort.navigator.storage, 'getDirectory', { value: undefined })
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(environment.targets.map(target => target.kind)).toEqual(['browser-handoff'])
    expect(environment.workspace).toBeNull()
    expect(environment.portable?.kind).toBe('portable-memory')
  })

  it('keeps installed routes available when quota estimation rejects', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    vi.mocked(windowPort.navigator.storage.estimate)
      .mockRejectedValueOnce(new DOMException('estimate unavailable', 'NotReadableError'))
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(environment.targets.map(target => target.kind)).toEqual([
      'fsa-parent-directory',
      'browser-handoff',
    ])
    expect(environment.workspace?.quotaAvailabilityEstimateBytes).toBeNull()
    expect(environment.portable?.kind).toBe('portable-memory')
  })

  it.each([
    ['missing usage', { quota: 8_192 }],
    ['non-finite usage', { usage: Number.NaN, quota: 8_192 }],
    ['negative quota', { usage: 0, quota: -1 }],
    ['unsafe quota', { usage: 0, quota: Number.MAX_SAFE_INTEGER + 1 }],
  ])('keeps installed routes available for an invalid quota estimate: %s', async (_case, estimate) => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    vi.mocked(windowPort.navigator.storage.estimate).mockResolvedValueOnce(estimate)
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(environment.targets.map(target => target.kind)).toEqual([
      'fsa-parent-directory',
      'browser-handoff',
    ])
    expect(environment.workspace?.quotaAvailabilityEstimateBytes).toBeNull()
    expect(environment.portable?.kind).toBe('portable-memory')
  })

  it('rejects an already-aborted environment probe before observing quota', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    const estimate = vi.mocked(windowPort.navigator.storage.estimate)
    const composition = createBrowserReceiveComposition(windowPort)
    const controller = new AbortController()
    controller.abort()

    await expect(composition.environment(controller.signal)).rejects.toMatchObject({ name: 'AbortError' })
    expect(estimate).not.toHaveBeenCalled()
  })

  it('keeps route identities stable across successful and failed quota probes', async () => {
    const picker = vi.fn(async () => directoryHandle())
    const windowPort = capableWindow(picker)
    const estimate = vi.mocked(windowPort.navigator.storage.estimate)
    estimate
      .mockResolvedValueOnce({ usage: 1_024, quota: 8_192 })
      .mockRejectedValueOnce(new DOMException('estimate unavailable', 'NotReadableError'))
      .mockResolvedValueOnce({ usage: 2_048, quota: 16_384 })
    const composition = createBrowserReceiveComposition(windowPort)
    const first = await composition.environment(new AbortController().signal)
    const second = await composition.environment(new AbortController().signal)
    const third = await composition.environment(new AbortController().signal)

    expect(first.workspace?.quotaAvailabilityEstimateBytes).toBe(7_168n)
    expect(second.workspace?.quotaAvailabilityEstimateBytes).toBeNull()
    expect(third.workspace?.quotaAvailabilityEstimateBytes).toBe(14_336n)
    expect([second.workspace?.routeId, third.workspace?.routeId])
      .toEqual([first.workspace?.routeId, first.workspace?.routeId])
    expect([second.portable?.routeId, third.portable?.routeId])
      .toEqual([first.portable?.routeId, first.portable?.routeId])
    expect(second.targets.map(target => target.routeId))
      .toEqual(first.targets.map(target => target.routeId))
    expect(third.targets.map(target => target.routeId))
      .toEqual(first.targets.map(target => target.routeId))

    const selection = await testSelection()
    const projected = projection(selection, treeProof())
    const offers = await offerArtifacts(projected, COMPLETE_DISCOVERY, first)
    if (offers.kind !== 'artifact-actions') throw new Error('workspace choice was not offered')
    const offered = [offers.primary, ...offers.alternatives].find(candidate =>
      candidate.route.kind === 'workspace-then-publish')
    if (offered === undefined || offered.route.kind !== 'workspace-then-publish') {
      throw new Error('workspace choice was not offered')
    }
    const authority = composition.startArtifactAuthority(offered, [offered.choice.choiceId])
    await authority.ready
    await authority.release('stable route identity proven')

    const foreign = Object.freeze({
      ...offered,
      route: Object.freeze({
        ...offered.route,
        workspace: Object.freeze({
          ...offered.route.workspace,
          routeId: `${offered.route.workspace.routeId}-replacement`,
        }),
      }),
    })
    expect(() => composition.startArtifactAuthority(foreign, [foreign.choice.choiceId]))
      .toThrowError(DOMException)
    expect(picker).not.toHaveBeenCalled()
  })

  it('removes FSA, workspace, and portable offers when their production registry owner is absent', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    Object.defineProperty(windowPort, 'indexedDB', { value: undefined })
    const composition = createBrowserReceiveComposition(windowPort)

    const environment = await composition.environment(new AbortController().signal)

    expect(environment.targets.map(target => target.kind)).toEqual(['browser-handoff'])
    expect(environment.workspace).toBeNull()
    expect(environment.portable).toBeNull()
  })

  it('projects every retained v6 continuation without starting picker or handoff authority', async () => {
    const picker = vi.fn(async () => directoryHandle())
    const windowPort = capableWindow(picker)
    const source = new FakeResumeSource(retainedLifecycles())
    const composition = createBrowserReceiveComposition(windowPort, {
      openResumeSource: async () => source,
      now: () => 1_000,
    })

    const inventory = await composition.retained.list(new AbortController().signal)
    const { operations } = inventory
    const continuations = Object.fromEntries(operations.map((operation) => [
      operation.lifecycle.kind,
      operation.continuation,
    ]))

    expect(continuations).toEqual({
      'download-started': 'retry-download',
      expired: 'cleanup-expired',
      'needs-attention': 'needs-attention',
      published: 'restoration-available',
      'resumable-package': 'resume-package',
      'resumable-receive': 'resume-receive',
      'waiting-to-save': 'save-artifact',
    })
    expect(operations).toHaveLength(8)
    expect(operations[0]?.lifecycle).not.toHaveProperty('verifiedRanges')
    expect(operations.every(operation => operation.actions.length === 0)).toBe(true)
    expect(source.closeCalls).toBe(0)
    inventory.close()
    expect(source.closeCalls).toBe(1)
    expect(picker).not.toHaveBeenCalled()
    expect(windowPort.URL.createObjectURL).not.toHaveBeenCalled()
  })

  it('turns an elapsed stable deadline into cleanup-only inventory', async () => {
    const source = new FakeResumeSource([receiveLifecycle(20, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(21, 32),
      completedFileCount: 3n,
      completedBytes: 512n,
      expiresAt: 1_000,
    })])
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      { openResumeSource: async () => source, now: () => 1_000 },
    )

    const inventory = await composition.retained.list(new AbortController().signal)

    expect(inventory.operations).toMatchObject([{
      continuation: 'cleanup-expired',
      expiresAt: 1_000,
      lifecycleGeneration: 1n,
      actions: [],
    }])
    expect(source.closeCalls).toBe(0)
    inventory.close()
    expect(source.closeCalls).toBe(1)
  })

  it('keeps the live resume reference until expiry cleanup consumes the exact inventory token', async () => {
    const source = new FakeResumeSource([receiveLifecycle(22, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(23, 32),
      completedFileCount: 1n,
      completedBytes: 64n,
      expiresAt: 1_000,
    })])
    const expiredDescriptors: ReceiveOperationResumeDescriptor[] = []
    const expire = vi.fn(async (descriptor: ReceiveOperationResumeDescriptor) => {
      expiredDescriptors.push(descriptor)
      return Object.freeze({
        kind: 'retention-cleanup',
        result: Object.freeze({ kind: 'already-absent' }),
      }) as AuthorityOwnedReceiveOperationMutationResult
    })
    const discard = vi.fn(async () => Object.freeze({ kind: 'already-absent' } as const))
    const mutations: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> =
      Object.freeze({
        resume: () => Promise.reject(new Error('unexpected resume')),
        expire,
        discard,
      })
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutations,
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('expired operation was not projected')

    expect(operation.actions).toEqual(['delete'])
    await expect(inventory.act(
      Object.freeze({ ...operation }),
      'delete',
      new AbortController().signal,
    )).rejects.toMatchObject({ name: 'InvalidStateError' })
    expect(expire).not.toHaveBeenCalled()

    await inventory.act(operation, 'delete', new AbortController().signal)

    expect(expire).toHaveBeenCalledTimes(1)
    expect(expiredDescriptors[0]?.operationId).toBe(operation.operationId)
    expect(discard).not.toHaveBeenCalled()
    expect(source.closeCalls).toBe(0)
    inventory.close()
    expect(source.closeCalls).toBe(1)
  })

  it('enables Continue and authority-owned discard only when mutation authority is installed', async () => {
    const source = new FakeResumeSource([receiveLifecycle(26, {
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: identity(27, 32),
      completedFileCount: 1n,
      completedBytes: 64n,
      expiresAt: 5_000,
    })])
    const discardedDescriptors: ReceiveOperationResumeDescriptor[] = []
    const discard = vi.fn(async (descriptor: ReceiveOperationResumeDescriptor) => {
      discardedDescriptors.push(descriptor)
      return Object.freeze({ kind: 'already-absent' } as const)
    })
    const mutations: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> =
      Object.freeze({
        resume: () => Promise.reject(new Error('unexpected resume')),
        expire: () => Promise.reject(new Error('unexpected expiry')),
        discard,
      })
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutations,
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const operation = inventory.operations[0]
    if (operation === undefined) throw new Error('resumable operation was not projected')

    expect(operation.actions).toEqual(['continue', 'discard'])
    expect(operation.unavailableReason).toBeUndefined()
    await inventory.act(operation, 'discard', new AbortController().signal)

    expect(discardedDescriptors[0]?.lifecycle).toBe(operation.lifecycle)
    inventory.close()
  })

})


describe('browser output attempt ownership', () => {
  it('keeps retained reopen and cleanup evidence inside the exact action attempts', async () => {
    const reopenFailure = new DOMException('private reopen detail', 'AbortError')
    const cleanupFailure = new DOMException('private cleanup detail', 'InvalidStateError')
    const source = new FakeResumeSource([
      receiveLifecycle(48, {
        kind: 'resumable-receive',
      payloadKind: 'file-set',
        checkpointSetDigest: identity(49, 32),
        completedFileCount: 1n,
        completedBytes: 64n,
        expiresAt: 5_000,
      }),
      receiveLifecycle(50, {
        kind: 'waiting-to-save',
        packageDigest: identity(51, 32),
        expiresAt: 5_000,
      }),
    ])
    const mutations: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> =
      Object.freeze({
        resume: (
          _descriptor: ReceiveOperationResumeDescriptor,
          request?: ReceiveOperationResumeRequest,
        ) => {
          recordOutputException(request?.failures?.reopen, reopenFailure)
          return Promise.reject(reopenFailure)
        },
        expire: () => Promise.reject(new Error('unexpected expiry')),
        discard: (
          _descriptor: ReceiveOperationResumeDescriptor,
          failures?: OutputFailureSinks,
        ) => {
          recordOutputException(failures?.cleanup, cleanupFailure)
          return Promise.resolve(Object.freeze({ kind: 'already-absent' as const }))
        },
      })
    const composition = createBrowserReceiveComposition(
      capableWindow(vi.fn(async () => directoryHandle())),
      {
        openResumeSource: async () => source,
        resumeMutations: mutations,
        now: () => 1_000,
      },
    )
    const inventory = await composition.retained.list(new AbortController().signal)
    const resumable = inventory.operations.find(
      operation => operation.continuation === 'resume-receive',
    )
    const discardable = inventory.operations.find(
      operation => operation.continuation === 'save-artifact',
    )
    if (resumable === undefined || discardable === undefined) {
      throw new Error('retained action fixtures were not projected')
    }

    const issuer = createIncidentScopeIssuer()
    const observed: Array<Array<Readonly<{
      fact: FailureFact
      relation: FailureFactRelation
    }>>> = [[], []]
    const attempts = observed.map((facts) => {
      const scope = issuer.open('retained_action', {
        factRecorded: observation => facts.push({
          fact: observation.fact,
          relation: observation.relation,
        }),
      })
      return {
        scope,
        failures: createAttemptOutputFailureCapability(scope.handle),
      }
    })

    await expect(inventory.act(
      resumable,
      'continue',
      new AbortController().signal,
      attempts[0]!.failures.sinks,
    )).rejects.toBe(reopenFailure)
    await expect(inventory.act(
      discardable,
      'discard',
      new AbortController().signal,
      attempts[1]!.failures.sinks,
    )).resolves.toEqual({ kind: 'completed' })

    expect(observed).toMatchObject([
      [{
        fact: {
          kind: 'native_output_failure',
          stage: 'reopen',
          payload: { nativeOutputFailure: { nativeClass: 'abort' } },
        },
        relation: 'contributor',
      }],
      [{
        fact: {
          kind: 'native_output_failure',
          stage: 'cleanup',
          payload: { nativeOutputFailure: { nativeClass: 'invalid_state' } },
        },
        relation: 'consequence',
      }],
    ])
    expect(attempts[0]!.scope.identity).not.toEqual(attempts[1]!.scope.identity)
    for (const attempt of attempts) {
      attempt.failures.revoke()
      attempt.scope.close()
    }
    inventory.close()
  })

  it('starts exactly one FSA picker synchronously in the explicit action stack', async () => {
    let inActionStack = true
    const calls: boolean[] = []
    const picker = vi.fn(async () => {
      calls.push(inActionStack)
      return directoryHandle()
    })
    const windowPort = capableWindow(picker)
    const composition = createBrowserReceiveComposition(windowPort)
    const selection = await testSelection()
    const environment = await composition.environment(new AbortController().signal)
    const offered = await offerArtifacts(
      projection(selection, treeProof()),
      COMPLETE_DISCOVERY,
      environment,
    )
    if (offered.kind !== 'artifact-actions') throw new Error('tree action was not offered')
    const choice = [offered.primary, ...offered.alternatives]
      .find(candidate => candidate.route.kind === 'direct-tree')
    if (choice === undefined) throw new Error('FSA DirectTree choice was not offered')

    const authority = composition.startArtifactAuthority(choice, [choice.choice.choiceId])
    inActionStack = false

    expect(picker).toHaveBeenCalledTimes(1)
    expect(calls).toEqual([true])
    await authority.ready
    await authority.release('test completed')
  })

  it('classifies a synchronous picker refusal without retrying the authority source', async () => {
    const refusal = new DOMException('user refused', 'AbortError')
    const picker = vi.fn(() => { throw refusal })
    const composition = createBrowserReceiveComposition(capableWindow(picker))
    const selection = await testSelection()
    const environment = await composition.environment(new AbortController().signal)
    const offers = await offerArtifacts(
      projection(selection, treeProof()),
      COMPLETE_DISCOVERY,
      environment,
    )
    if (offers.kind !== 'artifact-actions') throw new Error('tree choice was not offered')
    const choice = [offers.primary, ...offers.alternatives]
      .find(candidate => candidate.route.kind === 'direct-tree')
    if (choice === undefined) throw new Error('FSA DirectTree choice was not offered')

    expect(() => composition.startArtifactAuthority(choice, [choice.choice.choiceId]))
      .toThrowError(expect.objectContaining({
      outcome: 'picker_refused',
      cause: refusal,
    }))
    expect(picker).toHaveBeenCalledTimes(1)
  })

  it('binds native picker failures to the exact production authority attempt', async () => {
    const picker = vi.fn()
      .mockRejectedValueOnce(new DOMException('private refusal', 'AbortError'))
      .mockRejectedValueOnce(new DOMException('private reservation', 'QuotaExceededError'))
    const windowPort = capableWindow(picker)
    const composition = createBrowserReceiveComposition(windowPort)
    const selection = await testSelection()
    const environment = await composition.environment(new AbortController().signal)
    const offered = await offerArtifacts(
      projection(selection, treeProof()),
      COMPLETE_DISCOVERY,
      environment,
    )
    if (offered.kind !== 'artifact-actions') throw new Error('tree action was not offered')
    const choice = [offered.primary, ...offered.alternatives]
      .find(candidate => candidate.route.kind === 'direct-tree')
    if (choice === undefined) throw new Error('FSA DirectTree choice was not offered')

    const issuer = createIncidentScopeIssuer()
    const observed: Array<Array<Readonly<{
      fact: FailureFact
      relation: FailureFactRelation
    }>>> = [[], []]
    const attempts = observed.map((facts) => {
      const scope = issuer.open('authority_activation', {
        factRecorded: observation => facts.push({
          fact: observation.fact,
          relation: observation.relation,
        }),
      })
      return {
        scope,
        failures: createAttemptOutputFailureCapability(scope.handle),
      }
    })

    const first = composition.startArtifactAuthority(
      choice,
      [choice.choice.choiceId],
      attempts[0]!.failures.sinks,
    )
    await expect(first.ready).rejects.toMatchObject({ outcome: 'picker_refused' })

    const second = composition.startArtifactAuthority(
      choice,
      [choice.choice.choiceId],
      attempts[1]!.failures.sinks,
    )
    await expect(second.ready).rejects.toBeInstanceOf(DOMException)

    expect(observed).toMatchObject([
      [{
        fact: {
          kind: 'native_output_failure',
          stage: 'output_reservation',
          payload: { nativeOutputFailure: { nativeClass: 'abort' } },
        },
        relation: 'contributor',
      }],
      [{
        fact: {
          kind: 'native_output_failure',
          stage: 'output_reservation',
          payload: { nativeOutputFailure: { nativeClass: 'quota_exceeded' } },
        },
        relation: 'contributor',
      }],
    ])
    expect(attempts[0]!.scope.identity).not.toEqual(attempts[1]!.scope.identity)
    for (const attempt of attempts) {
      attempt.failures.revoke()
      attempt.scope.close()
    }
  })

  it('runs a timestamped portable artifact through the real operation-bound plan authority', async () => {
    const windowPort = capableWindow(vi.fn(async () => directoryHandle()))
    Object.defineProperty(windowPort.navigator, 'locks', { value: undefined })
    const outputTraceEvents: OutputTraceEvent[] = []
    const composition = createBrowserReceiveComposition(windowPort, {
      outputTrace: {
        current: event => outputTraceEvents.push(event),
      },
    })
    const selection = await createSelectionSpec({
      shareInstance: binaryIdentityText(1),
      syntheticRoot: binaryIdentityText(2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })
    const environment = await composition.environment(new AbortController().signal)
    const state = projection(selection, {
      kind: 'single-file',
      file: {
        fileId: binaryIdentityText(20),
        sourcePath: 'docs/report.txt',
        portableName: 'report.txt',
      },
    }, 3n)
    const offered = await offerArtifacts(state, COMPLETE_DISCOVERY, environment)
    if (offered.kind !== 'artifact-actions') throw new Error('file action was not offered')
    const choice = [offered.primary, ...offered.alternatives]
      .find(candidate => candidate.route.kind === 'portable-handoff')
    if (choice === undefined) throw new Error('portable choice was not offered')
    const resolution = await reconcileArtifactChoice({
      choice: choice.choice,
      preferredRoute: materializationRouteIdentity(choice.route),
      expectedSelectionDigest: selection.digest,
      projection: state,
      discovery: COMPLETE_DISCOVERY,
      environment,
      previousObservation: null,
    })
    if (resolution.kind !== 'resolved') throw new Error('portable choice did not resolve')
    const authority = composition.startArtifactAuthority(choice, [choice.choice.choiceId])
    await authority.ready
    const committed = await authority.commit({
      action: resolution.action,
      signal: new AbortController().signal,
      freezeAtFence: candidate => bindReceiveIntent({
        selection,
        action: resolution.action,
        candidate,
      }),
    })
    if (committed.kind !== 'bound-operation') {
      throw new Error('portable route unexpectedly retained durable effects')
    }
    const runtime = committed.operation

    expect(runtime.intent.plan.kind).toBe('portable-handoff')
    expect(runtime.transferJobId).toHaveLength(22)
    expect(runtime.plans.preparePortable).toBeTypeOf('function')
    expect(runtime.lifecycle.kind).toBe('intent-frozen')

    const file = Object.freeze({
      ...fileEntry(binaryIdentity(20), 'report.txt', 3n),
      modifiedTime: Object.freeze({
        seconds: 1_700_000_000n,
        nanoseconds: 123_000_000,
        precision: 3 as const,
        milliseconds: 1_700_000_000_123n,
      }),
    })
    const docs = directoryEntry(binaryIdentity(30), 'docs')
    const { catalog } = catalogFixture([
      { id: binaryIdentity(2), entries: [docs] },
      { id: docs.id, entries: [file] },
    ])
    const readers = readerFixture([file])
    const createObjectURL = vi.mocked(windowPort.URL.createObjectURL)
    const objectURLCallsBeforeAttempt = createObjectURL.mock.calls.length
    const result = await transferJobFixture({
      catalog,
      selection: new V2SelectionPolicy(true),
      intent: runtime.intent,
      plans: runtime.plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe('download-started')
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).not.toHaveLength(0)
    expect(outputTraceEvents).toEqual(expect.arrayContaining([
      {
        eventName: 'output_write',
        payload: {
          backend: 'portable',
          transition: 'transaction_committed',
        },
      },
      {
        eventName: 'publication',
        payload: {
          backend: 'portable',
          transition: 'committed',
        },
      },
    ]))
    expect(outputTraceEvents).not.toEqual(expect.arrayContaining([
      expect.objectContaining({
        payload: expect.objectContaining({ transition: 'transaction_failed' }),
      }),
    ]))
    expect(createObjectURL.mock.calls).toHaveLength(objectURLCallsBeforeAttempt + 1)
    const [publishedArtifact] = createObjectURL.mock.calls.at(-1) ?? []
    if (!(publishedArtifact instanceof Blob)) {
      throw new TypeError('portable attempt did not publish a Blob-backed artifact')
    }
    expect(publishedArtifact.size).toBe(3)
    await runtime.detach()
  })
})
