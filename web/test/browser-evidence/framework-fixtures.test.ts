import { join } from 'node:path'
import { readFile, readdir } from 'node:fs/promises'

import { describe, expect, it } from 'vitest'

import type { BrowserSampleTrace } from '../../scripts/browser-evidence/sample-runner.ts'
import {
  guardArtifactSuite,
  type GuardArtifactSuiteOptions,
} from '../../scripts/browser-evidence/artifact/guard.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'

import {
  createFrameworkGuardAuthority,
  createFrameworkWorkspace,
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  runSyntheticSample,
  type SyntheticSampleOptions,
} from './framework-fixtures.ts'

describe('browser-contract fixture process ownership', () => {
  it('fails closed without publisher DI instead of attempting an ambient native build', async () => {
    const workspace = await createFrameworkWorkspace()
    try {
      const topology = await loadFrameworkTopology()
      const guardAuthority = await createFrameworkGuardAuthority(topology)
      const missingPublisherOptions: unknown = {
        runId: 'missing-publisher',
        runPolicy: browserRunPolicy('blocking'),
        suite: 'main',
        checkoutSha: 'a'.repeat(40),
        samples: [],
        uploadParent: workspace,
        topology: guardAuthority.topology,
        settlementTrust: guardAuthority.settlementTrust,
        explicitSecrets: [],
      }
      await expect(guardArtifactSuite(
        missingPublisherOptions as GuardArtifactSuiteOptions,
      )).rejects.toThrow(/explicit directory publisher capability/u)
      expect(await readdir(workspace)).toEqual([])
    } finally {
      await removeFrameworkWorkspace(workspace)
    }
  })

  it('cannot acquire native Windows process authority through topology or ambient options', async () => {
    const workspace = await createFrameworkWorkspace()
    try {
      const topology = await loadFrameworkTopology()
      const guardAuthority = await createFrameworkGuardAuthority(topology)
      const traces: BrowserSampleTrace[] = []
      // A helper-shaped legacy property is deliberately smuggled across the
      // structural type boundary so a future ambient opt-in fails this contract.
      const hostileOptions: SyntheticSampleOptions & { readonly windowsJobHelperPath: string } = {
        workspace,
        topology,
        suite: 'main',
        mode: 'main-pass',
        windowsJobHelperPath: join(workspace, 'hostile-windowsjob.exe'),
        trace: (event) => traces.push(event),
      }

      const outcome = await runSyntheticSample(hostileOptions)

      expect(outcome.acceptedBeforeGuard).toBe(true)
      expect(Object.keys(guardAuthority.directoryPublisher)).toEqual(['invoke'])
      await expect(readFile(outcome.resultPath, 'utf8')).resolves.toBe(JSON.stringify(outcome.result))
      expect(traces.find(({ milestone }) => milestone === 'containment-preflight-started'))
        .toMatchObject({ context: { backend: 'test' } })
      expect(traces.some(({ context }) => (
        context?.backend === 'windows-job' || context?.containmentBackend === 'windows-job'
      ))).toBe(false)
    } finally {
      await removeFrameworkWorkspace(workspace)
    }
  })
})
