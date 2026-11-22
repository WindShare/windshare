import { describe, expect, it } from 'vitest'
import { isCatalogName } from '../../src/catalog/path-policy'
import { isPortableOutputName } from '../../src/output/names/policy'
import { deterministicSafeComponent } from '../../src/output/file-system-access/safe-name'
import { loadVectorFile } from '../vectors'

interface NameVector {
  readonly name: string
  readonly input: string
  readonly canonical: string
  readonly portableOutput?: boolean
}
const vectors = loadVectorFile(new URL('../../../core/testvectors/path-policy.json', import.meta.url))
  .cases as unknown as readonly NameVector[]

describe('destination naming is separate from catalog authority', () => {
  it.each(vectors.filter(vector => vector.portableOutput !== undefined))('$name', vector => {
    const name = vector.canonical.split('/').at(-1)!
    expect(isCatalogName(name)).toBe(true)
    expect(isPortableOutputName(name)).toBe(vector.portableOutput)
  })

  it.each(['NUL .txt', 'CON', 'COM¹', 'trailing-space ', 'trailing-dot.', 'question?.txt', 'C:notes', '界'.repeat(100) + '.txt'])(
    'allocates a portable fallback without losing %j', async name => {
      const physicalComponent = await deterministicSafeComponent({
        logicalPath: ['root', name],
        entryKind: 'file',
      })
      expect(isPortableOutputName(physicalComponent)).toBe(true)
      expect(physicalComponent).not.toBe(name)
    },
  )
})
