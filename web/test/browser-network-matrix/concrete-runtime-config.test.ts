import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  EXTERNAL_FIXTURE_CONFIG_SCHEMA,
  loadNetworkMatrixExternalFixtureConfig,
  parseNetworkMatrixExternalFixtureConfig,
  runtimeInputsFromExternalFixtureConfig,
} from '../../scripts/browser-network-matrix/linux-topology/concrete-runtime-config.ts'

const SHA_A = 'a'.repeat(64)
const SHA_B = 'b'.repeat(64)
const SHA_C = 'c'.repeat(64)
const CONTROL_CREDENTIAL = 'control-credential-authority-0001'

const temporaryDirectories: string[] = []

afterEach(async () => {
  vi.unstubAllEnvs()
  await Promise.all(temporaryDirectories.splice(0).map((path) =>
    rm(path, { recursive: true, force: true })))
})

describe('external network-matrix fixture config', () => {
  it('loads the complete exact contract from one explicit absolute path', async () => {
    const directory = await temporaryDirectory()
    const path = join(directory, 'runtime.json')
    const raw = completeConfig(directory)
    await writeFile(path, `${JSON.stringify(raw)}\n`, 'utf8')

    const config = await loadNetworkMatrixExternalFixtureConfig(path)

    expect(config).toEqual(raw)
    expect(Object.isFrozen(config)).toBe(true)
    expect(Object.isFrozen(config.publicStun)).toBe(true)
  })

  it('keeps nullable external authorities absent even when suggestive environment values exist', async () => {
    vi.stubEnv('WINDSHARE_PUBLIC_STUN_ENDPOINT', 'stun:ambient.example.test:3478')
    vi.stubEnv('WINDSHARE_REMOTE_PION_CREDENTIAL', CONTROL_CREDENTIAL)
    const directory = await temporaryDirectory()
    const path = join(directory, 'runtime.json')
    await writeFile(path, `${JSON.stringify(emptyConfig())}\n`, 'utf8')

    const config = await loadNetworkMatrixExternalFixtureConfig(path)

    expect(config.publicStun).toBeNull()
    expect(config.restrictedUdp).toBeNull()
    expect(config.coturn).toBeNull()
    expect(config.manualRealNat).toBeNull()
    expect(runtimeInputsFromExternalFixtureConfig(config).externalFixtures)
      .not.toHaveProperty('scheduled-coturn')
  })

  it('rejects unknown keys and wrong primitive types without reflecting config values', () => {
    const secret = 'secret-that-must-not-be-reflected-0001'
    const raw = {
      ...emptyConfig(),
      unexpectedCredential: secret,
    }

    const message = capturedError(() => parseNetworkMatrixExternalFixtureConfig(raw))
    expect(message).toContain('network matrix external fixture trust config is invalid')
    expect(message).not.toContain(secret)
  })

  it('rejects non-null Coturn trust config while no revocable provider adapter is compiled', () => {
    const marker = 'must-not-be-reflected-coturn-provider'
    const raw = completeConfig(resolve('testdata'))
    const malformed = {
      ...raw,
      coturn: { control: { ...controlConfig(resolve('testdata'), 'coturn'), marker } },
    }

    const message = capturedError(() => parseNetworkMatrixExternalFixtureConfig(malformed))
    expect(message).toContain('network matrix external fixture trust config is invalid')
    expect(message).not.toContain(marker)
  })

  it('rejects a trust config that aliases TLS and attestation authorities', () => {
    const raw = completeConfig(resolve('testdata'))
    const publicStun = raw.publicStun!
    const malformed = {
      ...raw,
      publicStun: {
        control: {
          ...publicStun.control,
          attestationPublicKeyFile: publicStun.control.tlsCertificateAuthorityFile,
        },
      },
    }
    expect(() => parseNetworkMatrixExternalFixtureConfig(malformed)).toThrow()
  })

  it('rejects relative config paths before consulting the filesystem', async () => {
    await expect(loadNetworkMatrixExternalFixtureConfig('runtime.json')).rejects.toThrow(
      'network matrix external fixture trust config is invalid',
    )
  })
})

function completeConfig(authorityRoot: string) {
  return {
    schemaVersion: EXTERNAL_FIXTURE_CONFIG_SCHEMA,
    publicStun: { control: controlConfig(authorityRoot, 'public') },
    restrictedUdp: { control: controlConfig(authorityRoot, 'restricted') },
    coturn: null,
    manualRealNat: { control: controlConfig(authorityRoot, 'manual') },
  } as const
}

function emptyConfig() {
  return {
    schemaVersion: EXTERNAL_FIXTURE_CONFIG_SCHEMA,
    publicStun: null,
    restrictedUdp: null,
    coturn: null,
    manualRealNat: null,
  } as const
}

function controlConfig(authorityRoot: string, identity: string) {
  return {
    controllerOrigin: `https://${identity}.example.test/`,
    tlsCertificateSha256: certificateSha256For(identity),
    tlsCertificateAuthorityFile: resolve(authorityRoot, `${identity}-ca.pem`),
    attestationPublicKeyFile: resolve(authorityRoot, `${identity}-attestation.pem`),
  } as const
}

function certificateSha256For(identity: string): string {
  if (identity === 'public') return SHA_A
  if (identity === 'manual') return SHA_B
  return SHA_C
}

async function temporaryDirectory(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), 'windshare-concrete-config-test-'))
  temporaryDirectories.push(directory)
  return directory
}

function capturedError(operation: () => unknown): string {
  try {
    operation()
    return ''
  } catch (cause) {
    return String(cause)
  }
}
