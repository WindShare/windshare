import { join } from 'node:path'
import { readFile, readdir } from 'node:fs/promises'

import { describe, expect, it } from 'vitest'

import type { BrowserSampleContainmentBackend } from '../../scripts/browser-evidence/process/containment.ts'
import {
  ArtifactGuardRecordedError,
  guardArtifactSuite,
  type GuardArtifactSuiteOptions,
} from '../../scripts/browser-evidence/artifact/guard.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'

import {
  createFrameworkGuardAuthority,
  createFrameworkWorkspace,
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  startSyntheticSample,
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
      )).rejects.toBeInstanceOf(ArtifactGuardRecordedError)
      expect(await readdir(workspace)).toEqual([])
    } finally {
      await removeFrameworkWorkspace(workspace)
    }
  })

  it('cannot acquire native process authority through topology or ambient path options', async () => {
    const workspace = await createFrameworkWorkspace()
    try {
      const topology = await loadFrameworkTopology()
      const guardAuthority = await createFrameworkGuardAuthority(topology)
      // A path-shaped property is deliberately smuggled across the structural
      // type boundary so ownership can never degrade from an authenticated artifact.
      const hostileOptions: SyntheticSampleOptions & { readonly processOwnerPath: string } = {
        workspace,
        topology,
        suite: 'main',
        mode: 'main-pass',
        processOwnerPath: join(workspace, 'hostile-testprocessowner'),
      }

      const execution = startSyntheticSample(hostileOptions)
      const outcome = await execution.result
      const traces = execution.traces.snapshot().events

      expect(outcome.acceptedBeforeGuard).toBe(true)
      expect(Object.keys(guardAuthority.directoryPublisher)).toEqual(['invoke'])
      await expect(readFile(outcome.resultPath, 'utf8')).resolves.toBe(JSON.stringify(outcome.result))
      expect(traces.find(({ milestone }) => milestone === 'containment-preflight-started'))
        .toMatchObject({ context: { backend: 'test' } })
      expect(traces.filter(({ milestone }) => milestone === 'operation-started')).toHaveLength(1)
      expect(traces.filter(({ milestone }) => milestone === 'operation-terminal')).toHaveLength(1)
      expect(traces.find(({ milestone }) => milestone === 'operation-terminal')).toMatchObject({
        schemaVersion: 'windshare.browser-sample-trace/v1',
        component: 'browser-evidence-runner',
        runId: 'framework-run',
        operationId: 'main-chromium-sample-1',
        scenario: 'synthetic-browser-evidence',
        outcome: 'succeeded',
        context: {
          cleanupOutcome: 'completed',
          lastMilestone: 'final-result-written',
        },
      })
      expect(traces.some(({ context }) => (
        context?.backend === 'test-process-owner' ||
        context?.containmentBackend === 'test-process-owner'
      ))).toBe(false)
    } finally {
      await removeFrameworkWorkspace(workspace)
    }
  })

  it('terminalizes a preflight failure before any child can launch', async () => {
    const workspace = await createFrameworkWorkspace()
    let launched = false
    const containment: BrowserSampleContainmentBackend = Object.freeze({
      kind: 'test',
      preflight: async () => { throw new Error('synthetic preflight rejection') },
      execute: async () => {
        launched = true
        throw new Error('unreachable child launch')
      },
    })
    try {
      const topology = await loadFrameworkTopology()
      const execution = startSyntheticSample({
        workspace,
        topology,
        suite: 'main',
        mode: 'main-pass',
        containmentBackend: containment,
      })
      await expect(execution.result).rejects.toThrow(/synthetic preflight rejection/u)
      const traces = execution.traces.snapshot().events

      expect(launched).toBe(false)
      expect(traces.filter(({ milestone }) => milestone === 'operation-started')).toHaveLength(1)
      expect(traces.filter(({ milestone }) => milestone === 'operation-terminal')).toHaveLength(1)
      expect(traces.find(({ milestone }) => milestone === 'operation-terminal')).toMatchObject({
        outcome: 'failed',
        context: {
          cleanupOutcome: 'not-required',
          lastMilestone: 'containment-preflight-started',
        },
      })
    } finally {
      await removeFrameworkWorkspace(workspace)
    }
  })

  it('settles while a pull trace consumer stops after the first event', async () => {
    const workspace = await createFrameworkWorkspace()
    try {
      const topology = await loadFrameworkTopology()
      const execution = startSyntheticSample({
        workspace,
        topology,
        suite: 'main',
        mode: 'main-pass',
      })
      const consumer = execution.traces[Symbol.asyncIterator]()
      await expect(consumer.next()).resolves.toMatchObject({
        done: false,
        value: { milestone: 'operation-started' },
      })
      const outcome = await execution.result
      const snapshot = execution.traces.snapshot()
      const traces = snapshot.events

      expect(outcome.acceptedBeforeGuard).toBe(true)
      expect(snapshot).toMatchObject({ completed: true, truncated: false })
      expect(traces.filter(({ milestone }) => milestone === 'operation-started')).toHaveLength(1)
      expect(traces.filter(({ milestone }) => milestone === 'operation-terminal')).toHaveLength(1)
      expect(traces.find(({ milestone }) => milestone === 'operation-terminal')).toMatchObject({
        outcome: 'succeeded',
        context: { cleanupOutcome: 'completed', lastMilestone: 'final-result-written' },
      })
    } finally {
      await removeFrameworkWorkspace(workspace)
    }
  })
})
