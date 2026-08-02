import type { BrowserSampleContainmentBackend } from './containment.ts'
import { createTestProcessOwnerContainmentBackend } from './test-process-owner-backend.ts'
import type { TestProcessOwnerArtifact } from './test-process-owner-client.mjs'

export interface ProductionContainmentBackendOptions {
  readonly processOwner?: TestProcessOwnerArtifact
}

export function createProductionContainmentBackend(
  options: ProductionContainmentBackendOptions,
): BrowserSampleContainmentBackend {
  if (options.processOwner === undefined) {
    throw new Error('production samples require an authenticated test process owner artifact')
  }
  return createTestProcessOwnerContainmentBackend(options.processOwner)
}
