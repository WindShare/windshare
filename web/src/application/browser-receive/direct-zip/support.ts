import { observeBrowserDirectZipFeatureFacts } from '../../../output/direct-zip/browser/features'
import { fsaOwnedFileGuarantees } from '../../../transfer/intent'
import { DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET } from '../../../output/direct-zip/format'
import {
  admitDirectZipRuntimeV1,
  type DirectZipSupportLookupV1,
} from '../../../output/direct-zip/session'
import type { EnvironmentTargetOfferInput } from '../../../output/planning'
import type { BrowserReceiveWindow } from '../../../output/browser/environment'
import type { BrowserDirectZipCompositionPort } from './contracts'

export const BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID = 'browser-fsa-owned-direct-zip-v1'

export interface BrowserDirectZipEnvironmentContribution {
  readonly lookup: DirectZipSupportLookupV1
  readonly target?: EnvironmentTargetOfferInput
}

export async function inspectBrowserDirectZipEnvironment(
  windowPort: BrowserReceiveWindow,
  directZip: BrowserDirectZipCompositionPort | undefined,
  signal: AbortSignal,
): Promise<BrowserDirectZipEnvironmentContribution> {
  signal.throwIfAborted()
  if (directZip === undefined) {
    return Object.freeze({
      lookup: Object.freeze({
        kind: 'unavailable',
        support: Object.freeze({ kind: 'unavailable', reason: 'runtime-not-installed' }),
      }),
    })
  }
  const capabilities = await directZip.capabilities.read(signal)
  signal.throwIfAborted()
  const lookup = await admitDirectZipRuntimeV1({
    capabilities: Object.freeze({
      authority: capabilities.authority,
      featureFacts: observeBrowserDirectZipFeatureFacts(windowPort),
    }),
    ...(capabilities.policy === undefined ? {} : { policy: capabilities.policy }),
  })
  if (lookup.kind === 'unavailable') return Object.freeze({ lookup })
  const guarantees = fsaOwnedFileGuarantees()
  return Object.freeze({
    lookup,
    target: Object.freeze({
      routeId: BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID,
      kind: 'fsa-owned-file-target',
      guarantees: Object.freeze({
        nameAuthority: guarantees.nameAuthority,
        replacement: guarantees.replacement,
        delivery: guarantees.delivery,
        targetVisibility: guarantees.targetVisibility,
        artifactAvailability: guarantees.artifactAvailability,
        cleanupAuthority: guarantees.cleanupAuthority,
      }),
      persistence: 'operation-scoped',
      hardMaximumOutputBytes: DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET,
      support: lookup.facts.support,
    }),
  })
}
