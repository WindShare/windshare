import { describe, expect, it } from 'vitest'

import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  createSelectionSpec,
} from '../../../src/transfer/intent'
import {
  createEnvironmentOffers,
  legalGuaranteeProfile,
  offerArtifacts,
  sameArtifactChoiceSemantics,
} from '../../../src/output/planning'
import type {
  DeliveryMode,
  NameAuthority,
  ReplacementGuarantee,
  RollbackGuarantee,
  CommitVisibility,
} from '../../../src/transfer/intent'
import type {
  DestinationGuaranteeFacts,
  ArtifactChoice,
  EnvironmentTargetKind,
  EnvironmentTargetOfferInput,
} from '../../../src/output/planning'
import {
  fsaTarget,
  handoffTarget,
  managedTarget,
  portableOffer,
  precreatedBrowserFileTarget,
  workspaceOffer,
  identity,
  projection,
  treeProof,
} from './fixture'

const NAME_AUTHORITIES: readonly NameAuthority[] = [
  'application-chosen', 'user-chosen', 'browser-chosen',
]
const REPLACEMENTS: readonly ReplacementGuarantee[] = [
  'atomic-no-replace', 'coordinated-no-replace', 'user-authorized-replace', 'unknown',
]
const DELIVERIES: readonly DeliveryMode[] = ['managed-target', 'browser-handoff']
const VISIBILITIES: readonly CommitVisibility[] = ['atomic-commit', 'prefix-visible', 'unobservable']
const ROLLBACKS: readonly RollbackGuarantee[] = ['to-absent', 'none']
const TARGET_KINDS: readonly EnvironmentTargetKind[] = [
  'native-directory-container',
  'fsa-parent-directory',
  'managed-atomic-file-target',
  'browser-handoff',
  'precreated-browser-file',
]

describe('environment guarantee legality', () => {
  it('classifies the closed legal rows across the full guarantee product', () => {
    const legalCounts = new Map<EnvironmentTargetKind, number>()
    for (const targetKind of TARGET_KINDS) {
      let legalCount = 0
      for (const facts of guaranteeProduct()) {
        const profile = legalGuaranteeProfile(targetKind, facts)
        if (profile !== null) legalCount += 1
        expect(profile).toBe(expectedProfile(targetKind, facts))
      }
      legalCounts.set(targetKind, legalCount)
    }

    expect(Object.fromEntries(legalCounts)).toEqual({
      'native-directory-container': 1,
      'fsa-parent-directory': 1,
      'managed-atomic-file-target': 2,
      'browser-handoff': 1,
      'precreated-browser-file': 0,
    })
  })

  it('retains showSaveFilePicker as an observed unsafe fact without offering a legal profile', () => {
    const offers = createEnvironmentOffers({ targets: [precreatedBrowserFileTarget()] })

    expect(offers.targets).toHaveLength(1)
    expect(offers.targets[0]).toMatchObject({
      kind: 'precreated-browser-file',
      legalProfile: null,
      guarantees: {
        nameAuthority: 'user-chosen',
        replacement: 'unknown',
        delivery: 'managed-target',
        visibility: 'unobservable',
        rollback: 'none',
      },
    })
  })

  it('rejects FSA overclaiming AtomicNoReplace and every UserAuthorizedReplace target', () => {
    const dishonestFSA = {
      ...fsaTarget(),
      guarantees: { ...fsaTarget().guarantees, replacement: 'atomic-no-replace' },
    } as EnvironmentTargetOfferInput
    expect(() => createEnvironmentOffers({ targets: [dishonestFSA] })).toThrow(/contradictory/u)

    for (const target of [managedTarget(), handoffTarget()]) {
      const authorizedReplace = {
        ...target,
        guarantees: { ...target.guarantees, replacement: 'user-authorized-replace' },
      } as EnvironmentTargetOfferInput
      expect(() => createEnvironmentOffers({ targets: [authorizedReplace] })).toThrow(/contradictory/u)
    }
  })

  it('separates hard limits from estimates and pins finite portable facts', () => {
    const offers = createEnvironmentOffers({
      targets: [handoffTarget()],
      workspace: { ...workspaceOffer(), quotaAvailabilityEstimateBytes: 1n },
      portable: portableOffer(),
    })

    expect(offers.workspace?.quotaAvailabilityEstimateBytes).toBe(1n)
    expect(offers.portable?.maximumArtifactBytes).toBe(DEFAULT_PORTABLE_ARTIFACT_LIMIT)
    expect(offers.portable?.objectUrlLeaseMilliseconds)
      .toBe(BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS)

    expect(() => createEnvironmentOffers({
      targets: [handoffTarget()],
      portable: { ...portableOffer(), maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT + 1n },
    })).toThrow(/bounded portable/u)
  })

  it('requires route identities to be unique across the entire installed snapshot', () => {
    expect(() => createEnvironmentOffers({
      targets: [fsaTarget('shared-route')],
      workspace: workspaceOffer('shared-route'),
    })).toThrow(/route identifiers must be unique/u)
    expect(() => createEnvironmentOffers({
      targets: [fsaTarget('duplicate'), managedTarget('duplicate')],
    })).toThrow(/route identifiers must be unique/u)
  })

  it('compares every frozen choice semantic while excluding presentation and route identity', async () => {
    const selection = await createSelectionSpec({
      shareInstance: identity(1), syntheticRoot: identity(2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })
    const first = await offerArtifacts(
      projection(selection, treeProof(), 1n),
      { kind: 'complete' },
      createEnvironmentOffers({ targets: [fsaTarget('route-a')] }),
    )
    const reprobed = await offerArtifacts(
      projection(selection, treeProof(), 1n),
      { kind: 'complete' },
      createEnvironmentOffers({ targets: [fsaTarget('route-b')] }),
    )
    if (first.kind !== 'artifact-actions' || reprobed.kind !== 'artifact-actions') {
      throw new Error('expected artifact choices')
    }
    const base = first.primary.choice
    if (base.plan.kind !== 'direct-tree') throw new Error('expected direct tree semantics')
    expect(sameArtifactChoiceSemantics(base, reprobed.primary.choice)).toBe(true)

    const mutations: readonly ArtifactChoice[] = [
      { ...base, operation: 'save-single-to-folder' },
      { ...base, artifactKind: 'original-file' },
      { ...base, recovery: 'restart-required' },
      { ...base, preparation: { ...base.preparation, manifest: 'exact-artifact' } },
      {
        ...base,
        plan: {
          ...base.plan,
          target: { ...base.plan.target, hardMaximumOutputBytes: 1n },
        } as ArtifactChoice['plan'],
      },
      {
        ...base,
        plan: {
          ...base.plan,
          target: {
            ...base.plan.target,
            guarantees: { ...base.plan.target.guarantees, rollback: 'to-absent' },
          },
        } as ArtifactChoice['plan'],
      },
    ]
    for (const mutation of mutations) {
      expect(sameArtifactChoiceSemantics(base, mutation)).toBe(false)
    }
  })
})

function guaranteeProduct(): DestinationGuaranteeFacts[] {
  const result: DestinationGuaranteeFacts[] = []
  for (const nameAuthority of NAME_AUTHORITIES) {
    for (const replacement of REPLACEMENTS) {
      for (const delivery of DELIVERIES) {
        for (const visibility of VISIBILITIES) {
          for (const rollback of ROLLBACKS) {
            result.push({ nameAuthority, replacement, delivery, visibility, rollback })
          }
        }
      }
    }
  }
  return result
}

function expectedProfile(
  kind: EnvironmentTargetKind,
  facts: DestinationGuaranteeFacts,
): ReturnType<typeof legalGuaranteeProfile> {
  if (kind === 'native-directory-container' && exact(facts, {
    nameAuthority: 'application-chosen', replacement: 'atomic-no-replace',
    delivery: 'managed-target', visibility: 'prefix-visible', rollback: 'none',
  })) return 'native-tree'
  if (kind === 'fsa-parent-directory' && exact(facts, {
    nameAuthority: 'application-chosen', replacement: 'coordinated-no-replace',
    delivery: 'managed-target', visibility: 'prefix-visible', rollback: 'none',
  })) return 'fsa-tree'
  if (kind === 'managed-atomic-file-target' &&
      (facts.nameAuthority === 'application-chosen' || facts.nameAuthority === 'user-chosen') &&
      facts.replacement === 'atomic-no-replace' && facts.delivery === 'managed-target' &&
      facts.visibility === 'atomic-commit' && facts.rollback === 'to-absent') {
    return 'managed-atomic'
  }
  if (kind === 'browser-handoff' && exact(facts, {
    nameAuthority: 'browser-chosen', replacement: 'unknown',
    delivery: 'browser-handoff', visibility: 'unobservable', rollback: 'none',
  })) return 'browser-handoff'
  return null
}

function exact(left: DestinationGuaranteeFacts, right: DestinationGuaranteeFacts): boolean {
  return left.nameAuthority === right.nameAuthority && left.replacement === right.replacement &&
    left.delivery === right.delivery && left.visibility === right.visibility &&
    left.rollback === right.rollback
}
