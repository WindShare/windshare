export interface R8TrendValidationResult {
  readonly browsers: number
  readonly records: number
  readonly sampleCount: number
}

export function validateR8TrendRecords(
  records: readonly unknown[],
  sampleCount?: number,
): R8TrendValidationResult
