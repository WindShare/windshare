import { describe, expect, it } from 'vitest'
import { createSelectionSpec } from '../../src/transfer/intent'
import { offerArtifacts } from '../../src/output/planning'
import { compareSavingTransition, presentSavingActions } from '../../src/ui/saving'
import {
  COMPLETE_DISCOVERY, directZipTarget, environment, fsaTarget, handoffTarget, identity,
  nativeTarget, portableOffer, projection, runtimeDirectZipSupport, singleFileProof, treeProof, workspaceOffer,
} from '../output/planning/fixture'

async function selection() {
  return createSelectionSpec({
    shareInstance: identity(1), syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

describe('saving action presenter over eligible offers', () => {
  it('preserves the original-file short flow and exposes folder saving as a distinct outcome', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), singleFileProof(), 128n), COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], portable: portableOffer() }),
    )
    const result = presentSavingActions({ offers })
    expect(result.primary).toMatchObject({ outcome: 'original-file', label: 'Download original' })
    expect(result.primary?.offered.route.kind).toBe('portable-handoff')
    expect(result.alternatives.map(group => group.kind)).toEqual(['original-file', 'folder'])
    expect(result.alternatives[1]?.primary.recoveryPreference).toBe('automatic')
    expect(result.alternatives[1]?.primary.consequences.join(' ')).toContain('Choose a folder once')
    const direct = result.alternatives[1]?.alternatives.find(choice => choice.recoveryPreference === 'direct')
    expect(direct?.consequences.join(' ')).toContain('copy its saved prefix')
    expect(direct?.offered.choice.choiceId).toBe(result.alternatives[1]?.primary.offered.choice.choiceId)
    if (offers.kind === 'artifact-actions') expect(result.primary?.offered).toBe(offers.primary)
  })

  it.each([
    { target: fsaTarget, copyLimited: true },
    { target: nativeTarget, copyLimited: false },
  ])('separates successful pause from copy-limited crash recovery: $copyLimited', async ({ target, copyLimited }) => {
    const offers = await offerArtifacts(
      projection(await selection(), treeProof(), 1n), COMPLETE_DISCOVERY,
      environment({ targets: [target()] }),
    )
    const model = presentSavingActions({ offers })
    const choice = copyLimited
      ? model.alternatives.find(group => group.kind === 'folder')!.alternatives.find(candidate => candidate.recoveryPreference === 'direct')!
      : model.primary!
    const copy = choice.consequences.join(' ')
    expect(copy).toContain('Wait for Pause to finish')
    expect(copy.includes('may lose most progress')).toBe(copyLimited)
    expect(copy.includes('copy its saved prefix')).toBe(copyLimited)
  })

  it('keeps original-file default at large sizes when eligible instead of guessing browser capability', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), singleFileProof(), 900_000_000n), COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    )
    const result = presentSavingActions({ offers })
    expect(result.primary?.outcome).toBe('original-file')
    expect(result.primary?.consequences.join(' ')).toContain('additional exported copy')
    expect(result.primary?.consequences.join(' ')).toContain('last verified checkpoint')
    expect(result.primary?.consequences.join(' ')).not.toContain('may lose most progress')
  })

  it('defaults to hierarchy-preserving folder output and keeps ZIP format change explicit', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), treeProof(), 1024n), COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    )
    const result = presentSavingActions({ offers, actionLabel: 'Download this folder' })
    expect(result.primary?.outcome).toBe('folder')
    expect(result.primary?.label).toBe('Download this folder')
    expect(result.alternatives.find(group => group.kind === 'zip')?.primary.consequences[0]).toContain('ZIP package')
  })

  it('keeps progressive ZIP enabled when exact comparison is unknown and follows existing ZIP ranking', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), treeProof(), 1024n), { kind: 'discovering' },
      environment({
        targets: [directZipTarget(), handoffTarget()], workspace: workspaceOffer(),
        directZipSupport: runtimeDirectZipSupport(),
        zipRecommendationPolicy: { version: 1, kind: 'available', workspacePeakBytesThreshold: 1_000_000n, policyDigest: identity(89, 32) },
      }),
    )
    const result = presentSavingActions({ offers })
    expect(result.primary?.outcome).toBe('zip')
    expect(result.primary?.disabledReason).toBeNull()
    expect(result.recommendation).toContain('start now')
    expect(result.alternatives).toHaveLength(1)
    expect(result.alternatives[0]?.alternatives).toHaveLength(1)
    if (offers.kind === 'artifact-actions') expect(result.primary?.offered.choice.choiceId).toBe(offers.zip?.primary.choice.choiceId)
    expect(result.alternatives[0]?.primary.consequences.join(' ')).toContain('additional exported copy')
  })

  it('explains empty selection and blocks every action with the current admission reason', async () => {
    const spec = await selection()
    const empty = await offerArtifacts(projection(spec, { kind: 'none' }), COMPLETE_DISCOVERY, environment({ targets: [] }))
    expect(presentSavingActions({ offers: empty })).toMatchObject({
      primary: null, disabledReason: 'Select items to download',
    })
    const offers = await offerArtifacts(
      projection(spec, treeProof(), 1n), COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget()] }),
    )
    const reason = 'Finish the current download before starting another.'
    const blocked = presentSavingActions({ offers, disabledReason: reason })
    expect(blocked.primary?.disabledReason).toBe(reason)
    expect(blocked.alternatives.every(group => group.primary.disabledReason === reason)).toBe(true)
  })

  it('provides concrete desktop guidance only when no route is eligible', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), treeProof(), 1024n), COMPLETE_DISCOVERY,
      environment({ targets: [] }),
    )
    const result = presentSavingActions({ offers })
    expect(result.primary).toBeNull()
    expect(result.desktopGuidance).toContain('wind get')
  })

  it('deduplicates recommendation traces when cost bytes change without changing the decision', async () => {
    const spec = await selection()
    const env = environment({ targets: [fsaTarget()] })
    const before = presentSavingActions({
      offers: await offerArtifacts(projection(spec, treeProof(), 1n), COMPLETE_DISCOVERY, env),
    })
    const after = presentSavingActions({
      offers: await offerArtifacts(projection(spec, treeProof(), 2n), COMPLETE_DISCOVERY, env),
    })
    expect(compareSavingTransition(before.transition, after.transition)).toBeNull()
  })
})
