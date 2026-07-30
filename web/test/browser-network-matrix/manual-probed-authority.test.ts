import { describe, expect, it } from 'vitest'

describe('retired profile-level manual pre-probe authority', () => {
  it('exports no compatibility surface that could bypass sample-scoped credentials', async () => {
    const retiredModule = await import(
      '../../scripts/browser-network-matrix/linux-topology/manual-probed-authority.ts'
    )

    expect(Object.keys(retiredModule)).toEqual([])
  })
})
