import { resolve } from 'node:path'

import { describe, expect, it, vi } from 'vitest'

import type { BrowserSampleContainmentBackend } from '../../scripts/browser-evidence/process/containment.ts'
import { runNetworkMatrix } from '../../scripts/browser-network-matrix/runner.ts'
import {
  EXTERNAL_FIXTURE_CONFIG_SCHEMA,
  parseNetworkMatrixExternalFixtureConfig,
} from '../../scripts/browser-network-matrix/linux-topology/concrete-runtime-config.ts'
import type { ExternalFixtureControlCredentialAuthority } from '../../scripts/browser-network-matrix/linux-topology/control-credential.ts'
import { createExternalFixtureNetworkMatrixRuntimeBootstrap } from '../../scripts/browser-network-matrix/linux-topology/external-fixture-runtime-bootstrap.ts'
import { loadRegistry } from './fixtures.ts'

describe('unavailable Coturn production composition', () => {
  it('emits no samples and never reaches probe, broker, or helper authority', async () => {
    const registry = await loadRegistry()
    const acquire = vi.fn()
    const closeAndWait = vi.fn().mockResolvedValue({ terminal: 'closed' as const })
    const credentials: ExternalFixtureControlCredentialAuthority = {
      acquire,
      closeAndWait,
      forceTerminateAndWait: vi.fn().mockResolvedValue({ terminal: 'closed' as const }),
    }
    const preflight = vi.fn()
    const execute = vi.fn()
    const containment = {
      kind: 'test',
      preflight,
      execute,
    } as unknown as BrowserSampleContainmentBackend
    const topologyFiles = vi.fn()
    const bootstrap = createExternalFixtureNetworkMatrixRuntimeBootstrap({
      config: parseNetworkMatrixExternalFixtureConfig({
        schemaVersion: EXTERNAL_FIXTURE_CONFIG_SCHEMA,
        publicStun: null,
        restrictedUdp: null,
        coturn: null,
        manualRealNat: null,
      }),
      controlCredentials: credentials,
      containment,
      checkoutSha: 'a'.repeat(40),
      topologyFiles,
      nodeExecutable: process.execPath,
      repositoryRoot: resolve('.'),
      processDeadlineMs: 1_000,
      terminationGraceMs: 100,
      attemptLeaseMs: 1_000,
      resultPollIntervalMs: 10,
      resultDeadlineMs: 500,
      challengeDeadlineMs: 500,
      cleanupDeadlineMs: 100,
    })
    const runtime = await bootstrap.bootstrap({
      registry,
      invocationId: 'invocation-0123456789abcdef0123456789abcdef',
      runId: 'coturn-unavailable-composition-run',
      executionMode: 'scheduled',
    }).result

    const run = await runNetworkMatrix({
      registry,
      runId: 'coturn-unavailable-composition-run',
      executionMode: 'scheduled',
      authorities: runtime.authorities,
      samples: runtime.samples,
      trace: () => undefined,
    })

    expect(run.samples).toEqual([])
    expect(run.runOutcome).toBe('not-executed')
    expect(run.profileResults).toEqual(expect.arrayContaining([
      expect.objectContaining({
        profileId: 'scheduled-coturn',
        prerequisiteOutcome: 'unavailable',
        expectedSamples: 15,
        observedSamples: 0,
        profileOutcome: 'not-executed',
      }),
    ]))
    expect(acquire).not.toHaveBeenCalled()
    expect(preflight).not.toHaveBeenCalled()
    expect(execute).not.toHaveBeenCalled()
    expect(topologyFiles).not.toHaveBeenCalled()

    await expect(runtime.closeAndWait()).resolves.toEqual({ terminal: 'closed' })
    expect(closeAndWait).toHaveBeenCalledOnce()
  })
})
