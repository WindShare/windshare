import { encodeBase64Url } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import type {
  DirectoryAdmission,
  DirectoryAdmissionScope,
  OutputFile,
  OutputModifiedTime,
  OutputSession,
} from '../../src/transfer/output-session'

const UTF8_ENCODER = new TextEncoder()
const CATALOG_IDENTITY_BYTES = 16
const TRANSFER_INTENT_DIGEST_BYTES = 32
const identityCache = new Map<string, Promise<string>>()
const outputIdentityCache = new Map<string, string>()
export const TEST_OUTPUT_SIGNAL = new AbortController().signal
export const TEST_DIRECTORY_ADMISSION_SCOPE: DirectoryAdmissionScope = Object.freeze({
  transferIntentDigest: testOpaqueOutputIdentity('transfer-intent'),
  syntheticRoot: testOutputIdentity('synthetic-root'),
})

export interface OutputDirectoryFixture {
  readonly path: readonly string[]
  readonly modifiedTime?: OutputModifiedTime
}

export async function admitOutputDirectory(
  session: OutputSession,
  directory: OutputDirectoryFixture,
): Promise<DirectoryAdmission> {
  return (await admitOutputDirectoryBinding(session, directory)).admission
}

export async function admittedOutputDirectory(
  session: OutputSession,
  directory: OutputDirectoryFixture,
): Promise<DirectoryAdmission> {
  return (await admitOutputDirectoryBinding(session, directory)).admission
}

async function admitOutputDirectoryBinding(
  session: OutputSession,
  directory: OutputDirectoryFixture,
): Promise<{ readonly admission: DirectoryAdmission }> {
  let parent: DirectoryAdmission | undefined
  for (let depth = 0; depth <= directory.path.length; depth += 1) {
    const path = Object.freeze(directory.path.slice(0, depth))
    const binding = path.join('/')
    const terminal = depth === directory.path.length
    const request = {
      directoryId: depth === 0
        ? TEST_DIRECTORY_ADMISSION_SCOPE.syntheticRoot
        : await testCatalogIdentity(`directory:${binding}`),
      generation: await testCatalogIdentity(`generation:${binding}`),
      path,
      ...(parent === undefined ? {} : { parentAdmission: parent }),
      ...(terminal && directory.modifiedTime !== undefined
        ? { modifiedTime: directory.modifiedTime }
        : {}),
    }
    parent = await session.admitDirectory(request, TEST_OUTPUT_SIGNAL)
  }
  if (parent === undefined) throw new Error('Test output root admission was not created')
  return Object.freeze({ admission: parent })
}

export function testOutputModifiedTime(milliseconds: bigint): OutputModifiedTime {
  return Object.freeze({
    seconds: milliseconds / 1_000n,
    nanoseconds: Number(milliseconds % 1_000n) * 1_000_000,
    precision: 2,
    milliseconds,
  })
}

/** Stable opaque identities keep output-boundary fixtures faithful without async setup. */
export function testOutputIdentity(label: string): string {
  const cached = outputIdentityCache.get(label)
  if (cached !== undefined) return cached
  const encoded = UTF8_ENCODER.encode(`windshare/test-output-identity/${label}`)
  const identity = new Uint8Array(CATALOG_IDENTITY_BYTES)
  identity[0] = 0xa5
  for (let index = 0; index < encoded.byteLength; index += 1) {
    const slot = index % identity.byteLength
    identity[slot] = ((identity[slot] ?? 0) + (encoded[index] ?? 0) + index) & 0xff
  }
  const value = encodeBase64Url(identity)
  outputIdentityCache.set(label, value)
  return value
}

function testOpaqueOutputIdentity(label: string): string {
  const encoded = UTF8_ENCODER.encode(`windshare/test-output-identity/${label}`)
  const identity = new Uint8Array(TRANSFER_INTENT_DIGEST_BYTES)
  identity[0] = 0x5a
  for (let index = 0; index < encoded.byteLength; index += 1) {
    const slot = index % identity.byteLength
    identity[slot] = ((identity[slot] ?? 0) + (encoded[index] ?? 0) + index) & 0xff
  }
  return encodeBase64Url(identity)
}

export async function admittedOutputFile(
  session: OutputSession,
  file: OutputFile,
): Promise<OutputFile> {
  const parentAdmission = await admitOutputDirectory(session, {
    path: file.path.slice(0, -1),
  })
  return Object.freeze({ ...file, parentAdmission })
}

function testCatalogIdentity(label: string): Promise<string> {
  const cached = identityCache.get(label)
  if (cached !== undefined) return cached
  const pending = sha256(UTF8_ENCODER.encode(`windshare/test-output-admission/${label}`))
    .then((digest) => encodeBase64Url(digest.slice(0, CATALOG_IDENTITY_BYTES)))
  identityCache.set(label, pending)
  return pending
}
