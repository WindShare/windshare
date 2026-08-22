import { fsaOwnedFileGuarantees } from '../../../transfer/intent'
import { DIRECT_ZIP_MAXIMUM_POSITIONED_FSA_OFFSET } from '../../../output/direct-zip/format'
import {
  lookupReviewedDirectZipSupportV1,
  type DirectZipRequiredFeatureFactsV1,
  type DirectZipSupportLookupV1,
} from '../../../output/direct-zip/session'
import type { EnvironmentTargetOfferInput } from '../../../output/planning'
import type { BrowserReceiveWindow } from '../contracts'
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
        support: Object.freeze({ kind: 'unavailable', reason: 'support-evidence-missing' }),
      }),
    })
  }
  const evidence = await directZip.evidence.read(signal)
  signal.throwIfAborted()
  const lookup = await lookupReviewedDirectZipSupportV1({
    artifact: evidence.artifact,
    runtime: Object.freeze({
      ...evidence.runtime,
      featureFacts: observeBrowserDirectZipFeatureFacts(windowPort),
    }),
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

export function observeBrowserDirectZipFeatureFacts(
  windowPort: BrowserReceiveWindow,
): DirectZipRequiredFeatureFactsV1 {
  const constructors = windowPort as BrowserReceiveWindow & Readonly<{
    FileSystemHandle?: { readonly prototype?: Record<string, unknown> }
    FileSystemFileHandle?: { readonly prototype?: Record<string, unknown> }
    FileSystemDirectoryHandle?: { readonly prototype?: Record<string, unknown> }
  }>
  const base = constructors.FileSystemHandle?.prototype
  const file = constructors.FileSystemFileHandle?.prototype
  const directory = constructors.FileSystemDirectoryHandle?.prototype
  return Object.freeze({
    createWritable: fact(file?.createWritable),
    handleIsSameEntry: fact(base?.isSameEntry ?? directory?.isSameEntry ?? file?.isSameEntry),
    handleQueryPermission: fact(base?.queryPermission ?? directory?.queryPermission),
    handleRequestPermission: fact(base?.requestPermission ?? directory?.requestPermission),
    indexedDB: typeof windowPort.indexedDB === 'object' ? 'object' : 'missing',
    isSecureContext: windowPort.isSecureContext === true,
    locks: typeof windowPort.navigator.locks === 'object' ? 'object' : 'missing',
    showDirectoryPicker: fact(windowPort.showDirectoryPicker),
  })
}

function fact(value: unknown): 'function' | 'missing' {
  return typeof value === 'function' ? 'function' : 'missing'
}
