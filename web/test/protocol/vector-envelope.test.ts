import { describe, expect, it } from 'vitest'

import { b64ToBytes, loadVectorFile } from '../vectors'

const sampleUrl = new URL('../../../core/testvectors/envelope-sample.json', import.meta.url)

describe('version-neutral golden-vector envelope', () => {
  it('parses the shared envelope and decodes its base64 byte fields', () => {
    const fixture = loadVectorFile(sampleUrl)
    expect(fixture.kind).toBe('envelope-sample')
    expect(fixture.cases).toHaveLength(2)
    const [hello, empty] = fixture.cases
    if (hello === undefined || empty === undefined) {
      throw new Error('Envelope sample must contain exactly two cases')
    }
    expect(hello.name).toBe('hello')
    expect(new TextDecoder().decode(b64ToBytes(hello.bytesB64 as string))).toBe('hello')
    expect(b64ToBytes(empty.bytesB64 as string)).toHaveLength(0)
  })
})
