import { describe, expect, it, vi } from 'vitest'
import type { ArtifactChoiceID } from '../../../../src/transfer/intent'
import {
  activateFreshDirectZipTarget,
  type DirectZipBootstrapPersistencePort,
} from '../../../../src/output/direct-zip/session'
import type {
  DirectZipReservationCandidatePort,
  DirectZipTargetPort,
} from '../../../../src/output/direct-zip/target'

type Parent = Readonly<{ id: string }>
type FileHandle = Readonly<{ id: string }>

const OPERATION_ID = Uint8Array.from({ length: 16 }, (_, index) => index + 1)
const CHOICE = 'choice' as ArtifactChoiceID

describe('Direct ZIP fresh bootstrap', () => {
  it('registers the durable provisional owner before the first filesystem effect', async () => {
    const order: string[] = []
    const persistence = bootstrapPersistence(order)
    const result = await activateFreshDirectZipTarget(bootstrapInput(persistence, reservations => ({
      ...unusedTarget(),
      reserveBootstrap: async () => {
        await reservations.persistCandidate({} as never, { leaseId: 'lease', generation: 1n })
        order.push('filesystem-effect')
        return {
          kind: 'gated',
          decision: { kind: 'needs-attention', stage: 'bootstrap-close', reason: 'ownership-unknown' },
          retainedEffect: {} as never,
        }
      },
    }), authority => {
      expect(authority).toBe(persistence.provisionalAuthority)
      order.push('provisional-registered')
    }))

    expect(result.kind).toBe('owned-effects')
    expect(order).toEqual(['candidate-persisted', 'provisional-registered', 'filesystem-effect'])
  })

  it('returns retryable-precut when permission fails before candidate persistence', async () => {
    const order: string[] = []
    const persistence = bootstrapPersistence(order)
    const register = vi.fn()
    const result = await activateFreshDirectZipTarget(bootstrapInput(persistence, () => ({
      ...unusedTarget(),
      reserveBootstrap: async () => ({
        kind: 'gated',
        decision: {
          kind: 'authorization-required',
          stage: 'permission-request',
          reason: 'permission-denied',
        },
      }),
    }), register))

    expect(result.kind).toBe('retryable-precut')
    expect(register).not.toHaveBeenCalled()
    expect(order).toEqual([])
  })
})

function bootstrapPersistence(
  order: string[],
): DirectZipBootstrapPersistencePort<Parent, FileHandle, never> {
  const reservations: DirectZipReservationCandidatePort<Parent> = {
    persistCandidate: vi.fn(async () => {
      order.push('candidate-persisted')
      return { targetRef: new Uint8Array(32), bindingDigest: new Uint8Array(32) }
    }),
    retireCandidate: vi.fn(async () => undefined),
  }
  return {
    operationId: 'operation',
    operationIdBytes: OPERATION_ID,
    parentBinding: {} as never,
    reservations,
    provisionalAuthority: {
      operationId: 'operation',
      choiceId: CHOICE,
      installedRoute: { kind: 'direct', targetRouteId: 'route' },
      settleActivationFailure: vi.fn(),
      detach: vi.fn(),
    },
    commitBootstrap: vi.fn(),
  }
}

function bootstrapInput(
  persistence: DirectZipBootstrapPersistencePort<Parent, FileHandle, never>,
  createTarget: (
    reservations: DirectZipReservationCandidatePort<Parent>,
  ) => DirectZipTargetPort<Parent, FileHandle>,
  registerProvisionalOwnedEffects: Parameters<typeof activateFreshDirectZipTarget<Parent, FileHandle, never>>[0][
    'registerProvisionalOwnedEffects'
  ],
) {
  return {
    action: {
      choiceId: CHOICE,
      artifact: { kind: 'zip-archive', layout: { name: 'root' } },
      route: { kind: 'direct-resumable-zip', target: { routeId: 'route' } },
    } as never,
    preClickRanking: [CHOICE],
    currentParent: { id: 'parent' },
    policies: {
      zipEncoding: 'a', layout: 'b', checkpoint: 'c', journalBudget: 'd', epoch: 'e',
    },
    targetRouteId: 'route',
    persistence,
    createTarget,
    registerProvisionalOwnedEffects,
    freezeAtFence: vi.fn(),
    trustedAction: true,
  }
}

function unusedTarget(): DirectZipTargetPort<Parent, FileHandle> {
  return {
    reserveBootstrap: vi.fn(),
    resumeBootstrap: vi.fn(),
    reopen: vi.fn(),
    openEpoch: vi.fn(),
    truncateToPredecessor: vi.fn(),
    deleteProvenTarget: vi.fn(),
  }
}
