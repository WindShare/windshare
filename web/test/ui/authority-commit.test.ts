import { describe, expect, it } from 'vitest'

import {
  bindReceiveIntent,
  materializationRouteIdentity,
  offerArtifacts,
  reconcileArtifactChoice,
} from '../../src/output/planning'
import { initialReceiveLifecycleState } from '../../src/output/workspace'
import { createSelectionSpec } from '../../src/transfer/intent'
import {
  advanceActivationCleanup,
  AuthorityCommitTransaction,
  provisionalOwnedEffectsCleanup,
} from '../../src/ui/controller/authority-commit'
import type {
  AuthorityCommitRoute,
  ProvisionalOwnedEffectAuthority,
} from '../../src/ui/controller/authority-commit'
import { V2ActivationStateContractError } from '../../src/ui/controller/activation-model'
import type { V2ArtifactPresentationAuthority } from '../../src/ui/v2-receive-runtime'
import {
  COMPLETE_DISCOVERY,
  projection,
  singleFileProof,
} from '../output/planning/fixture'
import {
  FakeBoundRuntime,
  FakeJoinedShare,
  MANAGED_ENVIRONMENT,
  candidateBindingForTest,
  identityText,
} from './v2-receiver-orchestration-fixture'

describe('authority commit transaction', () => {
  it('promotes pre-intent ownership only after the same choice, route, and operation cross the fence', async () => {
    const fixture = await commitFixture()
    const provisional = provisionalAuthority(fixture.action)
    const authority: AuthorityCommitRoute = {
      commit: async input => {
        input.registerProvisionalOwnedEffects(provisional)
        const candidate = await candidateBindingForTest(input.action, 'report.txt')
        const frozen = await input.freezeAtFence(candidate)
        return { kind: 'bound-operation', operation: new FakeBoundRuntime(frozen.intent) }
      },
    }
    const transaction = new AuthorityCommitTransaction({
      action: fixture.action,
      observationRevision: 1,
      authority,
      assertFinalFence: () => fixture.selection,
    })

    await expect(transaction.run()).resolves.toMatchObject({ kind: 'bound-operation' })
    expect(transaction.hasProvisionalOwnedEffects).toBe(false)
    expect(transaction.takeProvisionalOwnedEffects(new Error('late cleanup'))).toBeUndefined()
  })

  it('retains provisional ownership when a route falsely reports a no-effect retry', async () => {
    const fixture = await commitFixture()
    const settled: unknown[] = []
    let detached = 0
    const provisional = provisionalAuthority(fixture.action, {
      settleActivationFailure: reason => {
        settled.push(reason)
        return Promise.resolve()
      },
      detach: () => { detached += 1 },
    })
    const authority: AuthorityCommitRoute = {
      commit: async input => {
        input.registerProvisionalOwnedEffects(provisional)
        return { kind: 'retryable-precut' }
      },
    }
    const transaction = new AuthorityCommitTransaction({
      action: fixture.action,
      observationRevision: 1,
      authority,
      assertFinalFence: () => fixture.selection,
    })

    await expect(transaction.run()).rejects.toMatchObject({
      message: 'route reported a pre-cut retry while provisional effects remain owned',
    })
    expect(transaction.hasProvisionalOwnedEffects).toBe(true)
    const cause = new Error('bootstrap failed')
    const effects = transaction.takeProvisionalOwnedEffects(cause)
    if (effects === undefined) throw new Error('provisional effects were lost')
    const cleanup = provisionalOwnedEffectsCleanup(effects, {
      kind: 'owned-effects-settled',
      operationId: provisional.operationId,
    })

    await expect(advanceActivationCleanup(cleanup)).resolves.toEqual({
      failedStages: [],
      detachedNow: true,
      complete: true,
    })
    expect(settled).toEqual([cause])
    expect(detached).toBe(1)
    expect(transaction.takeProvisionalOwnedEffects(cause)).toBeUndefined()
  })

  it('rejects provisional ownership from a post-click route substitution', async () => {
    const fixture = await commitFixture()
    const provisional = provisionalAuthority(fixture.action, {
      installedRoute: { kind: 'direct', targetRouteId: 'substituted-route' },
    })
    const authority: AuthorityCommitRoute = {
      commit: async input => {
        input.registerProvisionalOwnedEffects(provisional)
        throw new Error('unreachable')
      },
    }
    const transaction = new AuthorityCommitTransaction({
      action: fixture.action,
      observationRevision: 1,
      authority,
      assertFinalFence: () => fixture.selection,
    })

    await expect(transaction.run()).rejects.toMatchObject({
      message: 'provisional effects do not belong to the installed materialization route',
    })
    expect(transaction.hasProvisionalOwnedEffects).toBe(false)
  })

  it('freezes one intent and validates the returned bound operation', async () => {
    const fixture = await commitFixture()
    let fenceChecks = 0
    const authority: V2ArtifactPresentationAuthority = {
      ready: Promise.resolve(),
      commit: async input => {
        const candidate = await candidateBindingForTest(input.action, 'report.txt')
        const frozen = await input.freezeAtFence(candidate)
        return { kind: 'bound-operation', operation: new FakeBoundRuntime(frozen.intent) }
      },
      release: () => undefined,
    }
    const transaction = new AuthorityCommitTransaction({
      action: fixture.action,
      observationRevision: 7,
      authority,
      assertFinalFence: () => {
        fenceChecks += 1
        return fixture.selection
      },
    })

    const outcome = await transaction.run()

    expect(outcome).toMatchObject({ kind: 'bound-operation' })
    expect(transaction.fenced).toBe(true)
    expect(fenceChecks).toBe(2)
    await expect(transaction.run()).rejects.toBeInstanceOf(V2ActivationStateContractError)
  })

  it('rejects a route that requests the once-only final fence twice', async () => {
    const fixture = await commitFixture()
    const authority: V2ArtifactPresentationAuthority = {
      ready: Promise.resolve(),
      commit: async input => {
        const candidate = await candidateBindingForTest(input.action, 'report.txt')
        await input.freezeAtFence(candidate)
        await input.freezeAtFence(candidate)
        throw new Error('unreachable')
      },
      release: () => undefined,
    }
    const transaction = new AuthorityCommitTransaction({
      action: fixture.action,
      observationRevision: 1,
      authority,
      assertFinalFence: () => fixture.selection,
    })

    await expect(transaction.run()).rejects.toMatchObject({
      message: 'route requested the final intent fence twice',
    })
  })

  it('rejects a bound lifecycle that differs from the frozen intent', async () => {
    const fixture = await commitFixture()
    const authority: V2ArtifactPresentationAuthority = {
      ready: Promise.resolve(),
      commit: async input => {
        const candidate = await candidateBindingForTest(input.action, 'report.txt')
        const frozen = await input.freezeAtFence(candidate)
        const lifecycle = initialReceiveLifecycleState({
          operationId: identityText(99),
          receiveIntentDigest: frozen.intent.digest,
        })
        return {
          kind: 'bound-operation',
          operation: new FakeBoundRuntime(frozen.intent, lifecycle),
        }
      },
      release: () => undefined,
    }
    const transaction = new AuthorityCommitTransaction({
      action: fixture.action,
      observationRevision: 1,
      authority,
      assertFinalFence: () => fixture.selection,
      binder: bindReceiveIntent,
    })

    await expect(transaction.run()).rejects.toMatchObject({
      message: 'bound receive operation does not match the coordinator-frozen intent',
    })
  })
})

async function commitFixture() {
  const joined = new FakeJoinedShare(true)
  const selection = await createSelectionSpec({
    shareInstance: joined.descriptor.shareInstanceId,
    syntheticRoot: joined.descriptor.syntheticRootId,
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const projected = projection(selection, singleFileProof(), 128n)
  const offers = await offerArtifacts(projected, COMPLETE_DISCOVERY, MANAGED_ENVIRONMENT)
  if (offers.kind !== 'artifact-actions') throw new Error('fixture did not offer an artifact')
  const outcome = await reconcileArtifactChoice({
    choice: offers.primary.choice,
    preferredRoute: materializationRouteIdentity(offers.primary.route),
    expectedSelectionDigest: selection.digest,
    projection: projected,
    discovery: COMPLETE_DISCOVERY,
    environment: MANAGED_ENVIRONMENT,
    previousObservation: null,
  })
  if (outcome.kind !== 'resolved') throw new Error('fixture did not resolve an artifact')
  return { selection, action: outcome.action }
}

function provisionalAuthority(
  action: Awaited<ReturnType<typeof commitFixture>>['action'],
  overrides: Partial<ProvisionalOwnedEffectAuthority> = {},
): ProvisionalOwnedEffectAuthority {
  return {
    operationId: identityText(40),
    choiceId: action.choiceId,
    installedRoute: materializationRouteIdentity(action.route),
    settleActivationFailure: () => Promise.resolve(),
    detach: () => undefined,
    ...overrides,
  }
}
