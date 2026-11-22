import { describe, expect, it } from 'vitest'
import { createSelectionSpec } from '../../src/transfer/intent'
import { offerArtifacts } from '../../src/output/planning/offers'
import { compareSavingTransition, presentSavingActions } from '../../src/ui/saving'
import {
  COMPLETE_DISCOVERY, environment, fsaTarget, handoffTarget, identity,
  nativeTarget, portableOffer, projection, singleFileProof, treeProof, workspaceOffer,
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
    expect(result.hasAlternatives).toBe(true)
    expect(result.alternatives.map(group => group.kind)).toEqual(['original-file', 'folder'])
    expect(result.alternatives[1]?.primary.recoveryPreference).toBe('automatic')
    expect(result.alternatives[1]?.primary.badges).toContain('Resumable')
    const direct = result.alternatives[1]?.alternatives.find(choice => choice.recoveryPreference === 'direct')
    expect(direct?.recoveryPreference).toBe('direct')
    expect(direct?.offered.choice.choiceId).toBe(result.alternatives[1]?.primary.offered.choice.choiceId)
    if (offers.kind === 'artifact-actions') expect(result.primary?.offered).toBe(offers.primary)
  })

  it('hides alternatives trigger when only a single saving option exists', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), singleFileProof(), 128n), COMPLETE_DISCOVERY,
      environment({ targets: [handoffTarget()], portable: portableOffer() }),
    )
    const result = presentSavingActions({ offers })
    expect(result.primary?.outcome).toBe('original-file')
    expect(result.hasAlternatives).toBe(false)
  })

  it.each([
    { target: fsaTarget, copyLimited: true },
    { target: nativeTarget, copyLimited: false },
  ])('distinguishes direct FSA recovery caveats from native targets: $copyLimited', async ({ target, copyLimited }) => {
    const offers = await offerArtifacts(
      projection(await selection(), treeProof(), 1n), COMPLETE_DISCOVERY,
      environment({ targets: [target()] }),
    )
    const model = presentSavingActions({ offers })
    const choice = copyLimited
      ? model.alternatives.find(group => group.kind === 'folder')!.alternatives.find(candidate => candidate.recoveryPreference === 'direct')!
      : model.primary!
    const notes = choice.notes.join(' ')
    expect(notes).toContain('Pause')
    expect(notes.includes('lose progress')).toBe(copyLimited)
  })

  it('keeps original-file default at large sizes when eligible instead of guessing browser capability', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), singleFileProof(), 900_000_000n), COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    )
    const result = presentSavingActions({ offers })
    expect(result.primary?.outcome).toBe('original-file')
    expect(result.primary?.badges).toContain('Resumable')
    expect(result.primary?.notes.join(' ')).not.toContain('lose progress')
  })

  it('defaults to hierarchy-preserving folder output and keeps ZIP format change explicit', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), treeProof(), 1024n), COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget(), handoffTarget()], workspace: workspaceOffer() }),
    )
    const result = presentSavingActions({ offers, actionLabel: 'Download this folder' })
    expect(result.primary?.outcome).toBe('folder')
    expect(result.primary?.label).toBe('Download this folder')
    expect(result.alternatives.find(group => group.kind === 'zip')?.primary.summary).toContain('ZIP')
  })

  it('keeps progressive ZIP enabled while discovery is still open', async () => {
    const offers = await offerArtifacts(
      projection(await selection(), treeProof(), 1024n), { kind: 'discovering' },
      environment({ targets: [handoffTarget()], workspace: workspaceOffer() }),
    )
    const result = presentSavingActions({ offers })
    expect(result.primary?.outcome).toBe('zip')
    expect(result.primary?.disabledReason).toBeNull()
    expect(result.recommendation).toBeNull()
    expect(result.hasAlternatives).toBe(false)
    if (offers.kind === 'artifact-actions') expect(result.primary?.offered.choice.choiceId).toBe(offers.zip?.choice.choiceId)
  })

  it('explains empty selection and blocks every action with the current admission reason', async () => {
    const spec = await selection()
    const empty = await offerArtifacts(projection(spec, { kind: 'none' }), COMPLETE_DISCOVERY, environment({ targets: [] }))
    expect(presentSavingActions({ offers: empty })).toMatchObject({
      primary: null, disabledReason: 'Select items to download', hasAlternatives: false,
    })
    const offers = await offerArtifacts(
      projection(spec, treeProof(), 1n), COMPLETE_DISCOVERY,
      environment({ targets: [fsaTarget()] }),
    )
    const reason = 'A download is in progress.'
    const blocked = presentSavingActions({ offers, blockedBy: 'downloading' })
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
