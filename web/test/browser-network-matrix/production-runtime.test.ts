import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { basename, join, sep } from 'node:path'

import { afterEach, describe, expect, it } from 'vitest'

import {
  loadProductionNetworkMatrixCliRuntime,
  PRODUCTION_NETWORK_MATRIX_RUNTIME_SCHEMA,
} from '../../scripts/browser-network-matrix/linux-topology/production-runtime.ts'

const CHECKOUT_SHA = 'a'.repeat(40)
const temporaryRoots: string[] = []

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => rm(root, { force: true, recursive: true })))
})

describe('production network matrix checkout authority', () => {
  it('accepts only a runtime config bound to the separately supplied current checkout', async () => {
    const fixture = await runtimeConfigFixture()

    await expect(loadProductionNetworkMatrixCliRuntime(fixture.path, {
      checkoutSha: CHECKOUT_SHA,
      repositoryRoot: fixture.root,
    }, 'linux')).resolves.toMatchObject({ platform: 'linux' })

    await expect(loadProductionNetworkMatrixCliRuntime(fixture.path, {
      checkoutSha: 'b'.repeat(40),
      repositoryRoot: fixture.root,
    }, 'linux')).rejects.toThrow(/runtime config is invalid/u)

    await expect(loadProductionNetworkMatrixCliRuntime(fixture.path, {
      checkoutSha: CHECKOUT_SHA,
      repositoryRoot: join(fixture.root, 'stale-checkout'),
    }, 'linux')).rejects.toThrow(/runtime config is invalid/u)
  })

  it('rejects non-canonical current operands and legacy-width config identities', async () => {
    const fixture = await runtimeConfigFixture()
    const legacyFixture = await runtimeConfigFixture({ checkoutSha: 'c'.repeat(64) })

    await expect(loadProductionNetworkMatrixCliRuntime(fixture.path, {
      checkoutSha: CHECKOUT_SHA.toUpperCase(),
      repositoryRoot: fixture.root,
    }, 'linux')).rejects.toThrow(/runtime config is invalid/u)

    await expect(loadProductionNetworkMatrixCliRuntime(fixture.path, {
      checkoutSha: CHECKOUT_SHA,
      repositoryRoot: `${fixture.root}${sep}..${sep}${basename(fixture.root)}`,
    }, 'linux')).rejects.toThrow(/runtime config is invalid/u)

    await expect(loadProductionNetworkMatrixCliRuntime(legacyFixture.path, {
      checkoutSha: CHECKOUT_SHA,
      repositoryRoot: legacyFixture.root,
    }, 'linux')).rejects.toThrow(/runtime config is invalid/u)
  })
})

async function runtimeConfigFixture(
  options: Readonly<{ checkoutSha?: string }> = {},
): Promise<Readonly<{ path: string, root: string }>> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-production-runtime-'))
  temporaryRoots.push(root)
  const path = join(root, 'runtime-config.json')
  await writeFile(path, `${JSON.stringify({
    schemaVersion: PRODUCTION_NETWORK_MATRIX_RUNTIME_SCHEMA,
    externalFixtureTrustConfigFile: join(root, 'external-fixtures.json'),
    credentialBrokerHelperFile: join(root, 'credential-broker'),
    credentialBrokerWorkloadIdentity: {
      kind: 'github-actions-oidc',
      audience: 'windshare-browser-network',
      issuer: 'https://token.actions.githubusercontent.com',
      repository: 'owner/repository',
      ref: 'refs/heads/main',
      workflowRef: 'owner/repository/.github/workflows/current-commit.yml@refs/heads/main',
      requestOrigin: 'https://actions.example.test',
      requestPath: '/oidc/token',
      requestQuery: '?api-version=2.0',
    },
    repositoryRoot: root,
    nodeExecutable: process.execPath,
    checkoutSha: options.checkoutSha ?? CHECKOUT_SHA,
    topologyFiles: {
      'scheduled-public-stun': null,
      'scheduled-restricted-udp': null,
      'scheduled-coturn': null,
    },
  })}\n`, 'utf8')
  return Object.freeze({ path, root })
}
