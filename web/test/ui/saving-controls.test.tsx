import { renderToString } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'
import { createSelectionSpec } from '../../src/transfer/intent'
import { offerArtifacts } from '../../src/output/planning/offers'
import { presentSavingActions, type SavingActionPresentation } from '../../src/ui/saving'
import { SavingControls } from '../../src/ui/saving/SavingControls'
import { COMPLETE_DISCOVERY, environment, fsaTarget, identity, projection, treeProof } from '../output/planning/fixture'

const TASK_ADMISSION = { operationId: 'active-download', reason: 'Another download is using the saving destination.' }

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
  it('keeps an occupied draft concise and restores the route summary when available', async () => {
    const offers = await folderOffers()
    const occupied = presentSavingActions({ offers, blockedBy: 'destinationBusy' })
    const summary = occupied.primary!.summary
    const html = renderSaving(occupied, TASK_ADMISSION)
    expect(html).toContain(TASK_ADMISSION.reason)
    expect(html).not.toContain(summary)
    expect(occupied.alternatives.flatMap(outcome => outcome.primary.summary)).toContain(summary)

    const available = presentSavingActions({ offers })
    expect(renderSaving(available, null)).toContain(summary)
    // A stale presentation context must never suppress the summary for an enabled saving action.
    expect(renderSaving(available, TASK_ADMISSION)).toContain(summary)
  })

  it('keeps limitations actionable when no eligible saving route exists', async () => {
    const offers = await folderOffers()
    const unsupported: SavingActionPresentation = {
      ...presentSavingActions({ offers }), primary: null, alternatives: [], hasAlternatives: false,
      disabledReason: 'This result cannot be saved in this browser.',
      desktopGuidance: 'Use the WindShare desktop receiver.',
    }
    const html = renderSaving(unsupported, TASK_ADMISSION)
    expect(html).toContain(unsupported.disabledReason)
    expect(html).toContain(unsupported.desktopGuidance)
  })

  it('keeps empty selection guidance at its disabled action without dumping route details', async () => {
    const model = presentSavingActions({ offers: await folderOffers(), blockedBy: 'emptySelection' })
    const html = renderSaving(model, null)
    expect(html).toContain('Select items to download')
    expect(html).not.toContain(model.primary!.summary)
  })
})
