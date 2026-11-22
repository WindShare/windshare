export const MAX_SAFE_RENAMED_SAMPLE = 8

export interface SafeRenamedEntry {
  readonly logicalPath: readonly string[]
  readonly physicalName: string
}

export interface SafeRenamedSummary {
  readonly count: number
  readonly sample: readonly SafeRenamedEntry[]
}

export function safeRenamedSummary(input: SafeRenamedSummary): SafeRenamedSummary {
  if (!Number.isSafeInteger(input.count) || input.count < 0) {
    throw new TypeError('Safe-renamed count must be a non-negative integer')
  }
  if (input.sample.length > MAX_SAFE_RENAMED_SAMPLE || input.sample.length > input.count) {
    throw new TypeError('Safe-renamed sample exceeds count or sample limit')
  }
  const sample = input.sample.map(entry => {
    if (entry.logicalPath.length === 0 || entry.logicalPath.some(segment => segment.length === 0)) {
      throw new TypeError('Safe-renamed entry requires a non-empty logical path')
    }
    if (entry.physicalName.length === 0) {
      throw new TypeError('Safe-renamed entry requires a non-empty physical name')
    }
    return Object.freeze({
      logicalPath: Object.freeze([...entry.logicalPath]),
      physicalName: entry.physicalName,
    })
  })
  return Object.freeze({
    count: input.count,
    sample: Object.freeze(sample),
  })
}
