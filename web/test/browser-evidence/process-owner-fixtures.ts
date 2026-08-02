import { afterAll } from 'vitest'

import { createTestProcessOwnerContainmentBackend } from '../../scripts/browser-evidence/process/test-process-owner-backend.ts'
import type { TestProcessOwnerArtifact } from '../../scripts/browser-evidence/process/test-process-owner-client.mjs'
import type { BrowserSampleRunExecution } from '../../scripts/browser-evidence/sample-runner.ts'
import {
  buildTestProcessOwnerFixture,
  type TestProcessOwnerFixture,
} from '../../scripts/browser-evidence/process/process-owner-fixture.ts'
import {
  runSyntheticSample,
  startSyntheticSample,
  type SyntheticSampleOptions,
} from './framework-fixtures.ts'

let ownerFixture: TestProcessOwnerFixture | undefined
let ownerPromise: Promise<TestProcessOwnerArtifact> | undefined

afterAll(async () => {
  await ownerFixture?.dispose()
})

// Native ownership remains isolated from contract tests so only the explicit
// process gate can create or terminate host process trees.
export async function runNativeOwnedSyntheticSample(
  options: Omit<SyntheticSampleOptions, 'containmentBackend'>,
): Promise<Awaited<ReturnType<typeof runSyntheticSample>>> {
  const owner = await loadFrameworkProcessOwner()
  return runSyntheticSample({
    ...options,
    containmentBackend: createTestProcessOwnerContainmentBackend(owner),
  })
}

export async function startNativeOwnedSyntheticSample(
  options: Omit<SyntheticSampleOptions, 'containmentBackend'>,
): Promise<BrowserSampleRunExecution> {
  const owner = await loadFrameworkProcessOwner()
  return startSyntheticSample({
    ...options,
    containmentBackend: createTestProcessOwnerContainmentBackend(owner),
  })
}

export async function loadFrameworkProcessOwner(): Promise<TestProcessOwnerArtifact> {
  if (process.platform !== 'win32' && process.platform !== 'linux') {
    throw new Error(`the native process owner fixture is unsupported on ${process.platform}`)
  }
  ownerPromise ??= buildFrameworkProcessOwner()
  return ownerPromise
}

async function buildFrameworkProcessOwner(): Promise<TestProcessOwnerArtifact> {
  ownerFixture = await buildTestProcessOwnerFixture()
  return ownerFixture.owner
}
