import { describe, expect, it } from 'vitest'

describe('unit runner boundary', () => {
  it('keeps browser application tests out of the Node unit environment', () => {
    expect(typeof document).toBe('undefined')
  })
})
