import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { dirname, isAbsolute, relative, resolve } from 'node:path'

import {
  NETWORK_MATRIX_BROWSERS,
  NETWORK_MATRIX_EXECUTION_MODES,
  NETWORK_MATRIX_ID,
  NETWORK_MATRIX_IDENTITY_COUNTS,
  NETWORK_MATRIX_MANIFEST_SCHEMA,
  NETWORK_MATRIX_PROFILE_REGISTRY,
  NETWORK_MATRIX_REPORTING_SEMANTICS,
  NETWORK_MATRIX_SAMPLE_ORDINALS,
  type NetworkMatrixBrowser,
  type NetworkMatrixExecutionMode,
  type NetworkMatrixProfileId,
  type NetworkMatrixSampleOrdinal,
} from './vocabulary.ts'
import {
  parseNetworkMatrixAuthorityRequirement,
  parseNetworkTopologyProfileJson,
  type NetworkMatrixAuthorityRequirement,
  type NetworkTopologyProfile,
} from './profile.ts'
import {
  networkMatrixError,
  parseNetworkMatrixJsonText,
  requireArray,
  requireCanonicalEncoding,
  requireEnum,
  requireExactArray,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSha256,
} from './contract-support.ts'

export interface NetworkMatrixProfileReference {
  readonly profileId: NetworkMatrixProfileId
  readonly profileKind: NetworkMatrixProfileId
  readonly executionMode: NetworkMatrixExecutionMode
  readonly authorityId: NetworkMatrixAuthorityRequirement['authorityId']
  readonly authorityKind: NetworkMatrixAuthorityRequirement['authorityKind']
  readonly profilePath: string
  readonly profileSha256: string
}

export interface NetworkMatrixManifest {
  readonly schemaVersion: typeof NETWORK_MATRIX_MANIFEST_SCHEMA
  readonly matrixId: typeof NETWORK_MATRIX_ID
  readonly reportingSemantics: typeof NETWORK_MATRIX_REPORTING_SEMANTICS
  readonly browsers: typeof NETWORK_MATRIX_BROWSERS
  readonly sampleOrdinals: typeof NETWORK_MATRIX_SAMPLE_ORDINALS
  readonly authorities: readonly NetworkMatrixAuthorityRequirement[]
  readonly profiles: readonly NetworkMatrixProfileReference[]
  readonly identityCounts: typeof NETWORK_MATRIX_IDENTITY_COUNTS
}

export interface NetworkMatrixIdentity {
  readonly profileId: NetworkMatrixProfileId
  readonly browser: NetworkMatrixBrowser
  readonly sampleOrdinal: NetworkMatrixSampleOrdinal
}

export interface LoadedNetworkMatrixRegistry {
  readonly manifestPath: string
  readonly manifestSha256: string
  readonly manifest: NetworkMatrixManifest
  readonly profiles: readonly NetworkTopologyProfile[]
}

export function parseNetworkMatrixManifest(value: unknown): NetworkMatrixManifest {
  const record = requireRecord(value, 'browser network matrix manifest')
  requireExactKeys(record, [
    'schemaVersion',
    'matrixId',
    'reportingSemantics',
    'browsers',
    'sampleOrdinals',
    'authorities',
    'profiles',
    'identityCounts',
  ], 'browser network matrix manifest')
  const authorities = parseAuthorities(record.authorities)
  const profiles = parseProfileReferences(record.profiles, authorities)
  const identityCounts = parseIdentityCounts(record.identityCounts)
  return Object.freeze({
    schemaVersion: requireLiteral(
      record.schemaVersion,
      NETWORK_MATRIX_MANIFEST_SCHEMA,
      'browser network matrix manifest schema',
    ),
    matrixId: requireLiteral(record.matrixId, NETWORK_MATRIX_ID, 'browser network matrix ID'),
    reportingSemantics: requireLiteral(
      record.reportingSemantics,
      NETWORK_MATRIX_REPORTING_SEMANTICS,
      'browser network matrix reporting semantics',
    ),
    browsers: requireExactArray(
      record.browsers,
      NETWORK_MATRIX_BROWSERS,
      'browser network matrix browser registry',
    ) as typeof NETWORK_MATRIX_BROWSERS,
    sampleOrdinals: requireExactArray(
      record.sampleOrdinals,
      NETWORK_MATRIX_SAMPLE_ORDINALS,
      'browser network matrix sample ordinals',
    ) as typeof NETWORK_MATRIX_SAMPLE_ORDINALS,
    authorities,
    profiles,
    identityCounts,
  })
}

export function parseNetworkMatrixManifestJson(encoded: string): NetworkMatrixManifest {
  return requireCanonicalEncoding(
    encoded,
    parseNetworkMatrixManifest(parseNetworkMatrixJsonText(encoded, 'browser network matrix manifest')),
    'browser network matrix manifest',
  )
}

export async function loadNetworkMatrixRegistry(
  manifestPath: string,
): Promise<LoadedNetworkMatrixRegistry> {
  if (!isAbsolute(manifestPath) || resolve(manifestPath) !== manifestPath) {
    networkMatrixError('browser network matrix manifest path must be absolute and canonical')
  }
  const manifestBytes = await readFile(manifestPath)
  const manifest = parseNetworkMatrixManifestJson(manifestBytes.toString('utf8'))
  const root = dirname(manifestPath)
  const profiles: NetworkTopologyProfile[] = []
  for (const reference of manifest.profiles) {
    const profilePath = resolve(root, ...reference.profilePath.split('/'))
    if (!isContained(root, profilePath)) networkMatrixError('network profile path escapes its manifest root')
    const bytes = await readFile(profilePath)
    if (sha256(bytes) !== reference.profileSha256) {
      networkMatrixError(`network profile ${reference.profileId} differs from its manifest digest`)
    }
    const profile = parseNetworkTopologyProfileJson(bytes.toString('utf8'))
    if (
      profile.profileId !== reference.profileId ||
      profile.profileKind !== reference.profileKind ||
      profile.executionMode !== reference.executionMode ||
      profile.authority.authorityId !== reference.authorityId ||
      profile.authority.authorityKind !== reference.authorityKind ||
      profile.authority.attestationPublicKeySha256 !==
        manifest.authorities.find((authority) => authority.authorityId === reference.authorityId)
          ?.attestationPublicKeySha256
    ) {
      networkMatrixError(`network profile ${reference.profileId} contradicts its manifest authority`)
    }
    profiles.push(profile)
  }
  return Object.freeze({
    manifestPath,
    manifestSha256: sha256(manifestBytes),
    manifest,
    profiles: Object.freeze(profiles),
  })
}

export function networkMatrixIdentities(
  manifest: NetworkMatrixManifest,
  mode?: NetworkMatrixExecutionMode,
): readonly NetworkMatrixIdentity[] {
  const parsed = parseNetworkMatrixManifest(manifest)
  const validatedMode = mode === undefined
    ? undefined
    : requireEnum(mode, NETWORK_MATRIX_EXECUTION_MODES, 'network matrix execution mode')
  return Object.freeze(parsed.profiles
    .filter((profile) => validatedMode === undefined || profile.executionMode === validatedMode)
    .flatMap((profile) => parsed.browsers.flatMap((browser) =>
      parsed.sampleOrdinals.map((sampleOrdinal) => Object.freeze({
        profileId: profile.profileId,
        browser,
        sampleOrdinal,
      })))))
}

export function networkMatrixIdentityKey(identity: NetworkMatrixIdentity): string {
  return `${identity.profileId}/${identity.browser}/${identity.sampleOrdinal}`
}

export function sha256(value: Uint8Array | string): string {
  return createHash('sha256').update(value).digest('hex')
}

function parseAuthorities(value: unknown): readonly NetworkMatrixAuthorityRequirement[] {
  const items = requireArray(value, 'browser network matrix authority registry')
  if (items.length !== NETWORK_MATRIX_PROFILE_REGISTRY.length) {
    networkMatrixError('browser network matrix authority registry has the wrong size')
  }
  return Object.freeze(items.map((item, index) => {
    const expected = NETWORK_MATRIX_PROFILE_REGISTRY[index]
    if (expected === undefined) networkMatrixError('browser network matrix authority exceeds its registry')
    return parseNetworkMatrixAuthorityRequirement(item, expected)
  }))
}

function parseProfileReferences(
  value: unknown,
  authorities: readonly NetworkMatrixAuthorityRequirement[],
): readonly NetworkMatrixProfileReference[] {
  const items = requireArray(value, 'browser network matrix profile registry')
  if (items.length !== NETWORK_MATRIX_PROFILE_REGISTRY.length) {
    networkMatrixError('browser network matrix profile registry has the wrong size')
  }
  const digests = new Set<string>()
  return Object.freeze(items.map((item, index) => {
    const reference = requireRecord(item, `browser network matrix profile ${index}`)
    requireExactKeys(reference, [
      'profileId',
      'profileKind',
      'executionMode',
      'authorityId',
      'authorityKind',
      'profilePath',
      'profileSha256',
    ], `browser network matrix profile ${index}`)
    const expected = NETWORK_MATRIX_PROFILE_REGISTRY[index]
    if (expected === undefined) networkMatrixError('browser network matrix profile exceeds its registry')
    const authority = authorities[index]
    if (
      authority === undefined ||
      authority.authorityId !== expected.authorityId ||
      authority.authorityKind !== expected.authorityKind
    ) networkMatrixError(`browser network matrix profile ${index} has no matching authority`)
    const profileSha256 = requireSha256(
      reference.profileSha256,
      `browser network matrix profile ${index} digest`,
    )
    if (digests.has(profileSha256)) networkMatrixError('browser network matrix profile digests repeat')
    digests.add(profileSha256)
    return Object.freeze({
      profileId: requireLiteral(reference.profileId, expected.profileId, `profile ${index} ID`),
      profileKind: requireLiteral(reference.profileKind, expected.profileKind, `profile ${index} kind`),
      executionMode: requireLiteral(
        reference.executionMode,
        expected.executionMode,
        `profile ${index} execution mode`,
      ),
      authorityId: requireLiteral(
        reference.authorityId,
        expected.authorityId,
        `profile ${index} authority ID`,
      ),
      authorityKind: requireLiteral(
        reference.authorityKind,
        expected.authorityKind,
        `profile ${index} authority kind`,
      ),
      profilePath: requireLiteral(
        reference.profilePath,
        expected.profilePath,
        `profile ${index} relative path`,
      ),
      profileSha256,
    })
  }))
}

function parseIdentityCounts(value: unknown): typeof NETWORK_MATRIX_IDENTITY_COUNTS {
  const counts = requireRecord(value, 'browser network matrix identity counts')
  requireExactKeys(counts, ['total', 'scheduled', 'manual'], 'browser network matrix identity counts')
  return Object.freeze({
    total: requireLiteral(counts.total, NETWORK_MATRIX_IDENTITY_COUNTS.total, 'total identity count'),
    scheduled: requireLiteral(
      counts.scheduled,
      NETWORK_MATRIX_IDENTITY_COUNTS.scheduled,
      'scheduled identity count',
    ),
    manual: requireLiteral(counts.manual, NETWORK_MATRIX_IDENTITY_COUNTS.manual, 'manual identity count'),
  })
}

function isContained(root: string, target: string): boolean {
  const path = relative(root, target)
  return path !== '' && !path.startsWith('..') && !isAbsolute(path)
}
