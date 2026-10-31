import { describe, expect, it } from 'vitest'
import { isCatalogName } from '../../src/catalog/path-policy'
import { isPortableOutputName } from '../../src/output/names/policy'
import { compatibleNameCandidate } from '../../src/output/file-system-access/compatible-name/naming'
import { loadVectorFile } from '../vectors'

interface NameVector {
  readonly name: string
  readonly input: string
  readonly portableOutput?: boolean
}
const vectors = loadVectorFile(new URL('../../../core/testvectors/path-policy.json', import.meta.url))
  .cases as unknown as readonly NameVector[]

describe('destination naming is separate from catalog authority', () => {
  it.each(vectors.filter(vector => vector.portableOutput !== undefined))('$name', vector => {
    const name = vector.input.split('/').at(-1)!
    expect(isCatalogName(name)).toBe(true)
    expect(isPortableOutputName(name)).toBe(vector.portableOutput)
  })

  it.each(['NUL .txt', 'CON', 'COM¹', 'trailing-space ', 'trailing-dot.', 'question?.txt'])(
    'allocates a portable fallback without losing %j', async name => {
      const candidate = await compatibleNameCandidate({
        operationId: 'AQIDBAUGBwgJCgsMDQ4PEA',
        logicalPath: ['root', name],
        entryKind: 'file',
        primaryToken: 'abcdef',
        attempt: 0,
      })
      expect(candidate.logicalPath).toEqual(['root', name])
      expect(isPortableOutputName(candidate.physicalComponent)).toBe(true)
      expect(candidate.physicalComponent).not.toBe(name)
    },
  )
})
