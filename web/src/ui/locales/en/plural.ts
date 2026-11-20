export function plural(count: number | bigint, singular: string, pluralForm = `${singular}s`): string {
  return count === 1 || count === 1n ? singular : pluralForm
}
