import { loadVectorFile, type VectorCase } from '../vectors'

export interface SelectionCase {
  readonly files: string
  readonly bytes: string
  readonly terminal: boolean
  readonly failed: boolean
  readonly class: string
}

export interface CheckpointCut {
  readonly cut: string
  readonly published: boolean
}

export interface SemanticsVector extends VectorCase {
  readonly values?: Readonly<Record<string, string>>
  readonly cases?: readonly SelectionCase[] | readonly CheckpointCut[]
  readonly cuts?: readonly CheckpointCut[]
  readonly order?: readonly string[]
  readonly publishOnlyAfter?: readonly string[]
  readonly preCommitCrashVisible?: boolean
  readonly states?: readonly string[]
  readonly explicitStopUsesCrashGrace?: boolean
}

export const semantics = loadVectorFile(
  new URL('../../../core/testvectors/v2-semantics.json', import.meta.url),
).cases as SemanticsVector[]

export function classifySelection(value: SelectionCase): string {
  if (BigInt(value.files) >= 30n || BigInt(value.bytes) >= 8n << 20n) {
    return 'large'
  }
  if (!value.terminal || value.failed) {
    return 'unknown'
  }
  return 'small'
}
