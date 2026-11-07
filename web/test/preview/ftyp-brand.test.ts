import { describe, expect, it } from 'vitest'
import { unsupportedStillImageFtypBrand } from '../../src/preview/ftyp-brand'

const cc = (s: string) => [...s].map((c) => c.charCodeAt(0))

function box(size32: number, body: number[], size64?: bigint): number[] {
  const out = new Uint8Array(8 + (size64 === undefined ? 0 : 8) + body.length)
  const v = new DataView(out.buffer)
  v.setUint32(0, size32, false)
  out.set(cc('ftyp'), 4)
  let at = 8
  if (size64 !== undefined) { v.setBigUint64(8, size64, false); at = 16 }
  out.set(body, at)
  return [...out]
}
const ftyp = (major: string, minor: string, compat: string[]) => [...cc(major), ...cc(minor), ...compat.flatMap(cc)]
const free = (payload: string) => [0, 0, 0, 8 + payload.length, ...cc('free'), ...cc(payload)]
const u8 = (a: number[]) => Uint8Array.from(a)

describe('unsupportedStillImageFtypBrand', () => {
  it('classifies genuine HEIC/AVIF major brands', () => {
    expect(unsupportedStillImageFtypBrand(u8(box(24, ftyp('heic', '\0\0\0\0', ['mif1', 'heic']))))).toBe('HEIC/AVIF')
    expect(unsupportedStillImageFtypBrand(u8(box(32, ftyp('avif', '\0\0\0\0', ['avif', 'mif1', 'miaf', 'MA1B']))))).toBe('HEIC/AVIF')
  })

  it('classifies compatible-only still brands under a non-video major', () => {
    expect(unsupportedStillImageFtypBrand(u8(box(24, ftyp('MiHE', '\0\0\0\0', ['mif1', 'heic']))))).toBe('HEIC/AVIF')
  })

  it('never treats minor_version or following boxes as brands', () => {
    expect(unsupportedStillImageFtypBrand(u8(box(16, ftyp('isom', 'heic', []))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(16, ftyp('XXXX', 'heic', []))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8([...box(24, ftyp('isom', '\0\0\0\0', ['isom', 'mp41'])), ...free('mif1')]))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8([...box(16, ftyp('XXXX', '\0\0\0\0', [])), ...free('mif1')]))).toBeUndefined()
  })

  it('reads extended size=1 boxes at the 64-bit offsets and bounds the scan', () => {
    expect(unsupportedStillImageFtypBrand(u8(box(1, ftyp('heic', '\0\0\0\0', ['mif1']), 28n)))).toBe('HEIC/AVIF')
    expect(unsupportedStillImageFtypBrand(u8(box(1, ftyp('XXXX', '\0\0\0\0', ['mif1']), 28n)))).toBe('HEIC/AVIF')
    // isom + following free payload 'mif1' must stay video.
    expect(unsupportedStillImageFtypBrand(u8([...box(1, ftyp('isom', '\0\0\0\0', ['isom', 'mp41']), 32n), ...free('mif1')]))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8([...box(1, ftyp('XXXX', '\0\0\0\0', []), 24n), ...free('mif1')]))).toBeUndefined()
    // Huge declared length is clamped to the sniff window, not converted unsafely.
    expect(unsupportedStillImageFtypBrand(u8(box(1, ftyp('XXXX', '\0\0\0\0', ['mif1']), 0xffff_ffff_ffff_ffffn)))).toBe('HEIC/AVIF')
  })

  it('declines malformed or truncated headers', () => {
    expect(unsupportedStillImageFtypBrand(u8(box(1, [], undefined).slice(0, 12)))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(1, ftyp('heic', '\0\0\0\0', []), 8n)))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(1, [...cc('he')], undefined).slice(0, 16).concat(cc('he'))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(12, ftyp('heic', '\0\0\0\0', []))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(24, ftyp('heic', '\0\0\0\0', ['mif1'])).slice(0, 10)))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(24, ftyp('XXXX', '\0\0\0\0', ['mif1'])).slice(0, 19)))).toBeUndefined()
  })

  it('declines size 0 (extends to EOF) because the brand list has no bound', () => {
    expect(unsupportedStillImageFtypBrand(u8(box(0, ftyp('heic', '\0\0\0\0', []))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(0, ftyp('XXXX', '\0\0\0\0', ['mif1']))))).toBeUndefined()
  })

  it('keeps image-sequence and mixed video brands on the video path', () => {
    for (const major of ['avis', 'msf1', 'heis', 'hevs']) {
      expect(unsupportedStillImageFtypBrand(u8(box(24, ftyp(major, '\0\0\0\0', ['mif1', 'miaf']))))).toBeUndefined()
    }
    expect(unsupportedStillImageFtypBrand(u8(box(28, ftyp('XXXX', '\0\0\0\0', ['avif', 'avis', 'msf1']))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(24, ftyp('isom', '\0\0\0\0', ['mif1', 'heic']))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(32, ftyp('mif1', '\0\0\0\0', ['mif1', 'iso2', 'avc1', 'mp41']))))).toBeUndefined()
    expect(unsupportedStillImageFtypBrand(u8(box(24, ftyp('mp42', '\0\0\0\0', ['isom', 'mp42']))))).toBeUndefined()
  })

  it('respects Uint8Array byteOffset for sliced views', () => {
    const inner = box(24, ftyp('XXXX', '\0\0\0\0', ['heic', 'mif1']))
    const backing = u8([...box(24, ftyp('isom', '\0\0\0\0', ['isom', 'mp41'])), ...inner, 0, 0])
    expect(unsupportedStillImageFtypBrand(backing.subarray(24, 48))).toBe('HEIC/AVIF')
    expect(unsupportedStillImageFtypBrand(backing.subarray(0, 24))).toBeUndefined()
    const ext = u8([9, 9, 9, ...box(1, ftyp('XXXX', '\0\0\0\0', ['mif1']), 28n)])
    expect(unsupportedStillImageFtypBrand(ext.subarray(3))).toBe('HEIC/AVIF')
  })
})
