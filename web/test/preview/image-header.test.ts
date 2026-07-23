import { describe, expect, it } from 'vitest'

import {
  sniffV2ImageHeader,
  V2_IMAGE_DECODED_BYTES,
  V2ImagePreviewError,
} from '../../src/preview/image-header'

function writeUint32(bytes: Uint8Array, offset: number, value: number): void {
  new DataView(bytes.buffer).setUint32(offset, value, false)
}

function png(width: number, height: number, animated = false): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(animated ? 53 : 41)
  bytes.set([137, 80, 78, 71, 13, 10, 26, 10])
  writeUint32(bytes, 8, 13)
  bytes.set([73, 72, 68, 82], 12)
  writeUint32(bytes, 16, width)
  writeUint32(bytes, 20, height)
  let offset = 33
  if (animated) {
    writeUint32(bytes, offset, 0)
    bytes.set([97, 99, 84, 76], offset + 4)
    offset += 12
  }
  // The IDAT body is intentionally absent: header sniffing must not require it.
  writeUint32(bytes, offset, 1_000_000)
  bytes.set([73, 68, 65, 84], offset + 4)
  return bytes
}

function webpAnimated(): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(30)
  bytes.set([82, 73, 70, 70])
  new DataView(bytes.buffer).setUint32(4, 22, true)
  bytes.set([87, 69, 66, 80, 86, 80, 56, 88], 8)
  new DataView(bytes.buffer).setUint32(16, 10, true)
  bytes[20] = 0x02
  return bytes
}

describe('bounded image header policy', () => {
  it('accepts a static PNG without reading its encoded payload', () => {
    expect(sniffV2ImageHeader(png(320, 200), 1_000_041n)).toEqual({
      mimeType: 'image/png',
      width: 320,
      height: 200,
    })
  })

  it('rejects animation and decoded-memory bombs before browser decode', () => {
    expect(() => sniffV2ImageHeader(png(10, 10, true), 53n))
      .toThrowError(new V2ImagePreviewError('Animated PNG preview is not supported'))
    const side = Math.floor(Math.sqrt(V2_IMAGE_DECODED_BYTES / 4)) + 1
    expect(() => sniffV2ImageHeader(png(side, side), 41n))
      .toThrow('decoded-memory limit')
    expect(() => sniffV2ImageHeader(webpAnimated(), 30n))
      .toThrow('Animated WebP')
  })

  it('rejects a WebP whose authenticated file size disagrees with RIFF', () => {
    expect(() => sniffV2ImageHeader(webpAnimated(), 31n)).toThrow('RIFF length')
  })
})
