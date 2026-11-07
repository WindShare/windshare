// ISO BMFF still-image brands (ISO/IEC 23008-12 HEIF, MIAF, AVIF). Image
// *sequence* brands (msf1, heis, hevs, avis) are deliberately absent: they carry
// a track and stay on the existing MP4 path so nothing that played before is
// rejected. JPEG XL is absent too: a real .jxl starts with the 'JXL ' signature
// box or a raw FF0A codestream, never with ftyp.
const STILL_IMAGE_FTYP_BRANDS: ReadonlySet<string> = new Set(['avif', 'heic', 'heix', 'heim', 'mif1', 'miaf'])
// Any of these as major or compatible brand means the file may be playable
// video, so it is never rejected.
const VIDEO_OR_SEQUENCE_FTYP_BRANDS: ReadonlySet<string> = new Set([
  'isom', 'iso2', 'iso3', 'iso4', 'iso5', 'iso6', 'iso8', 'iso9', 'mp41', 'mp42', 'mp71', 'avc1', 'hvc1', 'hev1',
  'av01', 'dash', 'cmfc', 'M4V ', 'M4A ', 'qt  ', '3gp4', '3gp5', '3gp6', '3g2a',
  'msf1', 'heis', 'hevs', 'avis',
])

function fourcc(bytes: Uint8Array, offset: number): string {
  return String.fromCharCode(...bytes.subarray(offset, offset + 4))
}

// Returns the bounded ftyp brand window [majorAt, end) or undefined when the
// header is malformed, truncated before the brand list, or size 0 ("extends to
// EOF": no end inside the sniff window, so the brand list cannot be told apart
// from later data and we keep the legacy path rather than guess).
function ftypBrandWindow(bytes: Uint8Array): { majorAt: number, end: number } | undefined {
  if (bytes.byteLength < 12 || fourcc(bytes, 4) !== 'ftyp') return undefined
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  const size32 = view.getUint32(0, false)
  if (size32 === 0) return undefined
  if (size32 !== 1) {
    return size32 < 16 ? undefined : { majorAt: 8, end: Math.min(size32, bytes.byteLength) }
  }
  if (bytes.byteLength < 16) return undefined
  const size64 = view.getBigUint64(8, false)
  if (size64 < 24n) return undefined
  return { majorAt: 16, end: size64 > BigInt(bytes.byteLength) ? bytes.byteLength : Number(size64) }
}

/**
 * Returns a label when the sniffed ftyp box unambiguously names a still image.
 * Declines (undefined -> legacy MP4 path) for malformed or ambiguous headers and
 * whenever a video or image-sequence brand appears anywhere in the bounded
 * brand list, even next to a still major such as mif1: it may hold a track.
 * minor_version (majorAt + 4) is never read as a brand.
 */
export function unsupportedStillImageFtypBrand(bytes: Uint8Array): string | undefined {
  const window = ftypBrandWindow(bytes)
  if (window === undefined || window.end < window.majorAt + 8) return undefined
  const brands = [fourcc(bytes, window.majorAt)]
  for (let offset = window.majorAt + 8; offset + 4 <= window.end; offset += 4) brands.push(fourcc(bytes, offset))
  if (brands.some((brand) => VIDEO_OR_SEQUENCE_FTYP_BRANDS.has(brand))) return undefined
  return brands.some((brand) => STILL_IMAGE_FTYP_BRANDS.has(brand)) ? 'HEIC/AVIF' : undefined
}
