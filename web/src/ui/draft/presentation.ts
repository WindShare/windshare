import type { ReceiveObjectLabel } from '../../output/model/operation-display'
import type { SelectionDraft } from '../model'
import { receiverCopy } from '../locales/receiver'

export function objectLabel(label: ReceiveObjectLabel, t = receiverCopy.en.browse): string {
  if (typeof label === 'string') return label
  if (label.kind === 'shared-files') return t.sharedFiles
  if (label.kind !== 'selection') return t.allItems
  const counts = [label.folders && t.folders(label.folders), label.files && t.files(label.files)].filter(Boolean)
  return counts.join(t.listSeparator) || t.selectedItems
}

export function selectionSummary(draft: SelectionDraft, t = receiverCopy.en.browse): string {
  if (draft.empty) return t.selectToDownload
  return t.selectionSummary(objectLabel(draft.label, t), draft.excludedItems)
}
