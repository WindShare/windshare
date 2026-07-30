import { mkdtemp, rename, rm, symlink, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { afterEach, describe, expect, it } from 'vitest'

import { HELPER_BUILD_MANIFEST_SCHEMA_VERSION } from '../../scripts/browser-network-matrix/cli/build-helpers.mjs'
import { openHelperBuildManifestAuthority } from '../../scripts/browser-network-matrix/cli/helper-build-manifest.ts'

const cleanupRoots: string[] = []

afterEach(async () => {
  await Promise.all(cleanupRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
})

describe('held helper build manifest authority', () => {
  it('accepts only the exact platform roles from canonical held bytes', async () => {
    const fixture = await manifestFixture()
    const authority = await openHelperBuildManifestAuthority(fixture.path, 'win32')
    try {
      expect(authority.manifest.helpers.map(({ role }) => role)).toEqual([
        'artifact-publisher',
        'windows-job',
      ])
      await expect(authority.assertUnchanged()).resolves.toBeUndefined()
    } finally {
      await authority.close()
    }
  })

  it.each([
    ['duplicate root key', (value: TestManifest) => Buffer.from(
      canonical(value).replace(
        `{"schemaVersion":"${HELPER_BUILD_MANIFEST_SCHEMA_VERSION}",`,
        `{"schemaVersion":"${HELPER_BUILD_MANIFEST_SCHEMA_VERSION}","schemaVersion":"${HELPER_BUILD_MANIFEST_SCHEMA_VERSION}",`,
      ),
    )],
    ['unknown root key', (value: TestManifest) => Buffer.from(canonical({ ...value, unknown: true }))],
    ['missing architecture', (value: TestManifest) => Buffer.from(canonical({
      schemaVersion: value.schemaVersion,
      platform: value.platform,
      helpers: value.helpers,
    }))],
    ['wrong root key order', (value: TestManifest) => Buffer.from(canonical({
      platform: value.platform,
      schemaVersion: value.schemaVersion,
      architecture: value.architecture,
      helpers: value.helpers,
    }))],
    ['pretty whitespace', (value: TestManifest) => Buffer.from(`${JSON.stringify(value, null, 2)}\n`)],
    ['trailing bytes', (value: TestManifest) => Buffer.from(`${canonical(value)}trailing`)],
    ['invalid UTF-8', () => Buffer.from([0xff])],
    ['wrong schema', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      schemaVersion: 'windshare.browser-network-matrix.helper-build/v0',
    }))],
    ['wrong architecture', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      architecture: value.architecture === 'amd64' ? 'arm64' : 'amd64',
    }))],
    ['relative helper path', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      helpers: [{ ...value.helpers[0], path: 'browsermatrixpublish.exe' }, value.helpers[1]],
    }))],
    ['malformed helper SHA', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      helpers: [{ ...value.helpers[0], sha256: 'A'.repeat(64) }, value.helpers[1]],
    }))],
    ['swapped platform roles', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      helpers: [value.helpers[1], value.helpers[0]],
    }))],
    ['missing platform role', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      helpers: [value.helpers[0]],
    }))],
    ['extra platform role', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      helpers: [...value.helpers, value.helpers[1]],
    }))],
    ['cross-platform manifest', (value: TestManifest) => Buffer.from(canonical({
      ...value,
      platform: 'linux',
      helpers: [value.helpers[0]],
    }))],
  ] as const)('rejects %s', async (_case, encode) => {
    const fixture = await manifestFixture()
    await writeFile(fixture.path, encode(fixture.value))
    await expect(openHelperBuildManifestAuthority(fixture.path, 'win32')).rejects.toThrow()
  })

  it('rejects an oversized manifest before allocating its declared content', async () => {
    const fixture = await manifestFixture()
    await writeFile(fixture.path, Buffer.alloc(16 * 1024 + 1, 0x20))
    await expect(openHelperBuildManifestAuthority(fixture.path, 'win32')).rejects.toThrow(
      /bounded regular file/u,
    )
  })

  it('requires the manifest invocation path itself to be explicit and canonical', async () => {
    await expect(openHelperBuildManifestAuthority(
      'helper-manifest.json',
      'win32',
    )).rejects.toThrow(/path must be explicit, absolute, and canonical/u)
  })

  it.runIf(process.platform !== 'win32')('rejects a symbolic-link manifest path', async () => {
    const fixture = await manifestFixture()
    const alias = join(fixture.root, 'manifest-alias.json')
    await symlink(fixture.path, alias, 'file')
    await expect(openHelperBuildManifestAuthority(alias, 'win32')).rejects.toThrow(
      /bounded regular file/u,
    )
  })

  it('rejects in-place byte mutation through the held authority', async () => {
    const fixture = await manifestFixture()
    const authority = await openHelperBuildManifestAuthority(fixture.path, 'win32')
    await writeFile(fixture.path, `${fixture.encoded} `, 'utf8')
    try {
      await expect(authority.assertUnchanged()).rejects.toThrow(
        /identity or revision changed|canonical bytes or SHA-256 changed/u,
      )
    } finally {
      await authority.close()
    }
  })

  it('rejects a named-path swap even when the replacement has identical bytes', async () => {
    const fixture = await manifestFixture()
    const authority = await openHelperBuildManifestAuthority(fixture.path, 'win32')
    await rename(fixture.path, `${fixture.path}.held`)
    await writeFile(fixture.path, fixture.encoded, 'utf8')
    try {
      await expect(authority.assertUnchanged()).rejects.toThrow(/identity or revision changed/u)
    } finally {
      await authority.close()
    }
  })
})

interface TestManifestEntry {
  readonly role: 'artifact-publisher' | 'windows-job'
  readonly path: string
  readonly sha256: string
}

interface TestManifest {
  readonly schemaVersion: string
  readonly platform: 'win32' | 'linux'
  readonly architecture: 'amd64' | 'arm64'
  readonly helpers: readonly [TestManifestEntry, TestManifestEntry]
}

async function manifestFixture(): Promise<Readonly<{
  root: string
  path: string
  value: TestManifest
  encoded: string
}>> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-helper-manifest-test-'))
  cleanupRoots.push(root)
  const path = join(root, 'helper-manifest.json')
  const helpers: readonly [TestManifestEntry, TestManifestEntry] = Object.freeze([
    Object.freeze({
      role: 'artifact-publisher' as const,
      path: join(root, 'browsermatrixpublish.exe'),
      sha256: '1'.repeat(64),
    }),
    Object.freeze({
      role: 'windows-job' as const,
      path: join(root, 'windowsjob.exe'),
      sha256: '2'.repeat(64),
    }),
  ])
  const value: TestManifest = Object.freeze({
    schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
    platform: 'win32',
    architecture: runtimeGoArchitecture(),
    helpers,
  })
  const encoded = canonical(value)
  await writeFile(path, encoded, 'utf8')
  return Object.freeze({ root, path, value, encoded })
}

function canonical(value: unknown): string {
  return `${JSON.stringify(value)}\n`
}

function runtimeGoArchitecture(): 'amd64' | 'arm64' {
  return process.arch === 'arm64' ? 'arm64' : 'amd64'
}
