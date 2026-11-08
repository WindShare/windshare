// Classifies a sniffed ISO BMFF ftyp box as a still HEIF/AVIF image only when
// that is unambiguous; otherwise returns undefined. FilePreview consults it only
// after the MP4 demuxer found no movie box, to word that failure as an
// unsupported image; it never decides whether a file is played. No video-brand
// list is kept: unknown brands decline.
// Brand sources: ISO/IEC 23008-12 (HEIF: mif1, mif2, heic, heix, heim, msf1,
// heis, hevs, and tmap for tone-map derived image items), ISO/IEC 23000-22
// (MIAF: miaf, MiHE/MiHA/MiHB/MiPr/MiAC/MiCm profile brands) and the AV1 Image
// File Format spec (avif, avis, MA1A, MA1B). iOS 18+ still HEIC photos write
// tmap as a compatible brand for adaptive HDR; it is not a sequence or video brand.
//
// Reject only when ALL of these hold:
// - the major brand is a still-image brand (ISO/IEC 23008-12 HEIF, MIAF, AVIF);
// - the whole ftyp box (size32 >= 16, or size=1 with largesize >= 24) lies inside
//   the sniffed bytes and its brand list is a whole number of 4-byte entries;
// - every compatible brand is a known still-image/profile brand.
// Image sequence brands (msf1, heis, hevs, avis), any video brand, any unknown
// brand, size 0 ("extends to EOF"), and malformed/truncated boxes all decline.
// JPEG XL is not handled: real .jxl starts with the 'JXL ' signature box or a raw
// FF0A codestream, never ftyp.
const STILL_MAJOR_BRANDS: ReadonlySet<string> = new Set(['avif', 'heic', 'heix', 'heim', 'mif1', 'miaf'])
const STILL_COMPATIBLE_BRANDS: ReadonlySet<string> = new Set([
  ...STILL_MAJOR_BRANDS, 'mif2', 'MA1A', 'MA1B', 'MiHE', 'MiHA', 'MiHB', 'MiPr', 'MiAC', 'MiCm', 'tmap',
])

function fourcc(bytes: Uint8Array, offset: number): string {
  return String.fromCharCode(bytes[offset]!, bytes[offset + 1]!, bytes[offset + 2]!, bytes[offset + 3]!)
}

function completeFtypBrandWindow(bytes: Uint8Array): { majorAt: number, end: number } | undefined {
  if (bytes.byteLength < 16 || fourcc(bytes, 4) !== 'ftyp') return undefined
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  const size32 = view.getUint32(0, false)
  if (size32 === 1) {
    const size64 = view.getBigUint64(8, false)
    if (size64 < 24n || size64 > BigInt(bytes.byteLength) || (size64 - 24n) % 4n !== 0n) return undefined
    return { majorAt: 16, end: Number(size64) }
  }
  if (size32 < 16 || size32 > bytes.byteLength || (size32 - 16) % 4 !== 0) return undefined
  return { majorAt: 8, end: size32 }
}

export function unsupportedStillImageFtypBrand(bytes: Uint8Array): string | undefined {
  const window = completeFtypBrandWindow(bytes)
  if (window === undefined || !STILL_MAJOR_BRANDS.has(fourcc(bytes, window.majorAt))) return undefined
  // minor_version (majorAt + 4) is never read as a brand.
  for (let offset = window.majorAt + 8; offset < window.end; offset += 4) {
    if (!STILL_COMPATIBLE_BRANDS.has(fourcc(bytes, offset))) return undefined
  }
  return 'HEIC/AVIF'
}
