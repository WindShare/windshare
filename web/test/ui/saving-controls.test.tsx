import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'
import { createSelectionSpec } from '../../src/transfer/intent'
import { offerArtifacts } from '../../src/output/planning'
import { presentSavingActions, type SavingActionPresentation } from '../../src/ui/saving'
import { SavingControls } from '../../src/ui/saving/SavingControls'
import { COMPLETE_DISCOVERY, environment, fsaTarget, identity, projection, treeProof } from '../output/planning/fixture'

const TASK_ADMISSION = { operationId: 'active-download', reason: 'The current download still owns its saving destination.' }

async function folderOffers() {
  const selection = await createSelectionSpec({
    shareInstance: identity(1), syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  return offerArtifacts(projection(selection, treeProof(), 1024n), COMPLETE_DISCOVERY,
    environment({ targets: [fsaTarget()] }))
}

function renderSaving(model: SavingActionPresentation, currentTaskContext: typeof TASK_ADMISSION | null) {
  return renderToString(<SavingControls model={model} activation={null} currentTaskContext={currentTaskContext}
    actionLabel="Download this folder" choose={vi.fn()} retry={vi.fn()} cancel={vi.fn()} onIntent={vi.fn()} />)
}

describe('saving decision placement beside a current task', () => {
  it('keeps an occupied draft concise and restores every route cost before commitment is available', async () => {
    const offers = await folderOffers()
    const occupied = presentSavingActions({ offers, disabledReason: TASK_ADMISSION.reason })
    const cost = occupied.primary!.consequences[1]!
    const html = renderSaving(occupied, TASK_ADMISSION)
    expect(html).toContain(TASK_ADMISSION.reason)
    expect(html).not.toContain(cost)
    expect(html).toContain('Other ways to save')
    expect(occupied.alternatives.flatMap(outcome => outcome.primary.consequences)).toContain(cost)

    const available = presentSavingActions({ offers })
    expect(renderSaving(available, null)).toContain(cost)
    // A stale presentation context must never suppress costs for an enabled saving action.
    expect(renderSaving(available, TASK_ADMISSION)).toContain(cost)
  })

  it('keeps limitations actionable when no eligible saving route exists', async () => {
    const offers = await folderOffers()
    const unsupported: SavingActionPresentation = {
      ...presentSavingActions({ offers }), primary: null, alternatives: [],
      disabledReason: 'This result cannot be saved in this browser.',
      desktopGuidance: 'Use the WindShare desktop receiver.',
    }
    const html = renderSaving(unsupported, TASK_ADMISSION)
    expect(html).toContain(unsupported.disabledReason)
    expect(html).toContain(unsupported.desktopGuidance)
  })

  it('keeps empty selection guidance at its disabled action even while a task remains active', async () => {
    const model = presentSavingActions({ offers: await folderOffers(), disabledReason: 'Select items to download' })
    const html = renderSaving(model, null)
    expect(html).toContain('Select items to download')
    expect(html).toContain(model.primary!.consequences[1]!)
  })
})
