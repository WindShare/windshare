import { frozenV2SelectionPolicy, type V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { decodeBase64Url } from '../../crypto/bytes'
import type { ReceiveIntent } from '../../transfer/intent'

export function v2SelectionPolicyFromIntent(
  intent: ReceiveIntent,
): V2FrozenSelectionPolicy {
  if (intent.selection.rules.mode !== 'node-id') {
    throw new TypeError('Browser resume requires node-identity selection authority')
  }
  return frozenV2SelectionPolicy(
    intent.selection.rules.defaultSelected,
    intent.selection.rules.rules.map((rule) => {
      const id = decodeBase64Url(rule.id)
      if (id === undefined) throw new TypeError('Resumed selection rule identity is invalid')
      return Object.freeze({ kind: rule.kind, id, selected: rule.selected })
    }),
  )
}
