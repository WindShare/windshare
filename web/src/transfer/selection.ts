import { decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import {
  frozenSelectionPolicy,
  type CanonicalSelectionRule,
  type FrozenSelectionPolicy,
} from '../catalog/selection'

export type ArtifactKind = 'original-file' | 'directory-tree' | 'zip-archive'

export interface SelectionRuleRecord {
  readonly kind: 'directory' | 'file'
  readonly id: string
  readonly selected: boolean
}

/** Plain-JSON selection specification persisted directly in IndexedDB without binary codecs. */
export interface SelectionSpec {
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly defaultSelected: boolean
  readonly rules: readonly SelectionRuleRecord[]
}

export interface ResolvedArtifactSpec {
  readonly kind: ArtifactKind
  readonly name: string
  /** Set for single-file downloads so single-file layouts avoid an extra wrapper folder. */
  readonly singleFileId?: string
  readonly singleFilePath?: readonly string[]
  readonly singleFileSize?: bigint
}

const TASK_ID_BYTES = 16

export function createTaskId(): string {
  const bytes = new Uint8Array(TASK_ID_BYTES)
  crypto.getRandomValues(bytes)
  if (bytes.every(byte => byte === 0)) bytes[0] = 1
  return encodeBase64Url(bytes)
}

export function selectionSpecFromPolicy(
  shareInstance: string,
  syntheticRoot: string,
  policy: FrozenSelectionPolicy,
): SelectionSpec {
  const rules = policy.canonicalRules
    .map((rule): SelectionRuleRecord => Object.freeze({
      kind: rule.kind,
      id: encodeBase64Url(rule.id),
      selected: rule.selected,
    }))
    .sort((left, right) => left.id.localeCompare(right.id) || left.kind.localeCompare(right.kind))
  return Object.freeze({
    shareInstance,
    syntheticRoot,
    defaultSelected: policy.defaultSelected,
    rules: Object.freeze(rules),
  })
}

export function policyFromSelectionSpec(spec: SelectionSpec): FrozenSelectionPolicy {
  const canonicalRules: CanonicalSelectionRule[] = spec.rules.map(rule => {
    const id = decodeBase64Url(rule.id)
    if (id === undefined) throw new TypeError('Invalid selection rule identity')
    return Object.freeze({ kind: rule.kind, id, selected: rule.selected })
  })
  return frozenSelectionPolicy(spec.defaultSelected, canonicalRules)
}

export function selectionKey(spec: SelectionSpec): string {
  const rules = [...spec.rules]
    .sort((left, right) => left.id.localeCompare(right.id) || left.kind.localeCompare(right.kind))
    .map(rule => `${rule.kind}:${rule.id}:${rule.selected ? '1' : '0'}`)
  return JSON.stringify([spec.shareInstance, spec.syntheticRoot, spec.defaultSelected ? 1 : 0, rules])
}
