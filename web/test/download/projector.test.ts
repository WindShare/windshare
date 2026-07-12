import { describe, expect, it } from 'vitest'

import { BlockProjector, DownloadError } from '../../src/download'
import {
  chunk,
  file,
  fixtureContext,
  range,
  validateFixturePath,
} from './fixtures'

describe('selected block projection', () => {
  it('advances across unselected siblings and snapshots selected bytes', () => {
    const context = fixtureContext(
      [file('tree/selected.bin', 3)],
      new Map([
        [
          0,
          [
            range('tree/skipped.bin', 0, 2),
            range('tree/selected.bin', 0, 3),
          ],
        ],
      ]),
    )
    const plaintext = new Uint8Array([90, 91, 1, 2, 3])

    const slices = new BlockProjector(context).project(chunk(0), plaintext)
    plaintext.fill(0)

    expect(slices).toHaveLength(1)
    expect(slices[0]).toMatchObject({ path: 'tree/selected.bin', offset: 0 })
    expect(slices[0]?.data).toEqual(new Uint8Array([1, 2, 3]))
    expect(Object.isFrozen(slices)).toBe(true)
  })

  it('rejects plan byte drift before consulting layout', () => {
    const context = fixtureContext(
      [file('file.bin', 3)],
      new Map([[0, [range('file.bin', 0, 3)]]]),
      validateFixturePath,
      2,
    )

    expect(() => new BlockProjector(context)).toThrowError(
      expect.objectContaining<Partial<DownloadError>>({ code: 'invalid-plan' }),
    )
  })

  it('rejects malformed ranges and incomplete block coverage', () => {
    const oversized = fixtureContext(
      [file('file.bin', 3)],
      new Map([[0, [range('file.bin', 2, 2)]]]),
    )
    const incomplete = fixtureContext(
      [file('file.bin', 3)],
      new Map([[0, [range('file.bin', 0, 2)]]]),
    )

    expect(() => new BlockProjector(oversized).project(chunk(0), new Uint8Array(2))).toThrowError(
      expect.objectContaining<Partial<DownloadError>>({ code: 'invalid-layout' }),
    )
    expect(() => new BlockProjector(incomplete).project(chunk(0), new Uint8Array(3))).toThrowError(
      expect.objectContaining<Partial<DownloadError>>({ code: 'invalid-layout' }),
    )
  })

  it('rejects blocks outside the immutable transfer plan', () => {
    const context = fixtureContext(
      [file('file.bin', 1)],
      new Map([[0, [range('file.bin', 0, 1)]]]),
    )

    expect(() => new BlockProjector(context).project(chunk(1), new Uint8Array([1]))).toThrowError(
      expect.objectContaining<Partial<DownloadError>>({ code: 'block-not-selected' }),
    )
  })
})
