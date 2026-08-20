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
  AuthorityCommitTransaction,
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
