export const D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES = Object.freeze([
  'WINDSHARE_WINDOWS_OS_NETWORK',
  'WINDSHARE_D5_E2E_LEASE_TOKEN',
  'WINDSHARE_D5_RUNNER_PIPE',
  'WINDSHARE_D5_CHILD_MANIFEST',
] as const)

/** Values are capabilities and therefore never enter receipts, argv, or logs. */
export function d5SettlementOwnershipEnvironment(
  insideWindowsD5: boolean,
  source: Readonly<Record<string, string | undefined>> = process.env,
): Readonly<Record<string, string>> {
  const byUppercaseName = new Map(
    Object.entries(source).map(([name, value]) => [name.toUpperCase(), { name, value }]),
  )
  const result: Record<string, string> = {}
  for (const canonicalName of D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES) {
    const entry = byUppercaseName.get(canonicalName)
    if (!insideWindowsD5) {
      if (entry?.value !== undefined) {
        throw new Error(`non-D5 sample inherited forbidden ownership capability ${canonicalName}`)
      }
      continue
    }
    if (entry?.value === undefined || entry.value.length === 0) {
      throw new Error(`D5 sample lacks ownership capability ${canonicalName}`)
    }
    result[canonicalName] = entry.value
  }
  return Object.freeze(result)
}
